#!/usr/bin/env bash
set -euo pipefail

ts(){ date '+%Y-%m-%d %H:%M:%S'; }
log(){ echo "[start-master][$(ts)] $*"; }

# ------------------------------------------------------------
# Fallbacks robustos (não assume que deploy.sh exportou tudo)
# ------------------------------------------------------------

# user remoto: tenta usar $remote_user, senão quem está rodando o deploy
remote_user="${remote_user:-$(whoami)}"

# ssh options padrão (não falha se não vier do deploy.sh)
ssh_options="${ssh_options:--o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null}"

# master_ip: tenta usar $master_ip; se não existir, tenta usar 1o argumento; senão falha com log.
master_ip="${master_ip:-${1:-}}"
if [[ -z "${master_ip}" ]]; then
  echo "[start-master][$(ts)] ERRO: master_ip não definido (nem env master_ip, nem argumento \$1)." >&2
  echo "[start-master][$(ts)] Dica: o deploy.sh precisa chamar: start-master.sh <master_ip> ..." >&2
  exit 2
fi

# diretórios: usa defaults compatíveis com seu setup atual
remote_work_dir="${remote_work_dir:-/users/${remote_user}/iss}"
remote_bin_dir="${remote_bin_dir:-/users/${remote_user}/go/bin}"

# exp_data_dir precisa existir localmente (o deploy.sh costuma passar/exportar; aqui tentamos inferir)
exp_data_dir="${exp_data_dir:-${EXP_DATA_DIR:-}}"
if [[ -z "${exp_data_dir}" ]]; then
  # tenta inferir pelo CWD típico: .../deployment e deployment-data/remote-0000
  # você pode ajustar aqui se seu layout mudar.
  if [[ -d "./deployment-data/remote-0000" ]]; then
    exp_data_dir="$(pwd)/deployment-data/remote-0000"
  elif [[ -d "./deployment/deployment-data/remote-0000" ]]; then
    exp_data_dir="$(pwd)/deployment/deployment-data/remote-0000"
  fi
fi

if [[ -z "${exp_data_dir}" || ! -d "${exp_data_dir}" ]]; then
  echo "[start-master][$(ts)] ERRO: exp_data_dir não definido ou não existe: '${exp_data_dir}'" >&2
  echo "[start-master][$(ts)] Defina exp_data_dir/EXP_DATA_DIR ou rode a partir do diretório correto." >&2
  exit 3
fi

DISCOVERY_PORT="${DISCOVERY_PORT:-9999}"

local_master_cmd="${exp_data_dir}/master-commands.cmd"
remote_master_cmd="${remote_work_dir}/master-commands.cmd"
remote_log="${remote_work_dir}/main_log.log"
remote_pid="${remote_work_dir}/.discoverymaster.pid"

log "remote_user=${remote_user}"
log "master_ip=${master_ip}"
log "ssh_options=${ssh_options}"
log "remote_work_dir=${remote_work_dir}"
log "remote_bin_dir=${remote_bin_dir}"
log "exp_data_dir=${exp_data_dir}"
log "DISCOVERY_PORT=${DISCOVERY_PORT}"
log "local_master_cmd=${local_master_cmd}"

if [[ ! -f "${local_master_cmd}" ]]; then
  echo "[start-master][$(ts)] ERRO: master-commands.cmd local não existe: ${local_master_cmd}" >&2
  ls -la "${exp_data_dir}" | head -n 120 >&2 || true
  exit 4
fi

# ------------------------------------------------------------
# Copia master-commands.cmd para o master
# ------------------------------------------------------------
log "Copiando master-commands.cmd para ${remote_user}@${master_ip}:${remote_master_cmd}"
scp ${ssh_options} "${local_master_cmd}" "${remote_user}@${master_ip}:${remote_master_cmd}" >/dev/null

# ------------------------------------------------------------
# Start discoverymaster no MASTER mode (file-based)
# ------------------------------------------------------------
log "Iniciando discoverymaster em MASTER mode no master remoto..."

ssh ${ssh_options} "${remote_user}@${master_ip}" bash -lc "'
set -euo pipefail
ts(){ date \"+%Y-%m-%d %H:%M:%S\"; }
log(){ echo \"[MASTER-REMOTE][\$(ts)] \$*\"; }

REMOTE_WORK_DIR=\"${remote_work_dir}\"
BIN_DIR=\"${remote_bin_dir}\"
PORT=\"${DISCOVERY_PORT}\"
MASTER_IP=\"${master_ip}\"
CMD_FILE=\"${remote_master_cmd}\"
LOG_FILE=\"${remote_log}\"
PID_FILE=\"${remote_pid}\"

mkdir -p \"\$REMOTE_WORK_DIR\"
cd \"\$REMOTE_WORK_DIR\"

log \"PWD=\$(pwd) HOST=\$(hostname) WHOAMI=\$(whoami)\"
log \"BIN=\$BIN_DIR/discoverymaster\"
log \"CMD_FILE=\$CMD_FILE\"
log \"PORT=\$PORT\"

if [[ ! -x \"\$BIN_DIR/discoverymaster\" ]]; then
  log \"ERRO: discoverymaster não é executável em \$BIN_DIR/discoverymaster\"
  ls -la \"\$BIN_DIR\" | head -n 120 || true
  exit 10
fi

if [[ ! -f \"\$CMD_FILE\" ]]; then
  log \"ERRO: master-commands.cmd não existe: \$CMD_FILE\"
  ls -la \"\$REMOTE_WORK_DIR\" | head -n 120 || true
  exit 11
fi

log \"Head do CMD_FILE:\"
head -n 40 \"\$CMD_FILE\" || true

# mata instância anterior
pkill -9 -f \"\$BIN_DIR/discoverymaster\" 2>/dev/null || true

# checa porta
log \"Checando porta \$PORT...\"
( command -v ss >/dev/null 2>&1 && ss -ltnp | grep -E \":\$PORT\\b\" ) || true
( command -v netstat >/dev/null 2>&1 && netstat -ltnp 2>/dev/null | grep -E \":\$PORT\\b\" ) || true

MASTER_ADDR=\"\$MASTER_IP:\$PORT\"
log \"CMD: nohup discoverymaster master \$MASTER_ADDR \$CMD_FILE\"

nohup \"\$BIN_DIR/discoverymaster\" master \"\$MASTER_ADDR\" \"\$CMD_FILE\" > \"\$LOG_FILE\" 2>&1 < /dev/null &
echo \$! > \"\$PID_FILE\"

sleep 1
if ! kill -0 \$(cat \"\$PID_FILE\") 2>/dev/null; then
  log \"ERRO: discoverymaster morreu ao iniciar\"
  tail -n 200 \"\$LOG_FILE\" || true
  exit 12
fi

log \"OK: pid=\$(cat \"\$PID_FILE\")\"
'"

log "Tail inicial do log remoto:"
ssh ${ssh_options} "${remote_user}@${master_ip}" "tail -n 80 '${remote_log}' || true"
log "start-master concluído."

