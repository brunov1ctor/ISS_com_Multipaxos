#!/usr/bin/env bash
# deploy-remote.sh
# Robustez + semântica clara de término:
# - espera por DONE/ANALYZED no status_file
# - ou EXIT rc=0 no status_exit_file (wrapper)
# - ou, opcionalmente, last_exp_id (se seu fluxo escrever isso no status)
#
# Não usa "pgrep discoverymaster" como condição de término (evita falso negativo).

set -euo pipefail

ts() { date +"%Y-%m-%d %H:%M:%S%z"; }
log_i() { echo "[INFO  ][$(ts)] $*"; }
log_w() { echo "[WARN  ][$(ts)] $*" >&2; }
log_e() { echo "[ERRO  ][$(ts)] $*" >&2; }

# =====================================================================
# 1) Variáveis esperadas do ambiente (deploy.sh + global-vars.sh)
# =====================================================================
if [[ -z "${exp_data_dir:-}" || -z "${instance_info_file:-}" ]]; then
  log_e "exp_data_dir ou instance_info_file não definidos."
  log_e "exp_data_dir='${exp_data_dir:-}' instance_info_file='${instance_info_file:-}'"
  exit 1
fi

if command -v realpath >/dev/null 2>&1; then
  instance_info_file="$(realpath "${instance_info_file}")"
  exp_data_dir="$(realpath "${exp_data_dir}")"
fi

instance_info_file_name="$(basename "$instance_info_file")"

local_master_command_template_file="${local_master_command_template_file:-master-commands-template.cmd}"
local_master_command_file="${local_master_command_file:-master-commands.cmd}"
local_result_fetching_log="${local_result_fetching_log:-result-fetching.log}"

remote_user="${remote_user:-${USER}}"
remote_work_dir="${remote_work_dir:-/users/${remote_user}/iss}"
remote_bin_dir="${remote_bin_dir:-/users/${remote_user}/go/bin}"

ssh_options="${ssh_options:--o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -T -o BatchMode=yes -o ConnectTimeout=8 -o ConnectionAttempts=1 -o ServerAliveInterval=5 -o ServerAliveCountMax=2 -o LogLevel=ERROR -o ControlMaster=no -o ControlPath=none -o ControlPersist=no}"

# status “lógico” do experimento
remote_status_file="${remote_status_file:-${remote_work_dir}/status}"
# status “do processo” (wrapper do start-master)
remote_status_exit_file="${remote_status_exit_file:-${remote_status_file}.exit}"

DISC_PORT="${master_port:-${MASTER_PORT:-9999}}"

remote_exp_dir="${remote_exp_dir:-${remote_work_dir}}"
remote_experiment_output_dir="${remote_experiment_output_dir:-${remote_work_dir}/experiment-output}"
published_root="${published_root:-${PUBLISHED_ROOT:-${remote_work_dir}/experiment-output}}"

rsh() { ssh $ssh_options "${remote_user}@${1}" "${2}"; }

# =====================================================================
# 2) Descobrir IP do master
# =====================================================================
master_ip="$(awk 'NF>=4 && $4=="master"{print $2; exit}' "$instance_info_file" || true)"
if [[ -z "${master_ip}" ]]; then
  log_e "Não foi possível obter o IP do master a partir de: $instance_info_file"
  exit 1
fi

cp -f "$instance_info_file" "$exp_data_dir/$instance_info_file_name"

log_i "instance-info: $instance_info_file"
log_i "Master IP: $master_ip"
log_i "Remote exp dir: ${remote_exp_dir}"
log_i "Remote experiment-output dir: ${remote_experiment_output_dir}"
log_i "Published root (local): ${published_root}"
log_i "Discovery port: ${DISC_PORT}"
log_i "Remote status_file: ${remote_status_file}"
log_i "Remote status_exit_file: ${remote_status_exit_file}"

# =====================================================================
# 2b) TLS: gerar auth.pem com SAN dos IPs públicos+privados
# =====================================================================
log_i "Gerando certificado TLS (auth.pem) com SAN dos IPs públicos+privados do cluster..."

tls_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/tls-data"
if [[ ! -d "${tls_dir}" ]]; then
  log_e "Diretório tls-data não encontrado: ${tls_dir}"
  exit 1
fi

mapfile -t all_ips < <(
  awk '{print $2"\n"$3}' "$instance_info_file" \
  | grep -Eo '([0-9]{1,3}\.){3}[0-9]{1,3}' \
  | sort -u
)

if [[ "${#all_ips[@]}" -eq 0 ]]; then
  log_e "Não foi possível extrair IPs (col 2/3) do instance-info: $instance_info_file"
  exit 1
fi

