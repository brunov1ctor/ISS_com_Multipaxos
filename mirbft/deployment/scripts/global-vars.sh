#!/bin/bash

# scripts/global-vars.sh
#
# Variables shared across deployment scripts (local / cloud / remote).

# --------------------------------------------------------------------
# SSH options and key
# --------------------------------------------------------------------
remote_user="${remote_user:-Bruno}"
remote_private_key_file="${remote_private_key_file:-$HOME/.ssh/id_rsa}"
ssh_options="-i $remote_private_key_file -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null"

# --------------------------------------------------------------------
# Remote environment defaults
# --------------------------------------------------------------------
remote_gopath="${remote_gopath:-/users/Bruno/go}"
remote_bin_dir="${remote_bin_dir:-$remote_gopath/bin}"
remote_work_dir="${remote_work_dir:-/users/Bruno/iss}"
remote_exp_dir="${remote_exp_dir:-$remote_work_dir/current-deployment-data}"

# File on remote machines marking status
remote_status_file="${remote_status_file:-$remote_work_dir/status}"

# Files/directories to delete when resetting remote machines
remote_delete_files="${remote_delete_files:-$remote_work_dir/experiment-output $remote_work_dir/current-deployment-data $remote_work_dir/status $remote_work_dir/master-ready}"

# --------------------------------------------------------------------
# Local files
# --------------------------------------------------------------------
local_result_fetching_log="${local_result_fetching_log:-result-fetching.log}"
result_summary_file="${result_summary_file:-result-summary.csv}"

# --------------------------------------------------------------------
# Exit trap: kill all child processes of this script
# --------------------------------------------------------------------
trap_exit_command='{ jobs; for pid in $(jobs -p); do kill -9 "$pid" 2>/dev/null || true; done; }'

