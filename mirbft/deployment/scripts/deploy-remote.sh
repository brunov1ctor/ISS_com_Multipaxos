#!/usr/bin/env bash
set -euo pipefail

ts() { date +"%Y-%m-%d %H:%M:%S%z"; }
log_i() { echo "[INFO  ][$(ts)] $*"; }
log_w() { echo "[WARN  ][$(ts)] $*" >&2; }
log_e() { echo "[ERRO  ][$(ts)] $*" >&2; }

cancel_instances="${cancel_instances:-false}"

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

remote_work_dir="${remote_work_dir:-/tmp/iss-${remote_user}}"
remote_exp_dir="${remote_exp_dir:-${remote_work_dir}}"

# Diretório leve (configs/TLS) permanece em /users/<user>/iss.
remote_base_dir="${remote_base_dir:-/users/${remote_user}/iss}"

remote_bin_dir="${remote_bin_dir:-${remote_work_dir}/bin}"

ssh_options="${ssh_options:--o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -T -o BatchMode=yes -o ConnectTimeout=8 -o ConnectionAttempts=1 -o ServerAliveInterval=5 -o ServerAliveCountMax=2 -o LogLevel=ERROR -o ControlMaster=no -o ControlPath=none -o ControlPersist=no}"
scp_options="${scp_options:--o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o BatchMode=yes -o ConnectTimeout=8 -o ConnectionAttempts=1 -o ServerAliveInterval=5 -o ServerAliveCountMax=2 -o LogLevel=ERROR -o ControlMaster=no -o ControlPath=none -o ControlPersist=no}"

remote_status_file="${remote_status_file:-${remote_work_dir}/status}"
DISC_PORT="${master_port:-${MASTER_PORT:-9999}}"

remote_experiment_output_dir="${remote_experiment_output_dir:-${remote_work_dir}/experiment-output}"
published_root="${published_root:-${PUBLISHED_ROOT:-${exp_data_dir}/published/experiment-output}}"

rsh() { ssh $ssh_options "${remote_user}@${1}" "${2}"; }

master_ip="$(awk 'NF>=4 && $4=="master"{print $2; exit}' "$instance_info_file" || true)"
if [[ -z "${master_ip}" ]]; then
  log_e "Não foi possível obter o IP do master a partir de: $instance_info_file"
  exit 1
fi

cp -f "$instance_info_file" "$exp_data_dir/$instance_info_file_name"

log_i "instance-info: $instance_info_file"
log_i "Master IP: $master_ip"
log_i "Remote exp dir: ${remote_exp_dir}"
log_i "Remote base dir (configs/TLS): ${remote_base_dir}"
log_i "Remote experiment-output dir: ${remote_experiment_output_dir}"
log_i "Published root (local): ${published_root}"
log_i "Discovery port: ${DISC_PORT}"

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

template_path="$exp_data_dir/$local_master_command_template_file"
deployment_file="$exp_data_dir/deployment.dpl"

if [[ ! -f "$template_path" ]]; then
  log_i "Gerando master-commands-template.cmd..."
  [[ -f "$deployment_file" ]] || { log_e "Deployment file não encontrado: $deployment_file"; exit 1; }

  # >>> FIX: informa pro gerador o user e o bin_dir remoto
  # Isso permite ele gerar comandos com caminho absoluto:
  #   /users/<user>/go/bin/orderingpeer e /users/<user>/go/bin/orderingclient
  ISS_REMOTE_USER="${remote_user}" \
  ISS_REMOTE_BIN_DIR="${remote_bin_dir}" \
  python3 scripts/generate-master-commands.py remote "$deployment_file" "$template_path" "$exp_data_dir" \
    || { log_e "Falha ao gerar master-commands-template.cmd"; exit 1; }
else
  log_i "Usando master-commands-template existente: $template_path"
fi

export ssh_key_file="${remote_private_key_file:-}"
export own_public_ip="$master_ip"
export master_port="${DISC_PORT}"
export status_file="$remote_status_file"
export ready_file="${remote_ready_file:-${remote_work_dir}/master-ready}"

log_i "Gerando master-commands.cmd a partir do template..."
envsubst '$ssh_key_file $own_public_ip $master_port $status_file $ready_file' \
  < "$template_path" > "$exp_data_dir/$local_master_command_file"

# RESTAURADO (igual ao original): marca finalização do master-commands
echo -e "\nwrite-file $status_file DONE" >> "$exp_data_dir/$local_master_command_file"

log_i "master-commands.cmd pronto: $exp_data_dir/$local_master_command_file"

log_i "Validando estrutura de master-commands.cmd (linhas exec-start)..."
python3 - "$exp_data_dir/$local_master_command_file" <<'PY'
import shlex
import sys

