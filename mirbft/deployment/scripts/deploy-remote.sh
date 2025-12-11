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
#
# This script performs:
#   1) Determine master IP from instance-info.
#   2) Generate master-commands template and final master-commands.cmd
#      *inside* $exp_data_dir.
#   3) Kill any previous runs, prune state on all machines.
#   4) Reset machine state (traffic shaping, processes, files).
#   5) Start master remotely (using start-master.sh).
#   6) Start slaves remotely (peers and clients).
#   7) Wait for finish, fetch results. (Resumo final é chamado fora.)

set -euo pipefail

this_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
deployment_dir="$(cd "$this_dir/.." && pwd)"

############################################
# Helper: resolve instance-info path
############################################

resolve_instance_info() {
  local base_dir="$1"   # e.g. $exp_data_dir
  local info_arg="$2"   # e.g. scripts/instance-info

  # If it's an absolute path that exists, use it:
  if [[ "$info_arg" = /* ]] && [[ -f "$info_arg" ]]; then
    echo "$info_arg"
    return 0
  fi

  # Try relative to repo root and deployment dir
  local repo_dir
  repo_dir="$(cd "$deployment_dir/.." && pwd)"
  local cand1="$repo_dir/$info_arg"
  local cand2="$deployment_dir/$info_arg"

  if [[ -f "$cand1" ]]; then
    echo "$cand1"
    return 0
  fi
  if [[ -f "$cand2" ]]; then
    echo "$cand2"
    return 0
  fi

  # Finally, try "as is" (relative to current dir):
  if [[ -f "$info_arg" ]]; then
    echo "$info_arg"
    return 0
  fi

  # Could not resolve:
  return 1
}

############################################
# 1) Determine master IP from instance-info
############################################

# exp_data_dir is already set by initialize-deployment.sh
# instance_info_file is relative to the repository or absolute
deployment_file="$exp_data_dir/deployment.dpl"

if [[ ! -f "$deployment_file" ]]; then
  echo "ERROR: Deployment file not found: $deployment_file"
  exit 1
fi

if ! instance_info_file="$(resolve_instance_info "$exp_data_dir" "$instance_info_file")"; then
  echo "ERROR: Could not find instance-info file '$instance_info_file'."
  exit 1
fi

# Grab the first instance marked as "master" or with tag == -1/1client
master_ip=""
while read -r tag ctrl_ip data_ip role itag; do
  [[ -z "$tag" ]] && continue
  [[ "$tag" =~ ^[[:space:]]*# ]] && continue

  # Heuristic: role "master" OR tag "-1" (for master) or tag "master"
  if [[ "$role" == "master" || "$tag" == "master" || "$tag" == "-1" ]]; then
    master_ip="$ctrl_ip"
    break
  fi
done < "$instance_info_file"

if [[ -z "$master_ip" ]]; then
  echo "ERROR: Could not determine master IP from instance-info '$instance_info_file'."
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
  "$exp_data_dir/$local_master_command_template" \
  "$exp_data_dir"
rc=$?

echo "initialize-deployment.sh: generate-master-commands.py exit code = $rc"
if [[ "$rc" -ne 0 ]]; then
  echo "ERROR: generate-master-commands.py failed with exit code $rc"
  exit 1
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

echo "Limpando processos antigos e removendo possíveis limitações de banda nas máquinas remotas..."

# 4a) Kill any tail/fetch scripts, kill discovery/ordering, remove traffic shaping
while read -r instance_id ctrl_ip data_ip role itag; do
  [[ -z "$instance_id" ]] && continue
  [[ "$instance_id" =~ ^[[:space:]]*# ]] && continue

  echo "  - [reset-proc] $ctrl_ip: matando processos antigos e removendo traffic shaping..."
  if ssh $ssh_options "$ctrl_ip" " \
    pkill -f 'tail -F' 2>/dev/null || true; \
    pkill -f 'fetch-results.sh' 2>/dev/null || true; \
    pkill -f 'start-slave.sh' 2>/dev/null || true; \
    pkill -f discoverymaster 2>/dev/null || true; \
    pkill -f discoveryslave 2>/dev/null || true; \
    pkill -f orderingpeer 2>/dev/null || true; \
    pkill -f orderingclient 2>/dev/null || true; \
    pkill -f 'scp ' 2>/dev/null || true; \
    pkill -f rsync 2>/dev/null || true; \
    tc qdisc del dev eth0 root tbf rate 1gbit burst 320kbit latency 400ms 2>/dev/null || true; \
  "; then
    echo "  - [reset-proc] $ctrl_ip: OK."
  else
    rc=$?
    echo "  - [reset-proc] $ctrl_ip: WARNING (ssh exit $rc). Continuando mesmo assim."
  fi
done < "$instance_info_file"

echo "Killed continuous analysis scripts."
echo

# 4b) Reset machine state (processes, files, bandwidth) – mais verboso e tolerante a erro
while read -r instance_id ctrl_ip data_ip role itag; do
  [[ -z "$instance_id" ]] && continue
  [[ "$instance_id" =~ ^[[:space:]]*# ]] && continue

  echo "  - [reset-state] $ctrl_ip: removendo shaping, matando processos do experimento e limpando arquivos antigos..."
  if ssh $ssh_options "$ctrl_ip" " \
    # Remove traffic shaping (ignore errors).
    tc qdisc del dev eth0 root tbf rate 1gbit burst 320kbit latency 400ms 2>/dev/null || true; \
    # Kill previous experiment processes (ignore if not running).
    killall -9 discoverymaster discoveryslave orderingpeer orderingclient scp rsync 2>/dev/null || true; \
    # Remove old experiment-related files.
    rm -rf $remote_delete_files 2>/dev/null || true; \
  "; then
    echo "  - [reset-state] $ctrl_ip: OK."
  else
    rc=$?
    echo "  - [reset-state] $ctrl_ip: WARNING (ssh exit $rc). Continuando mesmo assim."
  fi
done < "$instance_info_file"

echo
echo "Estado das máquinas remotas resetado."
echo

# --------------------------------------------------------------------
# 5) Start master
# --------------------------------------------------------------------

echo "Starting master on $master_ip."
scripts/start-master.sh "$exp_data_dir" "$master_ip"

# The master is now responsible for starting discovery,
# generating experiment configs, starting peers/clients, etc.

# --------------------------------------------------------------------
# 6) Start peer and client slaves
# --------------------------------------------------------------------

# Peers: tag "peers"
peers_tag="peers"
echo "Starting peer slaves (tag=$peers_tag)."
scripts/start-remote-slaves.sh "$exp_data_dir" 5 "$peers_tag" "$instance_info_file"

# Clients: tag "1client" (single client per experiment)
clients_tag="1client"
echo "Starting client slaves (tag=$clients_tag)."
scripts/start-remote-slaves.sh "$exp_data_dir" 1 "$clients_tag" "$instance_info_file"

echo "All slaves started. waiting for them to finish."
echo "Remote slave deployment finished."
echo

# --------------------------------------------------------------------
# 7) Wait for result fetching and (external) summarization
# --------------------------------------------------------------------

echo "Waiting for deployment process and result fetching to finish."
echo "For progress on experiment result fetching, see $local_result_fetching_log."
echo "Do not forget to cancel the used virtual servers using"
echo
echo "    scripts/cancel-cloud-instances.sh $exp_data_dir/cloud-instance-info"
echo
echo "Done. Experiment data directory: $exp_data_dir"

