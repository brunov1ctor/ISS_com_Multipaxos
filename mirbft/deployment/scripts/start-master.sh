#!/usr/bin/env bash
set -euo pipefail

# start-master.sh
# Sobe discoverymaster no nó master em modo MASTER (commands file).

remote_user="$1"
master_ip="$2"
ssh_options="$3"
remote_work_dir="$4"
remote_bin_dir="$5"
exp_data_dir="$6"
DISCOVERY_PORT="$7"
local_master_cmd="$8"

log() { echo "[start-master][$(date '+%Y-%m-%d %H:%M:%S')] $*"; }
warn() { echo "[start-master][$(date '+%Y-%m-%d %H:%M:%S')] WARN: $*" >&2; }
die() { echo "[start-master][$(date '+%Y-%m-%d %H:%M:%S')] ERROR: $*" >&2; exit 1; }

log "remote_user=$remote_user"
log "master_ip=$master_ip"
log "ssh_options=$ssh_options"
log "remote_work_dir=$remote_work_dir"
log "remote_bin_dir=$remote_bin_dir"
log "exp_data_dir=$exp_data_dir"
log "DISCOVERY_PORT=$DISCOVERY_PORT"
log "local_master_cmd=$local_master_cmd"

[[ -f "$local_master_cmd" ]] || die "local master-commands.cmd não existe: $local_master_cmd"

log "Ensuring remote workdir exists..."
ssh ${ssh_options} "${remote_user}@${master_ip}" "mkdir -p '${remote_work_dir}' '${remote_work_dir}/logs' '${remote_work_dir}/scripts' '${remote_work_dir}/config' '${remote_work_dir}/experiment-config' '${remote_work_dir}/current-deployment-data'"

log "Copying master-commands.cmd to remote..."
scp ${ssh_options} "$local_master_cmd" "${remote_user}@${master_ip}:${remote_work_dir}/master-commands.cmd"

# Copia configs gerados localmente (deployment-data/.../local-config/*.yml) para o master em experiment-config/
local_cfg_dir="${exp_data_dir}/local-config"
if [[ -d "$local_cfg_dir" ]]; then
  log "Copying generated configs to master (experiment-config/) via scp..."
  # copia somente os config-*.yml
  scp ${ssh_options} "${local_cfg_dir}"/config-*.yml "${remote_user}@${master_ip}:${remote_work_dir}/experiment-config/" || true

  ssh ${ssh_options} "${remote_user}@${master_ip}" "echo '[start-master] remote experiment-config:'; ls -la '${remote_work_dir}/experiment-config' || true"
else
  warn "local_cfg_dir não existe (pulando): $local_cfg_dir"
fi

log "Killing previous discoverymaster (if any)..."
set +e
ssh ${ssh_options} "${remote_user}@${master_ip}" "
  pkill -9 -f '${remote_bin_dir}/discoverymaster' 2>/dev/null || true
  pkill -9 -f 'discoverymaster ' 2>/dev/null || true
  sleep 0.2
  pgrep -af discoverymaster 2>/dev/null || true
" < /dev/null
rc=$?
set -e
if [[ $rc -ne 0 ]]; then
  warn "Kill step returned rc=$rc (continuando)"
fi

log "Starting discoverymaster in MASTER mode (file-based commands)..."
ssh ${ssh_options} "${remote_user}@${master_ip}" "
  cd '${remote_work_dir}' &&
  rm -f '${remote_work_dir}/status' '${remote_work_dir}/master-ready' 2>/dev/null || true;
  /usr/bin/nohup '${remote_bin_dir}/discoverymaster' master ':${DISCOVERY_PORT}' '${remote_work_dir}/master-commands.cmd' \
    > '${remote_work_dir}/logs/discoverymaster.log' 2>&1 < /dev/null &
  echo PID=\$!;
  sleep 0.2;
  pgrep -af discoverymaster 2>/dev/null || true;
  tail -n 30 '${remote_work_dir}/logs/discoverymaster.log' 2>/dev/null || true;
" < /dev/null

log "Verificando se o master está vivo e escutando na porta ${DISCOVERY_PORT}..."
ssh ${ssh_options} "${remote_user}@${master_ip}" "
  timeout 6 bash -lc 'for i in {1..30}; do (echo > /dev/tcp/127.0.0.1/${DISCOVERY_PORT}) >/dev/null 2>&1 && exit 0; sleep 0.2; done; exit 1'
" && echo "OK" || die "Master não está escutando na porta ${DISCOVERY_PORT}"

log "Master started successfully."

