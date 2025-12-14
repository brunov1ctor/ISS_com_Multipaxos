#!/usr/bin/env bash
set -euo pipefail

ts(){ date +"%Y-%m-%d %H:%M:%S"; }
log(){ echo "[start-master][$(ts)] $*"; }
warn(){ echo "[start-master][$(ts)] WARN: $*" >&2; }

# Uso:
#   start-master.sh <exp_data_dir> <master_ip>
#
# Requer vars (vindas do deploy.sh/global-vars.sh):
#   remote_user, ssh_options, remote_work_dir, remote_bin_dir
#   DISCOVERY_PORT (ou master_port), remote_status_file, remote_ready_file

exp_data_dir="${1:?missing exp_data_dir}"
master_ip="${2:?missing master_ip}"

remote_user="${remote_user:-${USER}}"
ssh_options="${ssh_options:--o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null}"
remote_work_dir="${remote_work_dir:-/users/${remote_user}/iss}"
remote_bin_dir="${remote_bin_dir:-/users/${remote_user}/go/bin}"
DISCOVERY_PORT="${DISCOVERY_PORT:-${master_port:-9999}}"

local_master_cmd="${exp_data_dir}/master-commands.cmd"
remote_master_cmd="${remote_work_dir}/master-commands.cmd"
remote_log="${remote_work_dir}/main_log.log"
remote_pid="${remote_work_dir}/.discoverymaster.pid"

remote_status_file="${remote_status_file:-${remote_work_dir}/status}"
remote_ready_file="${remote_ready_file:-${remote_work_dir}/master-ready}"

log "remote_user=${remote_user}"
log "master_ip=${master_ip}"
log "ssh_options=${ssh_options}"
log "remote_work_dir=${remote_work_dir}"
log "remote_bin_dir=${remote_bin_dir}"
log "exp_data_dir=${exp_data_dir}"
log "DISCOVERY_PORT=${DISCOVERY_PORT}"
log "local_master_cmd=${local_master_cmd}"

if [[ ! -s "${local_master_cmd}" ]]; then
  echo "[start-master][$(ts)] ERROR: local master-commands.cmd missing/empty: ${local_master_cmd}" >&2
  exit 2
fi

log "Ensuring remote workdir exists..."
ssh ${ssh_options} "${remote_user}@${master_ip}" "mkdir -p '${remote_work_dir}'"

log "Copying master-commands.cmd to remote..."
scp ${ssh_options} "${local_master_cmd}" "${remote_user}@${master_ip}:${remote_master_cmd}"

# -------------------------------------------------------------------
# Kill antigo (ROBUSTO): não deixa falha de ssh/pkill derrubar o script
# -------------------------------------------------------------------
log "Killing previous discoverymaster (if any)..."
set +e
ssh_rc=0
ssh ${ssh_options} "${remote_user}@${master_ip}" "
  pkill -9 -f '${remote_bin_dir}/discoverymaster' 2>/dev/null || true
  pkill -9 -f 'discoverymaster ' 2>/dev/null || true
  sleep 0.2
  pgrep -af discoverymaster 2>/dev/null || true
" </dev/null
ssh_rc=$?
set -e
if [[ $ssh_rc -ne 0 ]]; then
  warn "Kill step returned rc=${ssh_rc} (continuando)"
else
  log "Kill step OK"
fi

# -------------------------------------------------------------------
# Start MASTER mode (file-based commands) - não depende de stdin
# -------------------------------------------------------------------
log "Starting discoverymaster in MASTER mode (file-based commands)..."

# Observação:
#   discoverymaster master :PORT /path/master-commands.cmd
#   (Se teu binário exigir "addr:port" diferente, ajuste só a linha abaixo.)
set +e
ssh ${ssh_options} "${remote_user}@${master_ip}" "
  set -euo pipefail
  cd '${remote_work_dir}'

  # limpa arquivos antigos
  rm -f '${remote_pid}' '${remote_log}' '${remote_status_file}' '${remote_ready_file}' 2>/dev/null || true

  # sobe
  nohup '${remote_bin_dir}/discoverymaster' master :${DISCOVERY_PORT} '${remote_master_cmd}' \
    > '${remote_log}' 2>&1 < /dev/null &

  echo \$! > '${remote_pid}'
  sleep 1

  echo 'PID='\"\$(cat '${remote_pid}' 2>/dev/null || echo '?')\"
  ps -p \"\$(cat '${remote_pid}' 2>/dev/null || echo 0)\" -o pid,cmd --no-headers 2>/dev/null || echo 'NOT RUNNING'
  tail -n 60 '${remote_log}' 2>/dev/null || true
" </dev/null
start_rc=$?
set -e

if [[ $start_rc -ne 0 ]]; then
  warn "Start step returned rc=${start_rc}. Vou coletar diagnóstico do master e falhar."
  ssh ${ssh_options} "${remote_user}@${master_ip}" "
    echo '--- PID FILE ---'
    ls -la '${remote_pid}' 2>/dev/null || true
    echo '--- LOG ---'
    tail -n 200 '${remote_log}' 2>/dev/null || true
    echo '--- LISTEN ---'
    (ss -lntp 2>/dev/null || netstat -lntp 2>/dev/null || true) | egrep -n '(:${DISCOVERY_PORT}\b|discoverymaster)' || true
  " </dev/null || true
  exit 3
fi

# Confirma que subiu e está escutando
log "Verificando se o master está vivo e escutando na porta ${DISCOVERY_PORT}..."
ssh ${ssh_options} "${remote_user}@${master_ip}" "
  set -euo pipefail
  pid=\$(cat '${remote_pid}' 2>/dev/null || echo '')
  if [[ -z \"\$pid\" ]]; then
    echo 'PID missing'
    exit 10
  fi
  if ! kill -0 \"\$pid\" 2>/dev/null; then
    echo 'master process is not running'
    tail -n 200 '${remote_log}' 2>/dev/null || true
    exit 11
  fi
  # porta (best-effort)
  (ss -lntp 2>/dev/null || netstat -lntp 2>/dev/null || true) | egrep '(:${DISCOVERY_PORT}\b|discoverymaster)' >/dev/null || true
  echo 'OK'
" </dev/null

log "Master started successfully."

