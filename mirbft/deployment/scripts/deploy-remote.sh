#!/bin/bash

# scripts/deploy-remote.sh
#
# Remote deployment using instance-info (e.g., Emulab/cluster).
#
# This script is *sourced* by deploy.sh, which has already:
#   - sourced scripts/global-vars.sh
#   - run scripts/initialize-deployment.sh
#
# We assume the following variables are available:
#   - exp_data_dir
#   - instance_info_file
#   - cancel_instances
#   - remote_private_key_file, remote_status_file, ssh_options,
#     remote_work_dir, remote_delete_files, local_result_fetching_log, etc.
#
# High-level steps:
#   [STEP 1] Determine master IP from instance-info.
#   [STEP 2] Kill previous runs and prune state on all machines.
#   [STEP 3] Generate master command script (master-commands.cmd).
#   [STEP 4] Start master remotely (using start-master.sh).
#   [STEP 5] Start peer and client slaves.
#   [STEP 6] Hand control back; deploy.sh cuida do summarize final.

set -euo pipefail

this_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
deployment_dir="$(cd "$this_dir/.." && pwd)"

############################################
# Ajustar ssh_options sem quebrar global-vars
############################################

# Se global-vars.sh tiver definido ssh_options, usamos e só acrescentamos
# opções para evitar bloqueios interativos.
ssh_options="${ssh_options:--o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null}"
# Garante que não haja prompt de senha/host (se falhar, falha direto)
ssh_options="$ssh_options -o BatchMode=yes -o ConnectTimeout=10"

############################################
# Helper: resolve instance-info path
############################################

resolve_instance_info() {
  local base_dir="$1"   # e.g. $exp_data_dir
  local info_arg="$2"   # e.g. scripts/instance-info

  # Absolute path?
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

deployment_file="$exp_data_dir/deployment.dpl"

if [[ ! -f "$deployment_file" ]]; then
  echo "ERROR: Deployment file not found: $deployment_file"
  exit 1
fi

if ! instance_info_file="$(resolve_instance_info "$exp_data_dir" "$instance_info_file")"; then
  echo "ERROR: Could not find instance-info file '$instance_info_file'."
  exit 1
fi

master_ip=""
# instance-info: instance_id ctrl_ip data_ip role tag
while read -r instance_id ctrl_ip data_ip role tag; do
  [[ -z "$instance_id" ]] && continue
  [[ "$instance_id" =~ ^[[:space:]]*# ]] && continue

  if [[ "$role" == "master" || "$tag" == "master" ]]; then
    master_ip="$ctrl_ip"
    break
  fi
done < "$instance_info_file"

if [[ -z "$master_ip" ]]; then
  echo "ERROR: Could not determine master IP from instance-info '$instance_info_file'."
  exit 1
fi

echo "[STEP 1] Using instance info file: $instance_info_file"
echo "[STEP 1] Master IP address: $master_ip"
echo

# --------------------------------------------------------------------
# 2) Kill previous runs and prune state on all machines
# --------------------------------------------------------------------

echo "[STEP 2] Cleaning up previous experiment state on all nodes..."

# 2a) Fase 1: matar analisadores contínuos / scripts antigos (se existirem)
while read -r instance_id ctrl_ip data_ip role tag; do
  [[ -z "$instance_id" ]] && continue
  [[ "$instance_id" =~ ^[[:space:]]*# ]] && continue

  echo "  - Cleanup (phase 1) on ${ctrl_ip} (${instance_id})..."
  ssh $ssh_options "$ctrl_ip" " \
    pids=\$(ps -ef | grep 'analyze-continuously' | grep -v grep | awk '{print \$2}'); \
    if [ -n \"\$pids\" ]; then kill -9 \$pids || true; fi \
  " >/dev/null 2>&1 || true
done < "$instance_info_file"

echo "  - Phase 1 cleanup done on all nodes."
echo

# 2b) Fase 2: reset de estado de experimento (processos, arquivos, shaping)
while read -r instance_id ctrl_ip data_ip role tag; do
  [[ -z "$instance_id" ]] && continue
  [[ "$instance_id" =~ ^[[:space:]]*# ]] && continue

  echo "  - Cleanup (phase 2: state prune) on ${ctrl_ip} (${instance_id})..."
  ssh $ssh_options "$ctrl_ip" " \
    # Remove traffic shaping (ignore errors).
    tc qdisc del dev eth0 root tbf rate 1gbit burst 320kbit latency 400ms 2>/dev/null || true; \
    # Kill previous experiment processes (ignore if not running).
    killall -9 discoverymaster discoveryslave orderingpeer orderingclient scp rsync 2>/dev/null || true; \
    # Remove old experiment-related files.
    rm -rf $remote_delete_files 2>/dev/null || true; \
    # Ensure status dir and reset status to RUNNING (se usado).
    mkdir -p \"\$(dirname \"$remote_status_file\")\" 2>/dev/null || true; \
    echo RUNNING > \"$remote_status_file\" 2>/dev/null || true \
  " >/dev/null 2>&1 || true
done < "$instance_info_file"

echo
echo "[STEP 2] Remote machine state reset complete."
echo

# --------------------------------------------------------------------
# 3) Pre-generate master command script
# --------------------------------------------------------------------

local_master_command_template="master-commands-template.cmd"
local_master_command_file="master-commands.cmd"

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

if [[ "$rc" -ne 0 ]]; then
  echo "initialize-deployment.sh: failed processing deployment file: $deployment_file"
  # Para remoto, ainda continuamos; o schedule pode estar vazio.
fi

# --------------------------------------------------------------------
# 4) Prepare master-commands.cmd locally, substituting placeholders
# --------------------------------------------------------------------

export ssh_key_file="${remote_private_key_file:-}"
export own_public_ip="$master_ip"
export master_port
export status_file="$remote_status_file"

local_template_path="$exp_data_dir/$local_master_command_template"
local_cmd_path="$exp_data_dir/$local_master_command_file"

if [[ ! -f "$local_template_path" ]]; then
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
# 5) Start master
# --------------------------------------------------------------------

echo "[STEP 3] Starting master on $master_ip."
scripts/start-master.sh "$exp_data_dir" "$master_ip"
echo "[STEP 3] Master start sequence finished."
echo

# --------------------------------------------------------------------
# 6) Start peer and client slaves
# --------------------------------------------------------------------

peers_tag="peers"
echo "[STEP 4] Starting peer slaves (tag=$peers_tag)."
scripts/start-remote-slaves.sh "$exp_data_dir" 5 "$peers_tag" "$instance_info_file"

clients_tag="1client"
echo "[STEP 4] Starting client slaves (tag=$clients_tag)."
scripts/start-remote-slaves.sh "$exp_data_dir" 1 "$clients_tag" "$instance_info_file"

echo "[STEP 4] All slaves started. Waiting for them to finish."
echo "Remote slave deployment finished."
echo

# --------------------------------------------------------------------
# 7) Final message (deploy.sh cuidará do summarize)
# --------------------------------------------------------------------

echo "[STEP 5] Waiting for deployment process and result fetching to finish."
echo "For progress on experiment result fetching, see $local_result_fetching_log."
echo "Do not forget to cancel the used virtual servers using"
echo
echo "    scripts/cancel-cloud-instances.sh $exp_data_dir/cloud-instance-info"
echo
echo "Done. Experiment data directory: $exp_data_dir"

