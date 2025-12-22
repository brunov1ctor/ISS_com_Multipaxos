#!/usr/bin/env bash
# deploy-remote.sh (reprodutível + fail-fast)

set -euo pipefail

ts() { date +"%Y-%m-%d %H:%M:%S%z"; }
log_i() { echo "[INFO  ][$(ts)] $*"; }
log_w() { echo "[WARN  ][$(ts)] $*" >&2; }
log_e() { echo "[ERRO  ][$(ts)] $*" >&2; }

: "${cancel_instances:=false}"

# =====================================================================
# 1) Variáveis esperadas do ambiente (de deploy.sh + global-vars.sh)
# =====================================================================

if [[ -z "${exp_data_dir:-}" || -z "${instance_info_file:-}" ]]; then
  log_e "exp_data_dir ou instance_info_file não definidos."
  log_e "exp_data_dir='${exp_data_dir:-}' instance_info_file='${instance_info_file:-}'"
  exit 1
fi

instance_info_file_name="$(basename "$instance_info_file")"

local_master_command_template_file="${local_master_command_template_file:-master-commands-template.cmd}"
local_master_command_file="${local_master_command_file:-master-commands.cmd}"
local_result_fetching_log="${local_result_fetching_log:-result-fetching.log}"

remote_user="${remote_user:-${USER}}"
remote_work_dir="${remote_work_dir:-/users/${remote_user}/iss}"
remote_bin_dir="${remote_bin_dir:-/users/${remote_user}/go/bin}"

ssh_options="${ssh_options:--o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -T -o BatchMode=yes -o ConnectTimeout=8 -o ConnectionAttempts=1 -o ServerAliveInterval=5 -o ServerAliveCountMax=2 -o LogLevel=ERROR -o ControlMaster=no -o ControlPath=none -o ControlPersist=no}"

remote_status_file="${remote_status_file:-${remote_work_dir}/status}"

# =====================================================================
# 2) Descobrir IP do master e copiar instance-info para o diretório do experimento
# =====================================================================

master_ip="$(awk '$4 == "master" {print $2; exit}' "$instance_info_file")"
if [[ -z "$master_ip" ]]; then
  log_e "Não foi possível obter o IP do master a partir de: $instance_info_file"
  exit 1
fi

cp "$instance_info_file" "$exp_data_dir/$instance_info_file_name"

log_i "instance-info: $instance_info_file"
log_i "Master IP: $master_ip"

# =====================================================================
# 2b) TLS: gerar auth.pem com SAN = IPs públicos (col 2) + privados (col 3)
# =====================================================================

log_i "Garantindo certificado TLS (auth.pem) com SAN dos IPs PUBLICOS+PRIVADOS do cluster..."

tls_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/tls-data"
if [[ ! -d "${tls_dir}" ]]; then
  log_e "Diretório tls-data não encontrado: ${tls_dir}"
  exit 1
fi

# Col 2 (ctrl/public) + Col 3 (data/private), únicos
mapfile -t all_ips < <(
  awk '{print $2"\n"$3}' "$instance_info_file" \
  | grep -Eo '([0-9]{1,3}\.){3}[0-9]{1,3}' \
  | sort -u
)

if [[ "${#all_ips[@]}" -eq 0 ]]; then
  log_e "Não foi possível extrair IPs (coluna 2/3) do instance-info: $instance_info_file"
  exit 1
fi

