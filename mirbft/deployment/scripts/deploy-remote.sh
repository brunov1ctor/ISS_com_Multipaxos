#!/usr/bin/env bash
set -euo pipefail

# shellcheck source=/dev/null
source scripts/global-vars.sh

# --------------------------------------------------------------------
# Logging helpers
# --------------------------------------------------------------------
ts() { date +"%Y-%m-%d %H:%M:%S"; }
log_i(){ echo "[INFO  ][$(ts)] $*"; }
log_w(){ echo "[WARN  ][$(ts)] $*"; }
log_e(){ echo "[ERRO  ][$(ts)] $*" >&2; }

# --------------------------------------------------------------------
# Args
# --------------------------------------------------------------------
deployment_file="${1:-}"
instance_info_file="${2:-}"
config_script="${3:-}"

if [[ -z "${deployment_file}" || -z "${instance_info_file}" || -z "${config_script}" ]]; then
  log_e "Uso: scripts/deploy-remote.sh <deployment.dpl> <instance-info> <generate-config.sh>"
  exit 1
fi

# --------------------------------------------------------------------
# Defaults / env
# --------------------------------------------------------------------
cancel_instances="${cancel_instances:-false}"

# Published root (o deploy.sh espera isso)
published_root="/users/${USER}/iss/experiment-output"
remote_work_dir="${remote_work_dir:-/users/${remote_user}/iss}"
remote_status_file="${remote_status_file:-${remote_work_dir}/status}"

# --------------------------------------------------------------------
# Derived local paths
# --------------------------------------------------------------------
exp_data_dir="$(dirname "${deployment_file}")"
local_result_fetching_log="${local_result_fetching_log:-result-fetching.log}"
instance_info_file_name="$(basename "${instance_info_file}")"

log_i "deploy-remote.sh"
log_i "deployment_file=${deployment_file}"
log_i "instance_info_file=${instance_info_file}"
log_i "config_script=${config_script}"
log_i "exp_data_dir=${exp_data_dir}"
log_i "published_root=${published_root}"
log_i "remote_work_dir=${remote_work_dir}"
log_i "remote_status_file=${remote_status_file}"
echo

# --------------------------------------------------------------------
# Read master IP from instance-info
# Expected format (example):
#   node-0 172.20.5.5 10.10.1.1 master ...
# --------------------------------------------------------------------
master_ip="$(
  awk 'NF>=4 && $4=="master" {print $2; exit}' "${instance_info_file}" 2>/dev/null || true
)"
if [[ -z "${master_ip}" ]]; then
  master_ip="${MASTER_IP:-}"
fi
if [[ -z "${master_ip}" ]]; then
  log_e "Não consegui inferir master_ip (instance-info ou MASTER_IP)."
  exit 2
fi

log_i "master_ip=${master_ip}"
echo

# --------------------------------------------------------------------
# 1) Reset / prepare remote state
# --------------------------------------------------------------------
log_i "Resetando estado remoto e preparando diretórios..."
scripts/reset-remote-state.sh "${instance_info_file}" || log_w "reset-remote-state falhou (continue)."

# Marca status como RUNNING no master (best effort)
ssh ${ssh_options} "${remote_user}@${master_ip}" "mkdir -p '${remote_work_dir}' && echo RUNNING > '${remote_status_file}'" </dev/null || true

# --------------------------------------------------------------------
# 2) Generate configs (local)
# --------------------------------------------------------------------
log_i "Gerando configs localmente..."
"${config_script}" "${deployment_file}" "${instance_info_file}" || {
  log_e "Falha gerando configs."
  exit 3
}

# --------------------------------------------------------------------
# 3) Generate master-commands (local)
# --------------------------------------------------------------------
log_i "Gerando master-commands.cmd..."
scripts/generate-master-commands.sh "${deployment_file}" "${instance_info_file}" > "${exp_data_dir}/master-commands.cmd"

# --------------------------------------------------------------------
# 4) Deploy scripts/configs para o master
# --------------------------------------------------------------------
log_i "Deploy de scripts/configs para o master..."
scripts/push-to-master.sh "${master_ip}" "${exp_data_dir}" "${instance_info_file}" || {
  log_e "Falha enviando dados para o master."
  exit 4
}

