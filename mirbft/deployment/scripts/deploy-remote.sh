#!/usr/bin/env bash

set -euo pipefail

ts() { date +"%Y-%m-%d %H:%M:%S%z"; }
log_i() { echo "[INFO ][$(ts)] $*"; }
log_w() { echo "[WARN ][$(ts)] $*" >&2; }
log_e() { echo "[ERRO ][$(ts)] $*" >&2; }

# -------------------------------
# Valores vêm do deploy.sh
# -------------------------------

# NÃO usamos sentinel checks para ficar igual ao original
exp_data_dir="$exp_data_dir"
instance_info_file="$instance_info_file"
instance_info_file_name="$instance_info_file_name"
remote_user="$remote_user"
remote_work_dir="$remote_work_dir"
remote_status_file="$remote_status_file"
remote_delete_files="$remote_delete_files"
master_port="$master_port"
local_master_command_template_file="$local_master_command_template_file"
local_master_command_file="$local_master_command_file"
local_result_fetching_log="$local_result_fetching_log"
cancel_instances="${cancel_instances:-false}"

ssh_options="-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null"

# Se remote_private_key_file não existir, deixa vazio
remote_private_key_file="${remote_private_key_file:-}"
export ssh_key_file="$remote_private_key_file"

# -------------------------------
# MASTER IP
# -------------------------------

master_ip=$(awk '$4 == "master" {print $2}' "$instance_info_file" | head -n1)

if [ -z "$master_ip" ]; then
  log_e "could not obtain master ip from instance info file: $instance_info_file"
  exit 1
fi

cp "$instance_info_file" "$exp_data_dir/$instance_info_file_name"

log_i "Using instance info file: $instance_info_file"
log_i "Master IP address      : $master_ip"
log_i "remote_user            : $remote_user"
log_i "remote_work_dir        : $remote_work_dir"
log_i "remote_status_file     : $remote_status_file"
log_i "master_port            : $master_port"

# -------------------------------
# MASTER COMMANDS
# -------------------------------

log_i "Generating final master command file..."

export own_public_ip="$master_ip"
export master_port
export status_file="$remote_status_file"

envsubst '$ssh_key_file $own_public_ip $master_port $status_file' \
    < "$exp_data_dir/$local_master_command_template_file" \
    > "$exp_data_dir/$local_master_command_file"

echo -e "\nwrite-file $status_file DONE" >> "$exp_data_dir/$local_master_command_file"

log_i "Master command file ready: $exp_data_dir/$local_master_command_file"

# -------------------------------
# RESET REMOTO
# -------------------------------

log_i "Killing processes and pruning remote state..."

for ip in $(awk '{print $2}' "$instance_info_file"); do
    ssh $ssh_options ${remote_user}@"$ip" \
      "kill -9 \$(ps -ef | grep 'analyze-continuously' | grep -v \$\$ | awk '{print \$2}')" \
      >/dev/null 2>&1 || log_w "$ip: could not kill analyze-continuously"
    sleep 0.1
done

log_i "Killed continuous analysis scripts."

for ip in $(awk '{print $2}' "$instance_info_file"); do
    ssh $ssh_options ${remote_user}@"$ip" "
        tc qdisc del dev eth0 root tbf rate 1gbit burst 320kbit latency 400ms 2>/dev/null || true
        killall -9 discoverymaster discoveryslave orderingpeer orderingclient scp rsync 2>/dev/null || true
        rm -rf $remote_delete_files
        echo RUNNING > $remote_status_file
        kill -9 \$(ps -ef | grep 'sshd: ${remote_user}@notty' | awk '{print \$2}') 2>/dev/null || true
    " >/dev/null 2>&1 || log_w "$ip: reset failed"
    sleep 0.1
done

log_i "Remote state reset."

# -------------------------------
# MASTER
# -------------------------------

log_i "Starting master on $master_ip..."
scripts/start-master.sh "$exp_data_dir" "$master_ip"
log_i "Master started script returned."

# -------------------------------
# SLAVES (peers / clients)
# -------------------------------

log_i "Starting peer slaves..."
scripts/start-remote-slaves.sh "$exp_data_dir" "$instance_info_file" peers "$master_ip"

log_i "Starting client slaves..."
scripts/start-remote-slaves.sh "$exp_data_dir" "$instance_info_file" 1client "$master_ip"

log_i "All slaves started."

# -------------------------------
# FETCH-RESULTS
# -------------------------------

log_i "Starting fetch-results in background..."
scripts/fetch-results.sh "$master_ip" "$exp_data_dir" \
   > "$exp_data_dir/$local_result_fetching_log" 2>&1 &
fetch_pid=$!

log_i "Waiting for fetch-results (PID $fetch_pid)..."
log_i "Progress log: $exp_data_dir/$local_result_fetching_log"

if ! wait "$fetch_pid"; then
    rc=$?
    log_e "fetch-results.sh exited with non-zero: $rc"
    log_e "Veja: $exp_data_dir/$local_result_fetching_log"
    exit $rc
fi

log_i "fetch-results complete."

# -------------------------------
# CANCEL
# -------------------------------

if $cancel_instances; then
    log_i "Canceling cloud instances as requested..."
    scripts/cancel-cloud-instances.sh "$exp_data_dir/$instance_info_file_name"
else
    log_i "Remember: cancel cloud instances using:"
    log_i "  scripts/cancel-cloud-instances.sh $exp_data_dir/$instance_info_file_name"
fi

log_i "deploy-remote.sh finished successfully."