(
  cd "${tls_dir}"
  [[ -x "./generate-auth.sh" ]] || { log_e "generate-auth.sh não executável em ${tls_dir}"; exit 1; }

  ./generate-auth.sh "${all_ips[@]}"

  # Fail-fast: garante que pelo menos 1 IP público e 1 IP privado estão no SAN (se existirem)
  san="$(openssl x509 -in auth.pem -noout -ext subjectAltName || true)"

  # pega um exemplo de ip público e privado (se houver)
  pub_ip="$(awk '{print $2}' "$instance_info_file" | grep -Eo '([0-9]{1,3}\.){3}[0-9]{1,3}' | head -n1 || true)"
  priv_ip="$(awk '{print $3}' "$instance_info_file" | grep -Eo '([0-9]{1,3}\.){3}[0-9]{1,3}' | head -n1 || true)"

  if [[ -n "$pub_ip" ]] && ! grep -q "$pub_ip" <<<"$san"; then
    log_e "SAN NÃO contém IP público esperado ($pub_ip). subjectAltName:"
    echo "$san" >&2
    exit 1
  fi
  if [[ -n "$priv_ip" ]] && ! grep -q "$priv_ip" <<<"$san"; then
    log_e "SAN NÃO contém IP privado esperado ($priv_ip). subjectAltName:"
    echo "$san" >&2
    exit 1
  fi
)

log_i "TLS OK: SAN inclui IPs públicos+privados do cluster."

# =====================================================================
# 3) Garantir master-commands-template.cmd
# =====================================================================

template_path="$exp_data_dir/$local_master_command_template_file"
deployment_file="$exp_data_dir/deployment.dpl"

if [[ ! -f "$template_path" ]]; then
  log_i "Gerando master-commands-template.cmd..."
  log_i "deployment.dpl: $deployment_file"
  log_i "template:      $template_path"

  [[ -f "$deployment_file" ]] || { log_e "Deployment file não encontrado: $deployment_file"; exit 1; }

  python3 scripts/generate-master-commands.py remote "$deployment_file" "$template_path" "$exp_data_dir" || {
    log_e "Falha ao gerar master-commands-template.cmd"
    exit 1
  }
else
  log_i "Usando master-commands-template existente: $template_path"
fi

# =====================================================================
# 4) Gerar master-commands.cmd final com envsubst
# =====================================================================

export ssh_key_file="$remote_private_key_file"
export own_public_ip="$master_ip"
export master_port
export status_file="$remote_status_file"

log_i "Gerando master-commands.cmd a partir do template..."
envsubst '$ssh_key_file $own_public_ip $master_port $status_file' \
  < "$template_path" > "$exp_data_dir/$local_master_command_file"

echo -e "\nwrite-file $status_file DONE" >> "$exp_data_dir/$local_master_command_file"

log_i "master-commands.cmd pronto: $exp_data_dir/$local_master_command_file"

# =====================================================================
# 5) Reset remoto
# =====================================================================

log_i "Limpando processos antigos e estado remoto (incluindo SSH/bandwidth)."

for ip in $(awk '{print $2}' "$instance_info_file"); do
  ssh $ssh_options "${remote_user}@${ip}" "bash -s" >/dev/null 2>&1 <<'EOF_KILL_ANALYZE' || true
pids=$(ps -ef | grep 'analyze-continuously' | grep -v grep | awk '{print $2}')
if [ -n "$pids" ]; then kill -9 $pids 2>/dev/null || true; fi
EOF_KILL_ANALYZE
  sleep 0.05
done

log_i "Verificação de analyze-continuously concluída."

for ip in $(awk '{print $2}' "$instance_info_file"); do
  ssh $ssh_options "${remote_user}@${ip}" "bash -s" >/dev/null 2>&1 <<EOF_RESET || true
tc qdisc del dev eth0 root tbf rate 1gbit burst 320kbit latency 400ms 2>/dev/null || true
killall -9 discoverymaster discoveryslave orderingpeer orderingclient scp rsync 2>/dev/null || true
rm -rf '${remote_work_dir}'
mkdir -p '${remote_work_dir}'
echo RUNNING > '${remote_work_dir}/status'
kill -9 \$(ps -ef | grep 'sshd: ${remote_user}@notty' | grep -v grep | awk '{print \$2}') 2>/dev/null || true
EOF_RESET
  sleep 0.05
done
wait

log_i "Reset remoto concluído."

# =====================================================================
# 5c) NÃO fazer symlink na mão: garantir paths esperados em TODOS os nós
# =====================================================================

log_i "Garantindo path compatível para fetch-results em TODOS os nós (experiment-output -> current-deployment-data/experiment-output)..."

