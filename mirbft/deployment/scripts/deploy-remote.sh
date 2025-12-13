#!/bin/bash

# scripts/deploy-remote.sh
#
# Remote deployment using instance-info (e.g., Emulab/cluster).
#
# Este script é *sourced* por deploy.sh, que já:
#   - sourced scripts/global-vars.sh
#   - run scripts/initialize-deployment.sh
#   - fez preflight de build (binários locais)
#
# Objetivos desta versão:
#   - garantir que master-commands.cmd existe e foi enviado
#   - garantir que os slaves foram disparados
#   - garantir execução ponta-a-ponta: esperar status final no master
#   - chamar fetch-results.sh e FALHAR se não houver outputs reais
#   - logs claros (sem "sucesso" falso)

set -euo pipefail
shopt -s nullglob

this_dir="$(cd "$(dirname "${BASH_SOURCE[0]}" )" && pwd)"
deployment_dir="$(cd "$this_dir/.." && pwd)"

ts() { date +"%Y-%m-%d %H:%M:%S"; }
info(){ echo "[INFO  ][$(ts)] $*"; }
warn(){ echo "[WARN  ][$(ts)] $*"; }
err(){  echo "[ERRO  ][$(ts)] $*" >&2; }

die(){ err "$*"; exit 1; }

############################################
# Helper: resolve instance-info path
############################################
resolve_instance_info() {
  local base_dir="$1"   # e.g. $exp_data_dir
  local info_arg="$2"   # e.g. scripts/instance-info

  # absolute
  if [[ "$info_arg" = /* ]] && [[ -f "$info_arg" ]]; then
    echo "$info_arg"; return 0
  fi

  # repo root + deployment dir
  local repo_dir
  repo_dir="$(cd "$deployment_dir/.." && pwd)"

  local cand1="$repo_dir/$info_arg"
  local cand2="$deployment_dir/$info_arg"

  [[ -f "$cand1" ]] && { echo "$cand1"; return 0; }
  [[ -f "$cand2" ]] && { echo "$cand2"; return 0; }
  [[ -f "$info_arg" ]] && { echo "$info_arg"; return 0; }

  return 1
}

############################################
# 1) Determine master IP from instance-info
############################################

deployment_file="$exp_data_dir/deployment.dpl"
[[ -f "$deployment_file" ]] || die "Deployment file not found: $deployment_file"

if ! instance_info_file="$(resolve_instance_info "$exp_data_dir" "$instance_info_file")"; then
  die "Could not find instance-info file: '$instance_info_file'"
fi

master_ip=""
while read -r instance_id ctrl_ip data_ip role itag; do
  [[ -z "${instance_id:-}" ]] && continue
  [[ "${instance_id}" =~ ^[[:space:]]*# ]] && continue
  if [[ "${role}" == "master" || "${instance_id}" == "master" || "${instance_id}" == "-1" ]]; then
    master_ip="$ctrl_ip"; break
  fi
done < "$instance_info_file"

[[ -n "$master_ip" ]] || die "Could not determine master IP from instance-info '$instance_info_file'"

info "Using instance info file: $instance_info_file"
info "Master IP address      : $master_ip"
echo

# --------------------------------------------------------------------
# 2) Generate master commands (template -> envsubst -> master-commands.cmd)
# --------------------------------------------------------------------

local_master_command_template="master-commands-template.cmd"
local_master_command_file="master-commands.cmd"

info "Gerando master commands via generate-master-commands.py"
info "  deployment_file (.dpl) = $deployment_file"
info "  template out           = $exp_data_dir/$local_master_command_template"

python3 scripts/generate-master-commands.py \
  remote \
  "$deployment_file" \
  "$exp_data_dir/$local_master_command_template" \
  "$exp_data_dir"

export ssh_key_file="$remote_private_key_file"
export own_public_ip="$master_ip"
export master_port
export status_file="$remote_status_file"

local_template_path="$exp_data_dir/$local_master_command_template"
local_cmd_path="$exp_data_dir/$local_master_command_file"

if [[ -f "$local_template_path" ]]; then
  envsubst '$ssh_key_file $own_public_ip $master_port $status_file' \
    < "$local_template_path" > "$local_cmd_path"
else
  die "Template não existe: $local_template_path"
fi

[[ -s "$local_cmd_path" ]] || die "master-commands.cmd foi gerado mas está vazio: $local_cmd_path"
info "Master command script escrito em: $local_cmd_path"
echo

# --------------------------------------------------------------------
# 3) Reset remoto (tolerante a erros, mas loga)
# --------------------------------------------------------------------

info "Limpando processos antigos e removendo traffic shaping nas máquinas remotas..."

while read -r instance_id ctrl_ip data_ip role itag; do
  [[ -z "${instance_id:-}" ]] && continue
  [[ "${instance_id}" =~ ^[[:space:]]*# ]] && continue

  info "[reset-proc] ${ctrl_ip}: matando processos antigos..."
  if ssh $ssh_options "${ctrl_ip}" "\
    pkill -f 'tail -F' 2>/dev/null || true; \
    pkill -f 'fetch-results.sh' 2>/dev/null || true; \
    pkill -f 'start-slave.sh' 2>/dev/null || true; \
    pkill -f discoverymaster 2>/dev/null || true; \
    pkill -f discoveryslave 2>/dev/null || true; \
    pkill -f orderingpeer 2>/dev/null || true; \
    pkill -f orderingclient 2>/dev/null || true; \
    pkill -f 'scp ' 2>/dev/null || true; \
    pkill -f rsync 2>/dev/null || true; \
    tc qdisc del dev eth0 root tbf rate 1gbit burst 320kbit latency 400ms 2>/dev/null || true; \
  "; then
    info "[reset-proc] ${ctrl_ip}: OK"
  else
    warn "[reset-proc] ${ctrl_ip}: ssh falhou (continuando)"
  fi

done < "$instance_info_file"

echo
info "Resetando state (remove arquivos antigos do experimento)..."

while read -r instance_id ctrl_ip data_ip role itag; do
  [[ -z "${instance_id:-}" ]] && continue
  [[ "${instance_id}" =~ ^[[:space:]]*# ]] && continue

  info "[reset-state] ${ctrl_ip}: limpando..."
  if ssh $ssh_options "${ctrl_ip}" "\
    tc qdisc del dev eth0 root tbf rate 1gbit burst 320kbit latency 400ms 2>/dev/null || true; \
    killall -9 discoverymaster discoveryslave orderingpeer orderingclient scp rsync 2>/dev/null || true; \
    rm -rf $remote_delete_files 2>/dev/null || true; \
  "; then
    info "[reset-state] ${ctrl_ip}: OK"
  else
    warn "[reset-state] ${ctrl_ip}: ssh falhou (continuando)"
  fi

done < "$instance_info_file"

echo
info "Estado das máquinas remotas resetado."
echo

# --------------------------------------------------------------------
# 4) Start master
# --------------------------------------------------------------------

info "Starting master on $master_ip"
scripts/start-master.sh "$exp_data_dir" "$master_ip"

echo

# --------------------------------------------------------------------
# 5) Start slaves
# --------------------------------------------------------------------

peers_tag="peers"
info "Starting peer slaves (tag=${peers_tag})"
scripts/start-remote-slaves.sh "$exp_data_dir" 5 "$peers_tag" "$instance_info_file"

echo
clients_tag="1client"
info "Starting client slaves (tag=${clients_tag})"
scripts/start-remote-slaves.sh "$exp_data_dir" 1 "$clients_tag" "$instance_info_file"

echo
info "All slaves started."

# --------------------------------------------------------------------
# 6) Esperar término REAL (status_file) + fetch + validação
# --------------------------------------------------------------------

csv_file="$exp_data_dir/deployment.csv"
[[ -f "$csv_file" ]] || die "deployment.csv não encontrado em $csv_file"

last_exp="$(awk -F',' 'NR>1{print $1}' "$csv_file" | tail -n 1)"
[[ -n "${last_exp}" ]] || die "não consegui detectar last_exp pelo deployment.csv"

info "[WAIT] Aguardando master marcar status final (last_exp=${last_exp})"
wait_timeout_sec="${DEPLOY_WAIT_TIMEOUT_SEC:-1800}"   # 30 min
poll_sec="${DEPLOY_WAIT_POLL_SEC:-5}"
start_ts="$(date +%s)"

while true; do
  now="$(date +%s)"
  if (( now - start_ts > wait_timeout_sec )); then
    err "Timeout aguardando status final no master."
    err "master=${master_ip} status_file=${remote_status_file} last_exp=${last_exp}"
    err "Dica: verifique no master: ${remote_work_dir}/main_log.log e ${remote_status_file}"
    exit 3
  fi

  status_val="$(ssh ${ssh_options} "${remote_user}@${master_ip}" "cat '${remote_status_file}' 2>/dev/null || true" </dev/null || true)"
  status_val="$(echo "${status_val}" | tr -d '\r\n' | tail -n 1)"

  if [[ "${status_val}" == "${last_exp}" ]]; then
    info "[WAIT] OK: status final atingido: ${status_val}"
    break
  fi

  info "[WAIT] status atual='${status_val:-<vazio>}' (aguardando '${last_exp}'), dormindo ${poll_sec}s..."
  sleep "${poll_sec}"
done

echo
info "[FETCH] Baixando resultados do master -> ${exp_data_dir}"

scripts/fetch-results.sh "${master_ip}" "${exp_data_dir}" | tee "${exp_data_dir}/${local_result_fetching_log}"

echo
info "[VERIFY] Validando existência de resultados reais"

if [[ ! -d "${exp_data_dir}/experiment-output" ]]; then
  die "fetch-results terminou mas experiment-output não existe: ${exp_data_dir}/experiment-output"
fi

cnt_dirs="$(find "${exp_data_dir}/experiment-output" -mindepth 2 -maxdepth 2 -type d 2>/dev/null | wc -l | tr -d ' ')"
if [[ "${cnt_dirs}" == "0" ]]; then
  err "experiment-output está vazio -> deploy inválido (sem métricas)."
  err "Veja ${exp_data_dir}/${local_result_fetching_log} e ${exp_data_dir}/_debug/master-diag.txt"
  exit 4
fi

info "[VERIFY] OK: experiment-output contém resultados (${cnt_dirs} dirs)."

echo
info "Remote deployment finished (com outputs reais)."
info "Se estiver usando cloud, não esqueça: scripts/cancel-cloud-instances.sh $exp_data_dir/cloud-instance-info"

