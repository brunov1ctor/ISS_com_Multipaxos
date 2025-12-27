#!/usr/bin/env bash
set -euo pipefail

# start-master.sh
#
# Starts the master node in remote deployment.
# The master node is responsible for:
#  - Starting all other nodes
#  - Collecting results

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../.." && pwd)"

# shellcheck source=global-vars.sh
source "${script_dir}/global-vars.sh"

# Args:
#   1: deployment-data root on local machine (e.g., /tmp/deployment-data)
#   2: remote user (e.g., Bruno)
#   3: remote master ip (e.g., 172.21.17.1)
#   4: remote work dir (e.g., /tmp/iss-Bruno)
#   5: remote output dir (optional) (e.g., /tmp/deployment-data/experiment-output)
deployment_data_root="${1:?deployment_data_root is required}"
remote_user="${2:?remote_user is required}"
remote_master_ip="${3:?remote_master_ip is required}"
remote_work_dir="${4:?remote_work_dir is required}"
remote_output_dir="${5:-}"

remote_base_dir="/users/${remote_user}/iss"

# IMPORTANT:
# - "heavy" data (logs/work/status) can go to remote_work_dir (e.g., /tmp/iss-Bruno)
# - configs/tls remain on remote_base_dir (e.g., /users/Bruno/iss) to avoid scp path mismatch
#
# Minimal fix: force remote_config_dir to always be under remote_base_dir
remote_config_dir="${remote_base_dir}/experiment-config"

if [[ -n "${remote_output_dir}" ]]; then
  remote_output_dir="${remote_output_dir%/}"
else
  # default heavy output path
  remote_output_dir="/tmp/deployment-data/experiment-output"
fi

deployment_data_root="${deployment_data_root%/}"

# local directories
exp_data_dir="${deployment_data_root}/remote-0000"
local_exp_output_dir="${deployment_data_root}/experiment-output"

# remote directories
remote_deployment_data_root="/tmp/deployment-data"
remote_exp_data_dir="${remote_deployment_data_root}/remote-0000"

# Make sure required local inputs exist
if [[ ! -d "${exp_data_dir}" ]]; then
  echo "ERROR: Local exp_data_dir does not exist: ${exp_data_dir}" >&2
  exit 1
fi

if [[ ! -d "${exp_data_dir}/config" ]]; then
  echo "ERROR: Local config dir does not exist: ${exp_data_dir}/config" >&2
  exit 1
fi

# Ensure remote base dirs exist
ssh "${remote_user}@${remote_master_ip}" "mkdir -p '${remote_work_dir}'"
ssh "${remote_user}@${remote_master_ip}" "mkdir -p '${remote_output_dir}'"
ssh "${remote_user}@${remote_master_ip}" "mkdir -p '${remote_config_dir}'"
ssh "${remote_user}@${remote_master_ip}" "mkdir -p '${remote_base_dir}/config'"
ssh "${remote_user}@${remote_master_ip}" "mkdir -p '${remote_base_dir}/tls-data'"
ssh "${remote_user}@${remote_master_ip}" "mkdir -p '${remote_deployment_data_root}'"
ssh "${remote_user}@${remote_master_ip}" "mkdir -p '${remote_exp_data_dir}'"

# Copy remote-0000 (deployment data) - this can be heavy but is needed on master
# NOTE: We put remote-0000 under /tmp/deployment-data on the remote host.
rsync -avz --delete \
  "${exp_data_dir}/" \
  "${remote_user}@${remote_master_ip}:${remote_exp_data_dir}/"

# Copy experiment-output directory root (heavy results)
# (optional: may already be created by running experiments)
ssh "${remote_user}@${remote_master_ip}" "mkdir -p '${remote_output_dir}'"

# Copy configs to remote_config_dir (LIGHT, and must remain stable under /users/${remote_user}/iss)
# This is the root cause fix for master-commands.cmd referencing /users/.../experiment-config.
rsync -avz \
  "${exp_data_dir}/config/" \
  "${remote_user}@${remote_master_ip}:${remote_config_dir}/"

# Validate that at least config-0000.yml exists on remote master
ssh "${remote_user}@${remote_master_ip}" "test -s '${remote_config_dir}/config-0000.yml' || (echo 'WARN: missing config-0000.yml in ${remote_config_dir}' >&2; true)"

# Generate master commands on local machine
# The generated master-commands.cmd will reference BASE_DIR=/users/${remote_user}/iss and thus /users/.../experiment-config
master_commands_local="${exp_data_dir}/master-commands.cmd"

python3 "${script_dir}/generate-master-commands.py" \
  --deployment-data-root "${deployment_data_root}" \
  --node-count "$(cat "${exp_data_dir}/instance-info/node-count")" \
  --work-dir "${remote_work_dir}" \
  --remote-user "${remote_user}" \
  --remote-master-ip "${remote_master_ip}" \
  --remote-output-dir "${remote_output_dir}" \
  --base-dir "${remote_base_dir}" \
  > "${master_commands_local}"

# Copy master-commands.cmd to remote master under remote_work_dir (heavy/work area)
rsync -avz \
  "${master_commands_local}" \
  "${remote_user}@${remote_master_ip}:${remote_work_dir}/master-commands.cmd"

# Run master commands on remote master
ssh "${remote_user}@${remote_master_ip}" "bash '${remote_work_dir}/master-commands.cmd'"

# After completion, pull experiment output back (heavy)
mkdir -p "${local_exp_output_dir}"
rsync -avz \
  "${remote_user}@${remote_master_ip}:${remote_output_dir%/}/" \
  "${local_exp_output_dir}/"

echo "Done. Outputs copied to: ${local_exp_output_dir}"

