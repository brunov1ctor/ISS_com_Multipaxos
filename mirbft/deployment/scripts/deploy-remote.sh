#!/bin/bash
# scripts/deploy-remote.sh
#
# Este script é "sourced" por deploy.sh (não é para ser chamado direto).
# Ele depende das variáveis setadas por initialize-deployment.sh e global-vars.sh.

set -euo pipefail

# --------------------------------------------------------------------
# Sanity: variáveis obrigatórias (vêm do initialize-deployment.sh)
# --------------------------------------------------------------------
: "${repo_dir:?missing repo_dir}"
: "${exp_data_dir:?missing exp_data_dir}"
: "${instance_info_file:?missing instance_info_file}"
: "${master_ip:?missing master_ip}"
: "${remote_user:?missing remote_user}"
: "${remote_work_dir:?missing remote_work_dir}"
: "${remote_bin_dir:?missing remote_bin_dir}"
: "${local_bin_dir:?missing local_bin_dir}"
: "${ssh_options:?missing ssh_options}"
: "${local_result_fetching_log:?missing local_result_fetching_log}"

# Alguns scripts usam isso
export repo_dir exp_data_dir instance_info_file master_ip
export remote_user remote_work_dir remote_bin_dir local_bin_dir ssh_options

echo
echo "=================================================="
echo "[REMOTE] Deploy em máquinas remotas"
echo "=================================================="
echo "[deploy-remote] exp_data_dir       = $exp_data_dir"
echo "[deploy-remote] instance_info_file = $instance_info_file"
echo "[deploy-remote] master_ip          = $master_ip"
echo "[deploy-remote] remote_work_dir    = $remote_work_dir"
echo "[deploy-remote] remote_bin_dir     = $remote_bin_dir"
echo "[deploy-remote] local_bin_dir      = $local_bin_dir"
echo

# --------------------------------------------------------------------
# 1) Start master (copia scripts/configs/binários no master)
# --------------------------------------------------------------------
echo "[deploy-remote] Starting master on $master_ip ..."
scripts/start-master.sh "$master_ip" "$exp_data_dir"

# --------------------------------------------------------------------
# 2) Start slaves (peers + clients) via script dedicado
# --------------------------------------------------------------------
echo "[deploy-remote] Starting remote slaves ..."
scripts/start-remote-slaves.sh "$exp_data_dir" "$instance_info_file"

echo
echo "[deploy-remote] All slaves start attempted. waiting for them to finish."
echo "Remote slave deployment finished."
echo

# --------------------------------------------------------------------
# 3) Fetch results (CRIA result-fetching.log NO exp_data_dir)
# --------------------------------------------------------------------
echo "Waiting for deployment process and result fetching to finish."
echo "For progress on experiment result fetching, see $exp_data_dir/$local_result_fetching_log."

# roda fetch-results em background e espera terminar
scripts/fetch-results.sh "$master_ip" "$exp_data_dir" > "$exp_data_dir/$local_result_fetching_log" 2>&1 &
fetch_pid=$!

wait "$fetch_pid" || true

echo
echo "[deploy-remote] Fetch results finished (or returned non-zero; veja o log)."
echo "[deploy-remote] exp_data_dir: $exp_data_dir"
echo

