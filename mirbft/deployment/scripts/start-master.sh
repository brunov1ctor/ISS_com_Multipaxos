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

exp_data_dir="${1:?exp_data_dir required}"
master_ip="${2:?master_ip required}"

remote_user="${remote_user:-${REMOTE_USER:-${USER}}}"
ssh_options="${ssh_options:--o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null}"
remote_work_dir="${remote_work_dir:-/users/${remote_user}/iss}"
remote_bin_dir="${remote_bin_dir:-/users/${remote_user}/go/bin}"

DISCOVERY_PORT="${DISCOVERY_PORT:-${master_port:-9999}}"

remote_status_file="${remote_status_file:-${remote_work_dir}/status}"
remote_ready_file="${remote_ready_file:-${remote_work_dir}/master-ready}"

local_master_cmd="${exp_data_dir}/master-commands.cmd"
if [[ ! -f "$local_master_cmd" ]]; then
  echo "master-commands.cmd não encontrado: $local_master_cmd" >&2
  exit 2
fi

log "remote_user=${remote_user}"
log "master_ip=${master_ip}"
log "ssh_options=${ssh_options}"
log "remote_work_dir=${remote_work_dir}"
log "remote_bin_dir=${remote_bin_dir}"
log "exp_data_dir=${exp_data_dir}"
log "DISCOVERY_PORT=${DISCOVERY_PORT}"
log "local_master_cmd=${local_master_cmd}"

log "Ensuring remote workdir exists..."
ssh ${ssh_options} "${remote_user}@${master_ip}" "\
  mkdir -p '${remote_work_dir}' \
           '${remote_work_dir}/logs' \
           '${remote_work_dir}/scripts' \
           '${remote_work_dir}/experiment-config' \
           '${remote_work_dir}/current-deployment-data' \
" </dev/null

log "Copying master-commands.cmd to remote..."
scp ${ssh_options} "${local_master_cmd}" "${remote_user}@${master_ip}:${remote_work_dir}/master-commands.cmd" >/dev/null

# Copiar configs gerados localmente (exp_data_dir/config/*) para o master, em:
#   ${remote_work_dir}/experiment-config/
# Isso é necessário porque o master-commands manda os slaves fazerem SCP
# a partir de "experiment-config/config-XXXX.yml" no master.
if [[ -d "${exp_data_dir}/config" ]]; then
  log "Copying generated configs to master (experiment-config/)..."
  tar -C "${exp_data_dir}/config" -cf - . \
    | ssh ${ssh_options} "${remote_user}@${master_ip}" "\
        mkdir -p '${remote_work_dir}/experiment-config' && \
        tar -C '${remote_work_dir}/experiment-config' -xf - \
      " </dev/null
else
  warn "Diretório local de configs não encontrado: ${exp_data_dir}/config (isso pode quebrar o scp dos slaves)."
fi

log "Killing previous discoverymaster (if any)..."
set +e
ssh ${ssh_options} "${remote_user}@${master_ip}" "\
  pkill -9 -f '${remote_bin_dir}/discoverymaster' 2>/dev/null || true
  pkill -9 -f 'discoverymaster ' 2>/dev/null || true
  sleep 0.2
  pgrep -af discoverymaster 2>/dev/null || true
" </dev/null
rc=$?
set -e
if [[ $rc -ne 0 ]]; then
  warn "Kill step returned rc=$rc (continuando)"
fi

log "Starting discoverymaster in MASTER mode (file-based commands)..."
ssh ${ssh_options} "${remote_user}@${master_ip}" "\
  cd '${remote_work_dir}' && \
  rm -f '${remote_status_file}' '${remote_ready_file}' 2>/dev/null || true; \
  /usr/bin/nohup '${remote_bin_dir}/discoverymaster' master ':${DISCOVERY_PORT}' '${remote_work_dir}/master-commands.cmd' \
    > '${remote_work_dir}/logs/discoverymaster.log' 2>&1 < /dev/null & \
  echo PID=\$!; \
  sleep 0.2; \
  pgrep -af discoverymaster 2>/dev/null || true; \
  tail -n 20 '${remote_work_dir}/logs/discoverymaster.log' 2>/dev/null || true; \
" </dev/null

log "Verificando se o master está vivo e escutando na porta ${DISCOVERY_PORT}..."
# checagem simples: gRPC port listening
ssh ${ssh_options} "${remote_user}@${master_ip}" "\
  (ss -lnt 2>/dev/null || netstat -lnt 2>/dev/null || true) | grep -q ':${DISCOVERY_PORT}' && echo OK || (echo FAIL; exit 1)
" </dev/null

log "Master started successfully."

