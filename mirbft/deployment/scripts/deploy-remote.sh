#!/bin/bash

# scripts/deploy-remote.sh
#
# Remote deployment using instance-info (e.g., Emulab/cluster).
#
# Fluxo:
#   1) Descobre IP do master a partir do instance-info.
#   2) Gera master-commands.{cmd,template} dentro de $exp_data_dir.
#   3) Limpa execuções anteriores nos nós remotos.
#   4) Sobe o master remoto.
#   5) Dispara peers (tag=peers) e clients (tag=1client).
#   6) Delega ao topo do deploy.sh a geração do resumo final.

set -euo pipefail

this_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
deployment_dir="$(cd "$this_dir/.." && pwd)"

############################################
# Helper: resolve instance-info path
############################################

resolve_instance_info() {
  local base_dir="$1"   # e.g. $exp_data_dir
  local info_arg="$2"   # e.g. scripts/instance-info

  # Caminho absoluto?
  if [[ "$info_arg" = /* ]] && [[ -f "$info_arg" ]]; then
    echo "$info_arg"
    return 0
  fi

  # Tenta relativo ao repo root e ao deployment dir
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

  # Tenta como está (relativo ao CWD)
  if [[ -f "$info_arg" ]]; then
    echo "$info_arg"
    return 0
  fi

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
while read -r tag ctrl_ip data_ip role itag; do
  [[ -z "$tag" ]] && continue
  [[ "$tag" =~ ^[[:space:]]*# ]] && continue

  # Heurística: linha que representa o master
  if [[ "$role" == "master" || "$tag" == "master" || "$tag" == "-1" ]]; then
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
# 2) Generate master-commands template and script
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
  "$exp_data_dir/$local_master_command_template" \
  "$exp_data_dir"
rc=$?

echo "initialize-deployment.sh: generate-master-commands.py exit code = $rc"
if [[ "$rc" -ne 0 ]]; then
  echo "ERROR: generate-master-commands.py failed with exit code $rc"
  exit 1
fi

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
# 3) Kill previous runs and prune state on all machines
# --------------------------------------------------------------------

echo "[STEP 2] Cleaning up previous experiment state on all nodes..."

# 3a) Mata tails, scripts de fetch e processos de ordenação / discovery
while read -r instance_id ctrl_ip data_ip role itag; do
  [[ -z "$instance_id" ]] && continue
  [[ "$instance_id" =~ ^[[:space:]]*# ]] && continue

  echo "  - Cleanup (phase 1) on $ctrl_ip ..."
  ssh $ssh_options "$ctrl_ip" " \
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
  "
done < "$instance_info_file"

echo "Killed continuous analysis scripts."
echo

# 3b) Remove traffic shaping + arquivos de experimento anterior
while read -r instance_id ctrl_ip data_ip role itag; do
  [[ -z "$instance_id" ]] && continue
  [[ "$instance_id" =~ ^[[:space:]]*# ]] && continue

  echo "  - Cleanup (phase 2) on $ctrl_ip ..."
  ssh $ssh_options "$ctrl_ip" " \
    tc qdisc del dev eth0 root tbf rate 1gbit burst 320kbit latency 400ms 2>/dev/null || true; \
    killall -9 discoverymaster discoveryslave orderingpeer orderingclient scp rsync 2>/dev/null || true; \
    rm -rf $remote_delete_files 2>/dev/null || true; \
  "
done < "$instance_info_file"

echo
echo "[STEP 2] Reset machine state: OK."
echo

# --------------------------------------------------------------------
# 4) Start master
# --------------------------------------------------------------------

echo "[STEP 3] Starting master on $master_ip."
scripts/start-master.sh "$exp_data_dir" "$master_ip"

echo "[STEP 3] Master start script finished."
echo

# --------------------------------------------------------------------
# 5) Start peer and client slaves
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
# 6) Wait for result fetching (feito por outro script) e finalizar
# --------------------------------------------------------------------

echo "[STEP 5] Waiting for deployment process and result fetching to finish."
echo "For progress on experiment result fetching, see $local_result_fetching_log."
echo "Do not forget to cancel the used virtual servers using"
echo
echo "    scripts/cancel-cloud-instances.sh $exp_data_dir/cloud-instance-info"
echo

echo "Done. Experiment data directory: $exp_data_dir"

