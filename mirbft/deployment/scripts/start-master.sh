#!/bin/bash
# scripts/start-master.sh

set -euo pipefail

this_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
deployment_dir="$(cd "$this_dir/.." && pwd)"

# shellcheck source=/dev/null
. "$deployment_dir/scripts/global-vars.sh"

exp_data_dir="$1"
master_ip="$2"

local_master_cmd="$exp_data_dir/master-commands.cmd"
remote_master_cmd="$remote_work_dir/master-commands.cmd"

ssh_options="${ssh_options:--o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null}"
remote_user="${remote_user:-$USER}"

# local bin dir correto do node-0
local_bin_dir="${LOCAL_BIN_DIR:-${GOBIN:-${GOPATH:-$HOME/go}/bin}}"

echo "Using experiment data directory: $exp_data_dir"
echo "Using master IP: $master_ip"
echo "Local master command script: $local_master_cmd"
echo "Remote work dir: $remote_work_dir"
echo "Remote master command path: $remote_master_cmd"
echo "Local bin dir: $local_bin_dir"
echo

echo "Ensuring remote directories on master ($master_ip)."
ssh $ssh_options "${remote_user}@${master_ip}" " \
  mkdir -p \
    '$remote_work_dir' \
    '$remote_work_dir/config' \
    '$remote_work_dir/logs' \
    '$remote_work_dir/scripts' \
    '$remote_work_dir/tls-data' \
    '$remote_exp_dir' \
    '$remote_exp_dir/raw-results' \
    '$remote_work_dir/experiment-config' \
    '$remote_bin_dir' \
" </dev/null

echo "Copying master commands and helper scripts to master."
scp $ssh_options "$local_master_cmd" "${remote_user}@${master_ip}:${remote_master_cmd}"
scp $ssh_options "$deployment_dir/scripts/start-slave.sh" "${remote_user}@${master_ip}:${remote_work_dir}/scripts/start-slave.sh"
scp $ssh_options "$deployment_dir/scripts/stubborn-scp.sh" "${remote_user}@${master_ip}:${remote_work_dir}/scripts/stubborn-scp.sh"

echo "Copying required binaries to master (if found locally)."
for bin in discoverymaster discoveryslave orderingpeer orderingclient; do
  if [[ -x "${local_bin_dir}/${bin}" ]]; then
    echo "  - sending ${bin} -> ${remote_bin_dir}/${bin}"
    scp $ssh_options "${local_bin_dir}/${bin}" "${remote_user}@${master_ip}:${remote_bin_dir}/${bin}"
  else
    echo "  [WARN] local bin not found: ${local_bin_dir}/${bin}"
  fi
done

echo "Copying experiment config files to master."
local_config_src_dir="$exp_data_dir/config"
if ls "$local_config_src_dir"/config-*.yml >/dev/null 2>&1; then
  echo "  - Enviando configs de $local_config_src_dir para ${master_ip}:${remote_work_dir}/experiment-config/ ..."
  scp $ssh_options "$local_config_src_dir"/config-*.yml "${remote_user}@${master_ip}:${remote_work_dir}/experiment-config/"
else
  echo "WARNING: nenhum arquivo config-XXXX.yml encontrado em $local_config_src_dir; configs não foram copiadas."
fi
echo

echo "Starting discoverymaster on remote master ($master_ip)."
echo "  - remote_bin_dir     = $remote_bin_dir"
echo "  - remote_ready_file  = $remote_ready_file"
echo "  - remote_status_file = $remote_status_file"
echo "  - master_port        = $master_port"
echo

ssh $ssh_options "${remote_user}@${master_ip}" " \
  set -e; \
  chmod +x '$remote_bin_dir/discoverymaster' 2>/dev/null || true; \
  if [[ ! -x '$remote_bin_dir/discoverymaster' ]]; then \
    echo 'ERRO: discoverymaster não existe ou não é executável em $remote_bin_dir/discoverymaster'; \
    ls -la '$remote_bin_dir' | head -n 50; \
    exit 2; \
  fi; \
  cd '$remote_work_dir' && \
  /usr/bin/nohup '$remote_bin_dir/discoverymaster' \
    -ownID 0 \
    -listen '$master_ip:$master_port' \
    -readyFile '$remote_ready_file' \
    -statusFile '$remote_status_file' \
    -commands '$remote_master_cmd' \
    > main_log.log 2>&1 & \
" </dev/null

echo "Master discovery + orderingclient disparados via discoverymaster."
echo "start-master.sh finished."

