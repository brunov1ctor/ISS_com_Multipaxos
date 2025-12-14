#!/usr/bin/env bash
set -euo pipefail

ts(){ date '+%Y-%m-%d %H:%M:%S'; }
log(){ echo "[start-master][$(ts)] $*"; }

# Esperado do ambiente / global-vars.sh
: "${remote_user:?}"
: "${master_ip:?}"
: "${ssh_options:?}"
: "${remote_work_dir:?}"     # ex: /users/Bruno/iss
: "${remote_bin_dir:?}"      # ex: /users/Bruno/go/bin
: "${exp_data_dir:?}"        # local exp data dir
: "${DISCOVERY_PORT:=9999}"  # default

local_master_cmd="${exp_data_dir}/master-commands.cmd"
remote_master_cmd="${remote_work_dir}/master-commands.cmd"
remote_log="${remote_work_dir}/main_log.log"
remote_pid="${remote_work_dir}/.discoverymaster.pid"

log "Master IP: ${master_ip}"
log "Remote work dir: ${remote_work_dir}"
log "Remote bin dir: ${remote_bin_dir}"
log "Discovery port: ${DISCOVERY_PORT}"
log "Local master-commands: ${local_master_cmd}"
log "Remote master-commands: ${remote_master_cmd}"

if [[ ! -f "${local_master_cmd}" ]]; then
  echo "ERRO: local master-commands.cmd não existe em: ${local_master_cmd}" >&2
  exit 2
fi

# Copia o master-commands pro master (se você já faz isso fora, pode remover)
log "Copiando master-commands.cmd para o master..."
scp ${ssh_options} "${local_master_cmd}" "${remote_user}@${master_ip}:${remote_master_cmd}" >/dev/null

log "Subindo discoverymaster em MASTER mode (file-based commands)..."
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

cd \"\$REMOTE_WORK_DIR\"

log \"PWD=\$(pwd) HOST=\$(hostname) WHOAMI=\$(whoami)\"
log \"Bin: \$BIN_DIR/discoverymaster\"
log \"Cmd file: \$CMD_FILE\"
log \"Port: \$PORT\"

# valida binário
if [[ ! -x \"\$BIN_DIR/discoverymaster\" ]]; then
  log \"ERRO: discoverymaster não é executável em \$BIN_DIR/discoverymaster\"
  ls -la \"\$BIN_DIR\" | head -n 120 || true
  exit 10
fi

# valida arquivo de comandos
if [[ ! -f \"\$CMD_FILE\" ]]; then
  log \"ERRO: master-commands.cmd não existe no master: \$CMD_FILE\"
  ls -la \"\$REMOTE_WORK_DIR\" | head -n 120 || true
  exit 11
fi

log \"Head do master-commands.cmd:\"
head -n 60 \"\$CMD_FILE\" || true

# mata instância anterior (se houver)
pkill -9 -f \"\$BIN_DIR/discoverymaster\" 2>/dev/null || true

# checa porta ocupada (sem depender de ss/lsof estarem presentes)
log \"Checando se a porta \$PORT já está em uso...\"
( command -v ss >/dev/null 2>&1 && ss -ltnp | grep -E \":\$PORT\\b\" ) || true
( command -v netstat >/dev/null 2>&1 && netstat -ltnp 2>/dev/null | grep -E \":\$PORT\\b\" ) || true

# IMPORTANTe:
# MASTER mode (file-based) = mantém o server ativo enquanto consome CMD_FILE e aguarda slaves.
# Sintaxe (conforme usage do binário):
#   discoverymaster master addr:port master-commands.cmd
#
# addr:port aqui deve ser o endpoint do próprio master.
MASTER_ADDR=\"\$MASTER_IP:\$PORT\"

log \"Iniciando: nohup discoverymaster master \$MASTER_ADDR \$CMD_FILE\"
nohup \"\$BIN_DIR/discoverymaster\" master \"\$MASTER_ADDR\" \"\$CMD_FILE\" > \"\$LOG_FILE\" 2>&1 < /dev/null &
echo \$! > \"\$PID_FILE\"

sleep 1

if ! kill -0 \$(cat \"\$PID_FILE\") 2>/dev/null; then
  log \"ERRO: discoverymaster morreu ao iniciar.\"
  log \"Tail do log:\"
  tail -n 200 \"\$LOG_FILE\" || true
  exit 12
fi

log \"OK: discoverymaster pid=\$(cat \"\$PID_FILE\")\"
log \"Estado da porta:\"
( command -v ss >/dev/null 2>&1 && ss -ltnp | grep -E \":\$PORT\\b\" ) || true
'"

log "Discoverymaster iniciado. Verificando rapidamente o log..."
ssh ${ssh_options} "${remote_user}@${master_ip}" "tail -n 60 '${remote_log}' || true"
log "Done."