(
  cd "${tls_dir}"
  [[ -x "./generate-auth.sh" ]] || { log_e "generate-auth.sh não executável em ${tls_dir}"; exit 1; }

  ./generate-auth.sh "${all_ips[@]}"

  san="$(openssl x509 -in auth.pem -noout -ext subjectAltName || true)"
  for ip in "${all_ips[@]}"; do
    echo "$san" | grep -q "$ip" || { log_e "SAN não contém IP esperado: $ip"; echo "$san"; exit 1; }
  done
)

log_i "TLS OK: SAN contém IPs públicos e privados do cluster."

# =====================================================================
# 3) Garantir master-commands-template.cmd
# =====================================================================
template_path="$exp_data_dir/$local_master_command_template_file"
deployment_file="$exp_data_dir/deployment.dpl"

if [[ ! -f "$template_path" ]]; then
  log_i "Gerando master-commands-template.cmd..."
  [[ -f "$deployment_file" ]] || { log_e "Deployment file não encontrado: $deployment_file"; exit 1; }
  python3 scripts/generate-master-commands.py remote "$deployment_file" "$template_path" "$exp_data_dir" \
    || { log_e "Falha ao gerar master-commands-template.cmd"; exit 1; }
else
  log_i "Usando master-commands-template existente: $template_path"
fi

# =====================================================================
# 4) Gerar master-commands.cmd (envsubst) + DONE terminal
# =====================================================================
export ssh_key_file="${remote_private_key_file:-}"
export own_public_ip="$master_ip"
export master_port="${DISC_PORT}"
export status_file="$remote_status_file"
export status_exit_file="$remote_status_exit_file"
export ready_file="${remote_ready_file:-${remote_work_dir}/master-ready}"

log_i "Gerando master-commands.cmd a partir do template..."
envsubst '$ssh_key_file $own_public_ip $master_port $status_file $ready_file' \
  < "$template_path" > "$exp_data_dir/$local_master_command_file"

# DONE terminal (o discoverymaster executa isso no final do script)
echo -e "\nwrite-file $status_file DONE" >> "$exp_data_dir/$local_master_command_file"
log_i "master-commands.cmd pronto: $exp_data_dir/$local_master_command_file"

# =====================================================================
# 5) Reset remoto
# =====================================================================
log_i "Reset remoto: limpando ${remote_work_dir} e recriando layout canônico."

for ip in $(awk '{print $2}' "$instance_info_file"); do
  ssh $ssh_options "${remote_user}@${ip}" "bash -s" >/dev/null 2>&1 <<EOF_RESET || true
tc qdisc del dev eth0 root tbf rate 1gbit burst 320kbit latency 400ms 2>/dev/null || true
killall -9 discoverymaster discoveryslave orderingpeer orderingclient scp rsync 2>/dev/null || true
rm -rf '${remote_work_dir}'
mkdir -p '${remote_work_dir}' \
         '${remote_work_dir}/logs' \
         '${remote_work_dir}/scripts' \
         '${remote_work_dir}/tls-data' \
         '${remote_work_dir}/experiment-output' \
         '${remote_work_dir}/raw-results'
# status "lógico"
echo RUNNING > '${remote_status_file}' 2>/dev/null || true
# status "do processo"
: > '${remote_status_exit_file}' 2>/dev/null || true
EOF_RESET
  sleep 0.1
done
wait

log_i "Reset remoto concluído."

# =====================================================================
# 6) Start master
# =====================================================================
log_i "Iniciando master em $master_ip..."
scripts/start-master.sh \
  "$remote_user" \
  "$master_ip" \
  "$remote_work_dir" \
  "$remote_bin_dir" \
  "$exp_data_dir" \
  "$exp_data_dir/$local_master_command_file"

log_i "Validando TLS no master..."
rsh "$master_ip" "test -f '${remote_work_dir}/tls-data/ca.pem' -a -f '${remote_work_dir}/tls-data/auth.pem' -a -f '${remote_work_dir}/tls-data/auth.key'" \
  || { log_e "TLS não presente no master."; rsh "$master_ip" "ls -la '${remote_work_dir}/tls-data' || true" || true; exit 1; }

log_i "Validando discoverymaster na porta ${DISC_PORT}..."
rsh "$master_ip" "ss -lntp | grep \":${DISC_PORT} \"" >/dev/null \
  || { log_e "discoverymaster não escutando ${DISC_PORT}."; rsh "$master_ip" "tail -n 200 '${remote_work_dir}/logs/discoverymaster.log' || true" || true; exit 1; }
log_i "discoverymaster OK."

# =====================================================================
# 7) Start slaves
# =====================================================================
log_i "Iniciando slaves peers..."
scripts/start-remote-slaves.sh "$exp_data_dir" 0 peers "$instance_info_file"

