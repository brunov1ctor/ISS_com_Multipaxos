#!/usr/bin/env bash
set -euo pipefail

# Usage:
#   start-master.sh <exp_data_dir> <master_ip>
#
# Where:
#   exp_data_dir = .../deployment-data/remote-0000
#   master_ip    = 172.20.6.3

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=/dev/null
source "${SCRIPT_DIR}/global-vars.sh"

exp_data_dir="${1:-}"
master_ip="${2:-}"

if [[ -z "${exp_data_dir}" || -z "${master_ip}" ]]; then
  echo "[start-master] ERROR: missing args. Usage: $0 <exp_data_dir> <master_ip>" >&2
  exit 2
fi

# Default remote_user if not provided by caller
remote_user="${remote_user:-${REMOTE_USER:-${USER}}}"

ssh_options="${ssh_options:--o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null}"

# Where master will run on remote
remote_work_dir="${remote_work_dir:-/users/${remote_user}/iss}"
remote_bin_dir="${remote_bin_dir:-/users/${remote_user}/go/bin}"

DISCOVERY_PORT="${DISCOVERY_PORT:-9999}"

# Files used by discoverymaster command expansion (write-file $ready_file, $status_file)
remote_status_file="${remote_status_file:-${remote_work_dir}/status}"
remote_ready_file="${remote_ready_file:-${remote_work_dir}/master-ready}"

local_master_cmd="${exp_data_dir}/master-commands.cmd"
remote_master_cmd="${remote_work_dir}/master-commands.cmd"

echo "[start-master][$(date '+%F %T')] remote_user=${remote_user}"
echo "[start-master][$(date '+%F %T')] master_ip=${master_ip}"
echo "[start-master][$(date '+%F %T')] ssh_options=${ssh_options}"
echo "[start-master][$(date '+%F %T')] remote_work_dir=${remote_work_dir}"
echo "[start-master][$(date '+%F %T')] remote_bin_dir=${remote_bin_dir}"
echo "[start-master][$(date '+%F %T')] exp_data_dir=${exp_data_dir}"
echo "[start-master][$(date '+%F %T')] DISCOVERY_PORT=${DISCOVERY_PORT}"
echo "[start-master][$(date '+%F %T')] local_master_cmd=${local_master_cmd}"

if [[ ! -f "${local_master_cmd}" ]]; then
  echo "[start-master] ERROR: local master-commands.cmd not found: ${local_master_cmd}" >&2
  exit 3
fi

echo "[start-master][$(date '+%F %T')] Ensuring remote workdir exists..."
ssh ${ssh_options} "${remote_user}@${master_ip}" "mkdir -p '${remote_work_dir}'"

echo "[start-master][$(date '+%F %T')] Copying master-commands.cmd to remote..."
scp ${ssh_options} "${local_master_cmd}" "${remote_user}@${master_ip}:${remote_master_cmd}"

echo "[start-master][$(date '+%F %T')] Killing previous discoverymaster (if any)..."
ssh ${ssh_options} "${remote_user}@${master_ip}" "pkill -9 -f '${remote_bin_dir}/discoverymaster' 2>/dev/null || true"

echo "[start-master][$(date '+%F %T')] Starting discoverymaster in MASTER mode (file-based commands)..."
# IMPORTANT:
# - MASTER mode keeps server alive and serves commands from file
# - We export ready_file/status_file so "write-file $ready_file" works
ssh ${ssh_options} "${remote_user}@${master_ip}" bash -lc "'
  set -euo pipefail
  cd \"${remote_work_dir}\"

  export ready_file=\"${remote_ready_file}\"
  export status_file=\"${remote_status_file}\"
  export master_port=\"${DISCOVERY_PORT}\"

  rm -f \"${remote_ready_file}\" \"${remote_status_file}\" 2>/dev/null || true

  nohup \"${remote_bin_dir}/discoverymaster\" master \":${DISCOVERY_PORT}\" \"${remote_master_cmd}\" \
    > main_log.log 2>&1 < /dev/null &

  echo \$! > .discoverymaster.pid
  sleep 1
  echo \"PID=\$(cat .discoverymaster.pid)\"
  tail -n 80 main_log.log || true
'"

echo "[start-master][$(date '+%F %T')] Done."