# --------------------------------------------------------------------
# 5) Start master (assíncrono)
# --------------------------------------------------------------------
log_i "Iniciando master..."
scripts/start-master.sh "${master_ip}" "${exp_data_dir}/master-commands.cmd" || {
  log_e "Falha iniciando master."
  exit 5
}

# --------------------------------------------------------------------
# 6) Start slaves (assíncrono / paralelo)
# --------------------------------------------------------------------
log_i "Iniciando slaves..."
scripts/start-remote-slaves.sh "${instance_info_file}" || {
  log_e "Falha iniciando slaves."
  exit 6
}

log_i "Todos os slaves disparados."

# =====================================================================
# 7b) Aguardar master finalizar execução
#
# O start-master dispara o processo do master de forma assíncrona. Se o fetch
# acontecer imediatamente, é comum coletar apenas outputs iniciais (ex.: peer.log)
# antes do fechamento/flush de arquivos (peer.trc, profiles, etc.).
#
# Critério elegante: esperar o master escrever `DONE` no arquivo de status.
# Esse write acontece no fim do master-commands.cmd.
# =====================================================================

wait_for_master_done() {
  local ip="$1"
  local status_file="$2"
  local timeout_s="${3:-1800}"   # 30min default
  local sleep_s="${4:-2}"

  local start_ts now_ts elapsed
  start_ts="$(date +%s)"

  log_i "Aguardando master finalizar (status==DONE) em ${ip}:${status_file} (timeout ${timeout_s}s)..."

  while true; do
    # Lê status (best effort). Se não existir ainda, retorna vazio.
    local st
    st="$(ssh ${ssh_options} "${remote_user}@${ip}" "cat '${status_file}' 2>/dev/null || true" </dev/null | tr -d '\r' || true)"

    if [[ "${st}" == "DONE" ]]; then
      log_i "Master finalizou (status=DONE)."
      return 0
    fi

    now_ts="$(date +%s)"
    elapsed="$((now_ts - start_ts))"
    if (( elapsed >= timeout_s )); then
      log_w "Timeout aguardando status=DONE (último status: '${st:-<vazio>}')."
      return 1
    fi

    # Log leve a cada ~20s
    if (( elapsed % 20 == 0 )); then
      log_i "Ainda aguardando... status='${st:-<vazio>}' elapsed=${elapsed}s"
    fi

    sleep "${sleep_s}"
  done
}

if ! wait_for_master_done "${master_ip}" "${remote_status_file}" "${WAIT_MASTER_DONE_TIMEOUT_S:-1800}" "${WAIT_MASTER_DONE_SLEEP_S:-2}"; then
  log_w "Prosseguindo para fetch mesmo sem status=DONE (pode coletar parcial)."
fi

echo

# =====================================================================
# 8) Fetch de resultados
# =====================================================================

log_i "Fetching de resultados..."
set +e
scripts/fetch-results.sh "${master_ip}" "${exp_data_dir}" >"${exp_data_dir}/${local_result_fetching_log}" 2>&1
fetch_rc=$?
set -e

if (( fetch_rc != 0 )); then
  log_w "fetch-results.sh falhou (rc=${fetch_rc}). Veja: $exp_data_dir/$local_result_fetching_log"
  exit $fetch_rc
fi

log_i "fetch-results.sh OK. Log: $exp_data_dir/$local_result_fetching_log"

# =====================================================================
# 8b) Publicar (cópia) para /users/<user>/iss/experiment-output
#     (mantém canônico no exp_data_dir e só sincroniza para o publish)
# =====================================================================

log_i "Publicando (cópia) resultados em: ${published_root}"
mkdir -p "${published_root}"

# Copia o conteúdo de exp_data_dir/experiment-output/* para published_root/
# (sem depender de symlink; sem mudar o verify do deploy.sh)
rsync -rtz --delete \
  "${exp_data_dir}/experiment-output/" \
  "${published_root}/"

log_i "Publicação OK: ${published_root}"

# =====================================================================
# 9) Cancelar instâncias (se configurado)
# =====================================================================

if $cancel_instances; then
  log_i "Encerrando instâncias (cancel_instances=true)..."
  scripts/cancel-cloud-instances.sh "$exp_data_dir/$instance_info_file_name"
else
  echo -e "Lembre-se de encerrar as VMs com:\n  cancel-cloud-instances.sh $exp_data_dir/$instance_info_file_name\n"
fi

log_i "deploy-remote.sh finalizado."
exit 0