log_i "Iniciando slaves 1client..."
scripts/start-remote-slaves.sh "$exp_data_dir" 0 1client "$instance_info_file"

log_i "Todos os slaves disparados."

# =====================================================================
# 7b) Esperar fim do experimento (semântica clara)
# =====================================================================
last_exp_id=""
if ls "$exp_data_dir/experiment-config/config-"*.yml >/dev/null 2>&1; then
  last_exp_id="$(ls "$exp_data_dir/experiment-config/config-"*.yml | sed -E 's/.*config-([0-9]+).*/\1/' | sort -n | tail -n 1)"
fi

master_done_timeout_secs="${MASTER_DONE_TIMEOUT_SECS:-7200}"

if [[ -n "$last_exp_id" ]]; then
  log_i "Aguardando término no master (status DONE/ANALYZED, ou EXIT rc=0 em status.exit, ou exp_id=${last_exp_id}; timeout=${master_done_timeout_secs}s)..."
else
  log_i "Aguardando término no master (status DONE/ANALYZED, ou EXIT rc=0 em status.exit; timeout=${master_done_timeout_secs}s)..."
fi

done_ok=false
last_seen_status=""
last_seen_exit=""

for ((i=0; i<master_done_timeout_secs; i++)); do
  cur_status="$(rsh "$master_ip" "test -f '${remote_status_file}' && cat '${remote_status_file}' || true" 2>/dev/null | tr -d '\r' | tail -n 1)"
  cur_exit="$(rsh "$master_ip" "test -f '${remote_status_exit_file}' && cat '${remote_status_exit_file}' || true" 2>/dev/null | tr -d '\r' | tail -n 1)"

  if [[ "$cur_status" != "$last_seen_status" || "$cur_exit" != "$last_seen_exit" ]]; then
    log_i "status(master)='${cur_status}' exit='${cur_exit}'"
    last_seen_status="$cur_status"
    last_seen_exit="$cur_exit"
  elif (( i % 30 == 0 )); then
    log_i "status(master)='${cur_status}' exit='${cur_exit}' (aguardando...)"
  fi

  # 1) Condições principais
  if [[ "$cur_status" == "DONE" || "$cur_status" == "ANALYZED" ]]; then
    done_ok=true
    break
  fi

  # 2) Se o wrapper finalizou limpo
  if [[ "$cur_exit" == "EXIT rc=0" ]]; then
    done_ok=true
    break
  fi

  # 3) Opcional: alguns fluxos usam o exp_id final como status
  if [[ -n "$last_exp_id" && "$cur_status" == "$last_exp_id" ]]; then
    done_ok=true
    break
  fi

  sleep 1
done

if $done_ok; then
  log_i "Término detectado."
  log_i "Status final (tail):"
  rsh "$master_ip" "tail -n 20 '${remote_status_file}' 2>/dev/null || true" || true
  log_i "Exit status (tail):"
  rsh "$master_ip" "tail -n 20 '${remote_status_exit_file}' 2>/dev/null || true" || true
else
  log_w "Timeout atingido; tentando fetch mesmo assim (pode estar incompleto)."
  log_w "Status no master:"
  rsh "$master_ip" "tail -n 50 '${remote_status_file}' 2>/dev/null || true" || true
  log_w "Exit status no master:"
  rsh "$master_ip" "tail -n 50 '${remote_status_exit_file}' 2>/dev/null || true" || true
fi

# =====================================================================
# 8) Fetch de resultados
# =====================================================================
log_i "Coletando resultados para exp_data_dir=${exp_data_dir} ..."
export REMOTE_WORK_DIR="${remote_work_dir}"
export REMOTE_EXP_DIR="${remote_exp_dir}"
export REMOTE_EXPERIMENT_OUTPUT_DIR="${remote_experiment_output_dir}"

set +e
scripts/fetch-results.sh "$master_ip" "$exp_data_dir" > "$exp_data_dir/$local_result_fetching_log" 2>&1
fetch_rc=$?
set -e

if [[ $fetch_rc -ne 0 ]]; then
  log_e "fetch-results.sh falhou (rc=${fetch_rc}). Log: $exp_data_dir/$local_result_fetching_log"
  exit $fetch_rc
fi

log_i "fetch-results.sh OK. Log: $exp_data_dir/$local_result_fetching_log"

# =====================================================================
# 8b) Publicar
# =====================================================================
log_i "Publicando (cópia) resultados em: ${published_root}"
mkdir -p "${published_root}"

rsync -rtz --delete \
  "${exp_data_dir}/experiment-output/" \
  "${published_root}/"

log_i "Publicação OK: ${published_root}"

log_i "deploy-remote.sh finalizado."
exit 0