for ip in $(awk '{print $2}' "$instance_info_file"); do
  ssh $ssh_options "${remote_user}@${ip}" "bash -s" >/dev/null 2>&1 <<EOF_FIX_PATHS
set -e
mkdir -p '${remote_work_dir}/current-deployment-data'
ln -sfn '${remote_work_dir}/current-deployment-data/experiment-output' '${remote_work_dir}/experiment-output'
mkdir -p '${remote_work_dir}/current-deployment-data/raw-results'
EOF_FIX_PATHS || {
    log_e "Falha ao preparar paths em $ip"
    exit 1
  }
done

log_i "Paths OK em todos os nós."

# =====================================================================
# 6) Start master (SÍNCRONO + validações)
# =====================================================================

log_i "Iniciando master em $master_ip (síncrono, fail-fast)..."
scripts/start-master.sh \
  "$remote_user" \
  "$master_ip" \
  "$remote_work_dir" \
  "$remote_bin_dir" \
  "$exp_data_dir" \
  "$exp_data_dir/$local_master_command_file"

log_i "Master iniciou. Validando TLS no master..."

ssh $ssh_options "${remote_user}@${master_ip}" \
  "test -f '${remote_work_dir}/tls-data/ca.pem' -a -f '${remote_work_dir}/tls-data/auth.pem' -a -f '${remote_work_dir}/tls-data/auth.key'" \
  >/dev/null 2>&1 || {
    log_e "TLS não está presente no master em ${remote_work_dir}/tls-data."
    ssh $ssh_options "${remote_user}@${master_ip}" "ls -la '${remote_work_dir}/tls-data' || true" || true
    exit 1
}

log_i "TLS presente no master."

DISC_PORT="${master_port:-9999}"
log_i "Validando se discoverymaster está escutando em ${master_ip}:${DISC_PORT}..."
ssh $ssh_options "${remote_user}@${master_ip}" "ss -lntp | grep \":${DISC_PORT} \"" >/dev/null 2>&1 || {
  log_e "discoverymaster não está escutando na porta ${DISC_PORT} no master."
  ssh $ssh_options "${remote_user}@${master_ip}" "tail -n 120 '${remote_work_dir}/logs/discoverymaster.log' || true" || true
  exit 1
}
log_i "discoverymaster OK na porta ${DISC_PORT}."

# =====================================================================
# 7) Start slaves (peers + 1client)
# =====================================================================

log_i "Iniciando slaves com tag=peers..."
scripts/start-remote-slaves.sh "$exp_data_dir" 0 peers "$instance_info_file"

log_i "Iniciando slaves com tag=1client..."
scripts/start-remote-slaves.sh "$exp_data_dir" 0 1client "$instance_info_file"

log_i "Todos os slaves foram iniciados."

# =====================================================================
# 8) Fetch de resultados
# =====================================================================

log_i "Iniciando coleta de resultados..."
set +e
scripts/fetch-results.sh "$master_ip" "$exp_data_dir" > "$exp_data_dir/$local_result_fetching_log" 2>&1
fetch_rc=$?
set -e

if [[ $fetch_rc -ne 0 ]]; then
  log_e "fetch-results.sh falhou (rc=${fetch_rc}). Veja: $exp_data_dir/$local_result_fetching_log"
  exit $fetch_rc
fi

log_i "fetch-results.sh concluído com sucesso."
log_i "Acompanhe em: $exp_data_dir/$local_result_fetching_log"

# =====================================================================
# 9) Cancelar instâncias (se configurado)
# =====================================================================

if $cancel_instances; then
  log_i "Encerrando máquinas na nuvem (cancel_instances=true)..."
  scripts/cancel-cloud-instances.sh "$exp_data_dir/$instance_info_file_name"
else
  echo -e "Lembre-se de encerrar as VMs com:\n  cancel-cloud-instances.sh $exp_data_dir/$instance_info_file_name\n"
fi

log_i "deploy-remote.sh finalizado."