path = sys.argv[1]
bad = False

with open(path, 'r', encoding='utf-8', errors='replace') as f:
    for ln, line in enumerate(f, 1):
        s = line.strip()
        if not s or s.startswith('#'):
            continue
        try:
            toks = shlex.split(s, posix=True)
        except Exception as e:
            print(f"[ERRO  ] Linha {ln}: não consegui parsear (shlex): {e}\n  {s}", file=sys.stderr)
            bad = True
            continue

        if not toks:
            continue

        if toks[0] == 'exec-start':
            # Formato mínimo válido:
            # exec-start <tag> <outFile> <cmd> [args...]
            if len(toks) < 4:
                print(f"[ERRO  ] Linha {ln}: exec-start com poucos campos. Esperado: exec-start <tag> <outFile> <cmd> [args...]\n  {s}", file=sys.stderr)
                bad = True
            else:
                # outFile não pode ser vazio (pode ser "-" para descartar)
                if toks[2] == '':
                    print(f"[ERRO  ] Linha {ln}: outFile vazio em exec-start\n  {s}", file=sys.stderr)
                    bad = True

if bad:
    print("[ERRO  ] master-commands.cmd inválido; abortando deploy.", file=sys.stderr)
    sys.exit(2)
print("[INFO  ] master-commands.cmd OK.")
PY

log_i "Reset remoto: limpando ${remote_work_dir} e recriando layout canônico."
for ip in $(awk '{print $2}' "$instance_info_file"); do
  ssh $ssh_options "${remote_user}@${ip}" "bash -s" >/dev/null 2>&1 <<EOF_RESET || true
tc qdisc del dev eth0 root tbf rate 1gbit burst 320kbit latency 400ms 2>/dev/null || true
killall -9 discoverymaster discoveryslave orderingpeer orderingclient 2>/dev/null || true
rm -rf '${remote_work_dir}'

# pesado: /tmp (logs, status, experiment-output, binários)
mkdir -p '${remote_work_dir}' \
         '${remote_work_dir}/logs' \
         '${remote_work_dir}/scripts' \
         '${remote_work_dir}/bin' \
         '${remote_work_dir}/experiment-output' \
         '${remote_work_dir}/raw-results'

# leve: /users (configs/TLS)
mkdir -p '${remote_base_dir}' \
         '${remote_base_dir}/experiment-config' \
         '${remote_base_dir}/config' \
         '${remote_base_dir}/tls-data'
echo RUNNING > '${remote_status_file}' 2>/dev/null || true
EOF_RESET
  sleep 0.1
done
wait
log_i "Reset remoto concluído."

# === DIFUSÃO ATÔMICA SELETIVA: DEPLOY DE groups.yml ===
# Copia groups.yml para /tmp (onde o código procura)
log_i "Copiando groups.yml para ${remote_work_dir}/config em todos os nós..."
groups_yml_src="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../config" && pwd)/groups.yml"
if [[ -f "${groups_yml_src}" ]]; then
  for ip in $(awk '{print $2}' "$instance_info_file"); do
    (
      ssh $ssh_options "${remote_user}@${ip}" "mkdir -p ${remote_work_dir}/config" && \
      scp $scp_options "${groups_yml_src}" "${remote_user}@${ip}:${remote_work_dir}/config/groups.yml"
    ) &
  done
  wait
  log_i "groups.yml copiado para todos os nós."
else
  log_w "groups.yml não encontrado em ${groups_yml_src}, rodando sem grupos (broadcast mode)."
fi

# === COPIA instance-info PARA MASTER (IDs determinísticos) ===
log_i "Copiando instance-info para ${remote_work_dir}/config no master..."
scp $scp_options "${instance_info_file}" "${remote_user}@${master_ip}:${remote_work_dir}/config/instance-info"
log_i "instance-info copiado para o master."

# === COPIA BINÁRIOS PARA DISCO LOCAL (evita SIGBUS em NFS) ===
log_i "Copiando binários para ${remote_bin_dir} em todos os nós..."
local_bin_dir="${GOBIN:-${HOME}/go/bin}"
for bin in discoverymaster discoveryslave orderingpeer orderingclient; do
  if [[ ! -x "${local_bin_dir}/${bin}" ]]; then
    log_e "Binário ${bin} não encontrado em ${local_bin_dir}"
    exit 1
  fi
done

