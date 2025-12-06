#!/bin/bash

# scripts/deploy-remote.sh
#
# Remote deployment using instance-info (e.g., Emulab/cluster).

# This script is *sourced* by deploy.sh, which has already:
#   - sourced scripts/global-vars.sh
#   - run scripts/initialize-deployment.sh
#
# So we assume the following variables are available:
#   - exp_data_dir
#   - instance_info_file
#   - cancel_instances
#   - remote_private_key_file, remote_status_file, ssh_options,
#     remote_work_dir, remote_delete_files, local_result_fetching_log, etc.

# 1) Discover master IP from instance-info (role == master)
master_ip=$(awk '$4 == "master" {print $2}' "$instance_info_file")
if [ -z "$master_ip" ]; then
  >&2 echo "deploy-remote.sh: could not obtain master ip from instance info file: $instance_info_file"
  exit 1
fi

echo "Using instance info file: $instance_info_file"
echo "       Master IP address: $master_ip"
echo

# --------------------------------------------------------------------
# 2) Pre-generate master command script if needed
# --------------------------------------------------------------------

local_master_command_template="master-commands-template.cmd"
local_master_command_file="master-commands.cmd"

# generate-master-commands.py will read the deployment file and produce
# a template script (with placeholders) that we then envsubst.
echo "initialize-deployment.sh: about to generate master commands:"
echo "  depl_type                     = remote"
echo "  deployment_file (.dpl)        = $deployment_file"
echo "  local_master_command_template = $local_master_command_template"
echo "  output template path          = $exp_data_dir/$local_master_command_template"

python3 scripts/generate-master-commands.py \
  remote \
  "$deployment_file" \
  "$local_master_command_template" \
  "$exp_data_dir"
rc=$?

echo "initialize-deployment.sh: generate-master-commands.py exit code = $rc"

if [ $rc -ne 0 ]; then
  echo "initialize-deployment.sh: failed processing deployment file: $deployment_file"
  # For remote we still continue, but deploy_schedule may be empty.
fi

# --------------------------------------------------------------------
# 3) Prepare master-commands.cmd locally, substituting placeholders
# --------------------------------------------------------------------

export ssh_key_file="$remote_private_key_file"
export own_public_ip="$master_ip"
export master_port
export status_file="$remote_status_file"

local_template_path="$exp_data_dir/$local_master_command_template"
local_cmd_path="$exp_data_dir/$local_master_command_file"

if [ ! -f "$local_template_path" ]; then
  echo "Using pre-generated master command script at $local_cmd_path."
  cp "$local_cmd_path" "$local_cmd_path" 2>/dev/null || true
else
  envsubst '$ssh_key_file $own_public_ip $master_port $status_file' \
    < "$local_template_path" \
    > "$local_cmd_path"
fi

echo "Master command script written to $local_cmd_path."
echo

# --------------------------------------------------------------------
# 4) Kill previous runs and prune state on all machines
# --------------------------------------------------------------------

echo "Killing everything that is alive and pruning state on the remote machines (including SSH) and removing potential bandwidth limit."
echo

# 4a) Kill continuous analysis scripts (if any)
while read -r instance_id ctrl_ip data_ip role itag; do
  ssh $ssh_options "$ctrl_ip" " \
    pids=\$(ps -ef | grep 'analyze-continuously' | grep -v grep | awk '{print \$2}'); \
    if [ -n \"\$pids\" ]; then kill -9 \$pids || true; fi \
  " >/dev/null 2>&1 || true &
  sleep 0.1
done < "$instance_info_file"
wait

echo "Killed continuous analysis scripts."
echo

# 4b) Reset machine state (processes, files, bandwidth)
while read -r instance_id ctrl_ip data_ip role itag; do
  ssh $ssh_options "$ctrl_ip" " \
    # Remove traffic shaping (ignore errors).
    tc qdisc del dev eth0 root tbf rate 1gbit burst 320kbit latency 400ms 2>/dev/null || true; \
    # Kill previous experiment processes (ignore if not running).
    killall -9 discoverymaster discoveryslave orderingpeer orderingclient scp rsync 2>/dev/null || true; \
    # Remove old experiment-related files.
    rm -rf $remote_delete_files 2>/dev/null || true; \
    # Ensure status dir and reset status to RUNNING.
    mkdir -p \"\$(dirname \"$remote_status_file\")\" 2>/dev/null || true; \
    echo RUNNING > \"$remote_status_file\" 2>/dev/null || true \
  " >/dev/null 2>&1 || true &
  sleep 0.1
done < "$instance_info_file"
wait

echo
echo " Reset machine state."
echo

# --------------------------------------------------------------------
# 5) Start master (discoverymaster + orderingclient) on master node
# --------------------------------------------------------------------

echo "Starting master on $master_ip."
scripts/start-master.sh "$exp_data_dir" "$master_ip"

# --------------------------------------------------------------------
# 6) Start remote slaves (peers and clients) according to instance-info
# --------------------------------------------------------------------

# Count slaves with tag 'peers' and '1client'
num_peers=$(awk '$4 == "slave" && $5 == "peers" {c++} END {print c+0}' "$instance_info_file")
num_clients=$(awk '$4 == "slave" && $5 == "1client" {c++} END {print c+0}' "$instance_info_file")

if [ "$num_peers" -gt 0 ]; then
  echo "Starting $num_peers peer slaves."
  scripts/start-remote-slaves.sh "$exp_data_dir" "peers" "$num_peers" "$master_ip" "$instance_info_file"
fi

if [ "$num_clients" -gt 0 ]; then
  echo "Starting $num_clients client slaves (tag=1client)."
  scripts/start-remote-slaves.sh "$exp_data_dir" "1client" "$num_clients" "$master_ip" "$instance_info_file"
fi

echo "All slaves started. waiting for them to finish."
echo "Remote slave deployment finished."

# --------------------------------------------------------------------
# 7) Fetch results in background
# --------------------------------------------------------------------

scripts/fetch-results.sh "$master_ip" "$exp_data_dir" > "$exp_data_dir/$local_result_fetching_log" 2>&1 &

echo "Waiting for deployment process and result fetching to finish."
echo "For progress on experiment result fetching, see $exp_data_dir/$local_result_fetching_log."
wait

# --------------------------------------------------------------------
# 8) Cancel cloud instances if configured
# --------------------------------------------------------------------

if $cancel_instances; then
  scripts/cancel-cloud-instances.sh "$exp_data_dir/$instance_info_file_name"
else
  echo -e "Do not forget to cancel the used virtual servers using\n\n    scripts/cancel-cloud-instances.sh $exp_data_dir/$instance_info_file_name \n"
fi

