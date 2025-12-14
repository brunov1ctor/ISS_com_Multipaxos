#!/usr/bin/env bash
set -euo pipefail

# deploy-remote.sh
# Pipeline remoto (Emulab) com logs fortes e falha "barulhenta".
# - Garante binários localmente
# - Gera master-commands.cmd
# - Reseta estado remoto
# - Sobe discoverymaster no master
# - Sobe slaves via start-remote-slaves.sh
# - Aguarda status final no master
# - Fetch de resultados (se existirem)

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

# shellcheck source=global-vars.sh
source "${SCRIPT_DIR}/global-vars.sh"

log() {
  echo "[INFO  ][$(date '+%Y-%m-%d %H:%M:%S')] $*"
}

warn() {
  echo "[WARN  ][$(date '+%Y-%m-%d %H:%M:%S')] $*" >&2
}

die() {
  echo "[ERROR ][$(date '+%Y-%m-%d %H:%M:%S')] $*" >&2
  exit 1
}

usage() {
  cat >&2 <<EOF
Uso:
  $0 <instance-info> <exp_data_dir>

EOF
  exit 1
}

if [[ $# -lt 2 ]]; then
  usage
fi

INSTANCE_INFO_FILE="$1"
EXP_DATA_DIR="$2"

[[ -f "$INSTANCE_INFO_FILE" ]] || die "instance-info não existe: $INSTANCE_INFO_FILE"
[[ -d "$EXP_DATA_DIR" ]] || die "exp_data_dir não existe: $EXP_DATA_DIR"

DEBUG_DIR="${EXP_DATA_DIR}/_debug"
mkdir -p "$DEBUG_DIR"

# ==================================================
# Parse instance-info: master = primeira linha com tag 'master'
# Formato esperado (exemplo):
#   node-0 172.20.6.3 10.10.1.1 master
#   node-1 172.20.4.1 10.10.1.2 peers
#   node-6 172.20.5.6 10.10.1.7 1client
# ==================================================

MASTER_IP=""
MASTER_LINE=""

while IFS= read -r line; do
  l="$(echo "$line" | sed 's/#.*$//' | xargs || true)"
  [[ -z "$l" ]] && continue
  tag="$(echo "$l" | awk '{print $4}')"
  if [[ "$tag" == "master" ]]; then
    MASTER_IP="$(echo "$l" | awk '{print $2}')"
    MASTER_LINE="$l"
    break
  fi
done < "$INSTANCE_INFO_FILE"

[[ -n "$MASTER_IP" ]] || die "Não encontrei master no instance-info: $INSTANCE_INFO_FILE"

log "Using instance info file: $INSTANCE_INFO_FILE"
log "Master IP address      : $MASTER_IP"
log "remote_user            : $remote_user"
log "remote_work_dir        : $remote_work_dir"
log "master_port            : $master_port"

# ==================================================
# 1) Gera master-commands.cmd e substitui placeholder MASTER_IP
# ==================================================

MASTER_CMD_TEMPLATE="${EXP_DATA_DIR}/master-commands-template.cmd"
MASTER_CMD_FILE="${EXP_DATA_DIR}/master-commands.cmd"

log "Gerando master commands via generate-master-commands.py"
log "  deployment_file (.dpl) = ${EXP_DATA_DIR}/${dpl_filename}"
log "  template out           = $MASTER_CMD_TEMPLATE"

python3 "${SCRIPT_DIR}/generate-master-commands.py" \
  "${EXP_DATA_DIR}/${dpl_filename}" \
  "$MASTER_CMD_TEMPLATE" \
  "$MASTER_CMD_FILE" \
  --master-port "${master_port}" \
  --remote-config-dir "${remote_config_dir}" \
  --remote-work-dir "${remote_work_dir}"

# Substitui placeholder no master-commands (MASTER_IP)
sed -i "s/___MASTER_IP___/${MASTER_IP}/g" "$MASTER_CMD_FILE"

[[ -s "$MASTER_CMD_FILE" ]] || die "master-commands.cmd não foi gerado (vazio): $MASTER_CMD_FILE"

log "Master command script escrito em: $MASTER_CMD_FILE"

# ==================================================
# 2) Reset remoto: processos e estado
# ==================================================

log "Limpando processos antigos e removendo traffic shaping nas máquinas remotas..."

# reset-proc: tenta matar processos antigos; se ssh falhar, continua (mas loga)
{
  ssh ${ssh_options} "${remote_user}@${MASTER_IP}" "pkill -9 -f discoverymaster 2>/dev/null || true; pkill -9 -f discoveryslave 2>/dev/null || true; pkill -9 -f orderingpeer 2>/dev/null || true; pkill -9 -f orderingclient 2>/dev/null || true; true" \
    2> "${DEBUG_DIR}/reset-proc-${MASTER_IP}.stderr" || {
      warn "[reset-proc] ${MASTER_IP}: ssh falhou (continuando). stderr em ${DEBUG_DIR}/reset-proc-${MASTER_IP}.stderr"
    }
}

log "Resetando state (remove arquivos antigos do experimento)..."
ssh ${ssh_options} "${remote_user}@${MASTER_IP}" "rm -rf ${remote_delete_files} 2>/dev/null || true; mkdir -p '${remote_work_dir}' '${remote_work_dir}/logs' '${remote_work_dir}/scripts' '${remote_work_dir}/config' '${remote_work_dir}/experiment-config' '${remote_exp_dir}'"

log "Estado das máquinas remotas resetado."

# ==================================================
# 3) Start master: copia master-commands + configs + sobe discoverymaster
# ==================================================

log "Starting master on ${MASTER_IP}"

"${SCRIPT_DIR}/start-master.sh" \
  "${remote_user}" \
  "${MASTER_IP}" \
  "${ssh_options}" \
  "${remote_work_dir}" \
  "${remote_bin_dir}" \
  "${EXP_DATA_DIR}" \
  "${master_port}" \
  "${MASTER_CMD_FILE}"

# ==================================================
# 4) Start slaves: peers e 1client
# ==================================================

log "Starting peer slaves (tag=peers, n=)"
"${SCRIPT_DIR}/start-remote-slaves.sh" \
  "${EXP_DATA_DIR}" \
  "${INSTANCE_INFO_FILE}" \
  "peers" \
  "${remote_user}" \
  "${remote_work_dir}" \
  "${remote_bin_dir}" \
  "${remote_exp_dir}" \
  "${local_bin_dir}" \
  "${ssh_options}"

log "Starting client slaves (tag=1client, n=)"
"${SCRIPT_DIR}/start-remote-slaves.sh" \
  "${EXP_DATA_DIR}" \
  "${INSTANCE_INFO_FILE}" \
  "1client" \
  "${remote_user}" \
  "${remote_work_dir}" \
  "${remote_bin_dir}" \
  "${remote_exp_dir}" \
  "${local_bin_dir}" \
  "${ssh_options}"

log "All slaves started."

# ==================================================
# 5) Espera master atualizar status (e.g. "0003")
# ==================================================

LAST_EXP="0003"
log "[WAIT] Aguardando master marcar status final (last_exp=${LAST_EXP})"

status=""
diag_printed="false"

while true; do
  status="$(ssh ${ssh_options} "${remote_user}@${MASTER_IP}" "cat '${remote_status_file}' 2>/dev/null || true" | tr -d '\r' | xargs || true)"

  if [[ -n "$status" ]]; then
    log "[WAIT] status atual='${status}' (aguardando '${LAST_EXP}')"
  else
    log "[WAIT] status atual='<vazio>' (aguardando '${LAST_EXP}'), dormindo 5s..."
    if [[ "$diag_printed" == "false" ]]; then
      warn "[WAIT] status vazio. Imprimindo diagnóstico rápido do MASTER (1x)..."
      diag_printed="true"
      ssh ${ssh_options} "${remote_user}@${MASTER_IP}" "echo '--- master diag: ls workdir ---'; ls -la '${remote_work_dir}' || true; echo '--- master diag: ls experiment-config ---'; ls -la '${remote_config_dir}' || true; echo '--- master diag: tail main_log.log ---'; tail -n 50 '${remote_main_log}' 2>/dev/null || true" || true
    fi
    sleep 5
    continue
  fi

  if [[ "$status" == "$LAST_EXP" ]]; then
    log "[WAIT] status final atingido: ${status}"
    break
  fi

  sleep 5
done

# ==================================================
# 6) Fetch resultados (se existirem)
# ==================================================

log "Fetching results..."
"${SCRIPT_DIR}/fetch-results.sh" "${INSTANCE_INFO_FILE}" "${EXP_DATA_DIR}" || warn "fetch-results.sh retornou erro (continuando)."

log "DONE."