for ip in $(awk '{print $2}' "$instance_info_file"); do
  (
    for bin in discoverymaster discoveryslave orderingpeer orderingclient; do
      scp $scp_options "${local_bin_dir}/${bin}" "${remote_user}@${ip}:${remote_bin_dir}/${bin}.tmp" \
        && ssh $ssh_options "${remote_user}@${ip}" "mv -f '${remote_bin_dir}/${bin}.tmp' '${remote_bin_dir}/${bin}' && chmod +x '${remote_bin_dir}/${bin}'" \
        || { log_e "Falha ao copiar ${bin} para ${ip}"; exit 1; }
    done
  ) &
done
wait
log_i "Binários copiados para todos os nós."

log_i "Iniciando master em $master_ip..."
scripts/start-master.sh \
  "$remote_user" \
  "$master_ip" \
  "$remote_work_dir" \
  "$remote_bin_dir" \
  "$exp_data_dir" \
  "$exp_data_dir/$local_master_command_file"

log_i "Validando discoverymaster na porta ${DISC_PORT}..."
rsh "$master_ip" "ss -lntp | grep \":${DISC_PORT} \"" >/dev/null \
  || { log_e "discoverymaster não escutando ${DISC_PORT}."; rsh "$master_ip" "tail -n 200 '${remote_work_dir}/logs/discoverymaster.log' || true" || true; exit 1; }
log_i "discoverymaster OK."

log_i "Iniciando slaves peers..."
scripts/start-remote-slaves.sh "$exp_data_dir" 0 peers "$instance_info_file"

log_i "Iniciando slaves 1client..."
scripts/start-remote-slaves.sh "$exp_data_dir" 0 1client "$instance_info_file"

log_i "Todos os slaves disparados."

# -----------------------------------------------------------------------------
# MINIMA CORREÇÃO: esperar o master finalizar (status == DONE) antes do fetch
# -----------------------------------------------------------------------------
log_i "Aguardando o master finalizar (status: ${remote_status_file})..."
timeout_s="${WAIT_DONE_TIMEOUT_S:-1200}"   # 20min (override via env se quiser)
sleep_s="${WAIT_DONE_POLL_S:-3}"
start_epoch="$(date +%s)"

while true; do
  status="$(ssh $ssh_options "${remote_user}@${master_ip}" "cat '${remote_status_file}' 2>/dev/null || true" </dev/null | tr -d '\r\n')"
  [[ "${status}" == "DONE" ]] && break

  now_epoch="$(date +%s)"
  if (( now_epoch - start_epoch > timeout_s )); then
    log_w "Timeout esperando DONE (>${timeout_s}s). Continuando para fetch mesmo assim."
    break
  fi
  sleep "$sleep_s"
done

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

log_i "Publicando (cópia) resultados em: ${published_root}"
mkdir -p "${published_root}"
rsync -rtz \
  "${exp_data_dir}/experiment-output/" \
  "${published_root}/"
log_i "Publicação OK: ${published_root}"

# -----------------------------------------------------------------------------
# análise automática local (traces -> *.val -> result-summary.csv)
# -----------------------------------------------------------------------------
log_i "Auto-análise: gerando métricas a partir dos traces (*.trc) ..."

ANALYZE_QUERIES=("queries/aggregates.sql" "queries/histograms.sql")

for exp_dir in "${published_root}"/[0-9][0-9][0-9][0-9]; do
  [[ -d "$exp_dir" ]] || continue
  exp_id="$(basename "$exp_dir")"

  # opcional: não sumarizar experimento quebrado
  if ls "$exp_dir"/slave-*/FAILED >/dev/null 2>&1; then
    log_w "Auto-análise: pulando exp ${exp_id} (há FAILED)."
    continue
  fi

  log_i "Auto-análise: analisando exp ${exp_id} em ${exp_dir}"
  scripts/analyze/analyze.sh -d "$exp_dir" \
    -q "${ANALYZE_QUERIES[0]}" \
    -q "${ANALYZE_QUERIES[1]}" || log_w "Auto-análise: falhou no exp ${exp_id} (continuando)."
done

if [[ -f "$exp_data_dir/deployment.csv" ]]; then
  out_csv="$exp_data_dir/result-summary.csv"
  scripts/analyze/summarize.sh "$exp_data_dir/deployment.csv" "${published_root}" > "$out_csv" || true
  log_i "Auto-análise: CSV consolidado gerado em: $out_csv"
else
  log_w "Auto-análise: deployment.csv não encontrado em $exp_data_dir (pulando summarize)."
fi
# -----------------------------------------------------------------------------

if $cancel_instances; then
  log_i "Encerrando instâncias (cancel_instances=true)..."
  scripts/cancel-cloud-instances.sh "$exp_data_dir/$instance_info_file_name"
else
  echo -e "Lembre-se de encerrar as VMs com:\n  cancel-cloud-instances.sh $exp_data_dir/$instance_info_file_name\n"
fi

log_i "deploy-remote.sh finalizado."
exit 0

