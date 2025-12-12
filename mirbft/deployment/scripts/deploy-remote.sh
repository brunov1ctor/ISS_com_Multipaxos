#!/usr/bin/env bash
set -euo pipefail

# Usage:
#   scripts/deploy-remote.sh <instance_info_file> <exp_data_dir>
#
# This script:
#   1) Reads master IP from instance info
#   2) Starts master (discoverymaster)
#   3) Starts result fetching in background (writes exp_data_dir/result-fetching.log)
#   4) Starts all slaves (peers + 1client)
#   5) Waits for result fetching to finish (i.e., master status -> FINISHED), then downloads results

instance_info_file="${1:?missing instance_info_file}"
exp_data_dir="${2:?missing exp_data_dir}"

repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_dir"

# Load global vars used by the deployment scripts
# (remote_user, remote_work_dir, local_result_fetching_log, etc.)
# shellcheck source=/dev/null
source scripts/global-vars.sh

if [[ ! -f "$instance_info_file" ]]; then
  echo "[deploy-remote] ERROR: instance info file not found: $instance_info_file" >&2
  exit 1
fi

if [[ ! -d "$exp_data_dir" ]]; then
  echo "[deploy-remote] ERROR: experiment data dir not found: $exp_data_dir" >&2
  exit 1
fi

# --------------------------------------------------------------------
# 1) Parse master IP from instance-info
# --------------------------------------------------------------------
master_ip="$(head -n 1 "$instance_info_file" | awk '{print $2}')"
if [[ -z "$master_ip" ]]; then
  echo "[deploy-remote] ERROR: failed to parse master IP from: $instance_info_file" >&2
  exit 1
fi

# --------------------------------------------------------------------
# 2) Reset remote machines (master only here; slaves are reset implicitly by start script)
# --------------------------------------------------------------------
echo
echo "Limpando processos antigos e removendo possíveis limitações de banda nas máquinas remotas..."
echo "  - [reset-proc] $master_ip: matando processos antigos e removendo traffic shaping..."
scripts/remote-machine-status.sh "$master_ip" reset-proc || echo "  - [reset-proc] $master_ip: WARNING (ssh exit $?). Continuando mesmo assim."

echo
echo "  - [reset-state] $master_ip: removendo shaping, matando processos do experimento e limpando arquivos antigos..."
scripts/remote-machine-status.sh "$master_ip" reset-state || echo "  - [reset-state] $master_ip: WARNING (ssh exit $?). Continuando mesmo assim."

echo
echo "Estado das máquinas remotas resetado."
echo

# --------------------------------------------------------------------
# 3) Start master (background)
# --------------------------------------------------------------------
echo "Starting master on $master_ip."
scripts/start-master.sh "$exp_data_dir" "$master_ip" &
master_pid=$!

# --------------------------------------------------------------------
# 4) Start result fetching in background (writes log into exp_data_dir)
# --------------------------------------------------------------------
result_fetching_log_path="$exp_data_dir/$local_result_fetching_log"
echo "Starting result fetching in the background."
echo "For progress on experiment result fetching, see $result_fetching_log_path."
scripts/fetch-results.sh "$master_ip" "$exp_data_dir" > "$result_fetching_log_path" 2>&1 &
fetch_pid=$!

# --------------------------------------------------------------------
# 5) Start peer and client slaves
# --------------------------------------------------------------------
peers_tag="peers"
echo "Starting peer slaves (tag=$peers_tag)."
scripts/start-remote-slaves.sh "$exp_data_dir" "$instance_info_file" "$peers_tag"

clients_tag="1client"
echo "Starting client slaves (tag=$clients_tag)."
scripts/start-remote-slaves.sh "$exp_data_dir" "$instance_info_file" "$clients_tag"

echo "All slaves started. waiting for them to finish."
echo "Remote slave deployment finished."
echo

# --------------------------------------------------------------------
# 6) Wait for result fetching and master (best-effort)
# --------------------------------------------------------------------
echo "Waiting for deployment process and result fetching to finish."
echo "For progress on experiment result fetching, see $result_fetching_log_path."
echo

set +e
wait "$fetch_pid"
fetch_rc=$?
wait "$master_pid"
master_rc=$?
set -e

if [[ $fetch_rc -ne 0 ]]; then
  echo "[deploy-remote] ERROR: fetch-results.sh exited with code $fetch_rc" >&2
  echo "[deploy-remote] Tip: veja o log: $result_fetching_log_path" >&2
  exit $fetch_rc
fi

# master_rc pode ser não-zero se o start-master apenas disparou e retornou (ou se o SSH caiu),
# mas o fetch-results é quem garante que o experimento terminou e os resultados foram baixados.
echo "Done. Experiment data directory: $exp_data_dir"
exit 0

