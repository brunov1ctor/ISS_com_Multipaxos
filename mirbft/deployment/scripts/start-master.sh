#!/usr/bin/env bash
set -euo pipefail

ts(){ date '+%Y-%m-%d %H:%M:%S'; }
log(){ echo "[start-master][$(ts)] $*"; }

# ===============================
# Required environment variables
# ===============================
: "${remote_user:?missing remote_user}"
: "${master_ip:?missing master_ip}"              # MUST be IP/hostname
: "${remote_work_dir:?missing remote_work_dir}"  # ex: /users/Bruno/iss
: "${remote_bin_dir:?missing remote_bin_dir}"    # ex: /users/Bruno/go/bin
: "${local_bin_dir:?missing local_bin_dir}"
: "${exp_data_dir:?missing exp_data_dir}"
: "${DISCOVERY_PORT:=9999}"
: "${ssh_options:=-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null}"

local_master_cmd="${exp_data_dir}/master-commands.cmd"
remote_master_cmd="${remote_work_dir}/master-commands.cmd"
remote_log="${remote_work_dir}/main_log.log"
remote_pid="${remote_work_dir}/.discoverymaster.pid"

log "remote_user=${remote_user}"
log "master_ip=${master_ip}"
log "remote_work_dir=${remote_work_dir}"
log "remote_bin_dir=${remote_bin_dir}"
log "exp_data_dir=${exp_data_dir}"
log "DISCOVERY_PORT=${DISCOVERY_PORT}"

# ===============================
# Sanity checks
# ===============================
if [[ "${master_ip}" == /* || "${master_ip}" == *"/deployment-data/"* ]]; then
  log "ERROR: master_ip is NOT an IP/hostname: ${master_ip}"
  exit 2
fi

if [[ ! -f "${local_master_cmd}" ]]; then
  log "ERROR: master-commands.cmd not found: ${local_master_cmd}"
  exit 3
fi

# ===============================
# Prepare remote environment
# ===============================
log "Preparing remote directories"
ssh ${ssh_options} "${remote_user}@${master_ip}" \
  "mkdir -p '${remote_work_dir}' '${remote_bin_dir}'"

# ===============================
# Copy master commands (DSL!)
# ===============================
log "Copying master-commands.cmd via SCP"
scp ${ssh_options} \
  "${local_master_cmd}" \
  "${remote_user}@${master_ip}:${remote_master_cmd}"

# ===============================
# Copy binaries
# ===============================
for bin in discoverymaster discoveryslave orderingpeer orderingclient; do
  if [[ -x "${local_bin_dir}/${bin}" ]]; then
    log "Sending binary ${bin}"
    scp ${ssh_options} \
      "${local_bin_dir}/${bin}" \
      "${remote_user}@${master_ip}:${remote_bin_dir}/${bin}"
    ssh ${ssh_options} "${remote_user}@${master_ip}" \
      "chmod +x '${remote_bin_dir}/${bin}'"
  fi
done

# ===============================
# Kill old master
# ===============================
log "Killing old discoverymaster"
ssh ${ssh_options} "${remote_user}@${master_ip}" \
  "pkill -9 -f '${remote_bin_dir}/discoverymaster' 2>/dev/null || true"

# ===============================
# Start discoverymaster (DSL input!)
# ===============================
log "Starting discoverymaster"
ssh ${ssh_options} "${remote_user}@${master_ip}" "
  set -e
  cd '${remote_work_dir}'
  echo '[START]' > '${remote_log}'
  echo 'PWD='\"\$(pwd)\" >> '${remote_log}'
  echo 'PORT=${DISCOVERY_PORT}' >> '${remote_log}'
  echo 'HEAD master-commands:' >> '${remote_log}'
  head -n 40 '${remote_master_cmd}' >> '${remote_log}' || true

  nohup '${remote_bin_dir}/discoverymaster' '${DISCOVERY_PORT}' \
    < '${remote_master_cmd}' \
    >> '${remote_log}' 2>&1 < /dev/null &

  echo \$! > '${remote_pid}'
  sleep 1

  if ! kill -0 \$(cat '${remote_pid}') 2>/dev/null; then
    echo 'ERROR: discoverymaster died immediately' >> '${remote_log}'
    tail -n 200 '${remote_log}'
    exit 10
  fi
"

log "Master started successfully"
ssh ${ssh_options} "${remote_user}@${master_ip}" \
  "tail -n 80 '${remote_log}'"

