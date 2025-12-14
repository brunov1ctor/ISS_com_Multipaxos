#!/usr/bin/env bash
set -euo pipefail

# Usage: start-slave.sh <tag> <master_ip> <own_public_ip> <own_private_ip> <remote_exp_dir>
tag="${1:-}"
master_ip="${2:-}"
own_public_ip="${3:-}"
own_private_ip="${4:-}"
remote_exp_dir="${5:-}"

if [[ -z "${tag}" || -z "${master_ip}" || -z "${own_public_ip}" || -z "${own_private_ip}" || -z "${remote_exp_dir}" ]]; then
  echo "Usage: $0 <tag> <master_ip> <own_public_ip> <own_private_ip> <remote_exp_dir>"
  exit 2
fi

# shellcheck source=/dev/null
source "$(dirname "$0")/global-vars.sh"

mkdir -p "${remote_work_dir}/logs"
mkdir -p "${remote_work_dir}/bin"
mkdir -p "${remote_work_dir}/config"
mkdir -p "${remote_work_dir}/tls-data"
mkdir -p "${remote_exp_dir}"

# Make sure both bin/ and scripts/ are visible to processes spawned by discoveryslave.
# (The discovery system runs commands via execve (no shell), so PATH matters.)
remote_scripts_dir="${remote_work_dir}/scripts"
export PATH="${remote_scripts_dir}:${remote_bin_dir}:${PATH}"

export master_port="${DISCOVERY_PORT}"
export own_public_ip="${own_public_ip}"
export own_private_ip="${own_private_ip}"

cd "${remote_work_dir}" || exit 1

# Start discoveryslave (keep running; master will feed commands)
exec /usr/bin/nohup "${remote_bin_dir}/discoveryslave" slave "${master_ip}:${DISCOVERY_PORT}" "${tag}" "${remote_exp_dir}" \
  > "${remote_work_dir}/logs/discoveryslave-${tag}.log" 2>&1 < /dev/null

