#!/usr/bin/env bash
set -euo pipefail

# -----------------------------------------------------------------------------
# Original-like deploy-remote.sh (mínimas mudanças, sem robustez extra)
# -----------------------------------------------------------------------------

cancel_instances="${cancel_instances:-false}"

if [[ -z "${exp_data_dir:-}" || -z "${instance_info_file:-}" ]]; then
  >&2 echo "deploy-remote.sh: exp_data_dir ou instance_info_file não definidos."
  >&2 echo "exp_data_dir='${exp_data_dir:-}' instance_info_file='${instance_info_file:-}'"
  exit 1
fi

if command -v realpath >/dev/null 2>&1; then
  instance_info_file="$(realpath "${instance_info_file}")"
  exp_data_dir="$(realpath "${exp_data_dir}")"
fi

instance_info_file_name="$(basename "$instance_info_file")"

local_master_command_template_file="${local_master_command_template_file:-master-commands-template.cmd}"
local_master_command_file="${local_master_command_file:-master-commands.cmd}"
local_result_fetching_log="${local_result_fetching_log:-result-fetching.log}"

remote_user="${remote_user:-${USER}}"
remote_work_dir="${remote_work_dir:-/tmp/iss-${remote_user}}"
remote_exp_dir="${remote_exp_dir:-${remote_work_dir}}"

# Diretório leve (configs/TLS) permanece em /users/<user>/iss.
remote_base_dir="${remote_base_dir:-/users/${remote_user}/iss}"
remote_bin_dir="${remote_bin_dir:-/users/${remote_user}/go/bin}"

ssh_options="${ssh_options:--o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -T -o BatchMode=yes -o ConnectTimeout=8 -o ConnectionAttempts=1 -o ServerAliveInterval=5 -o ServerAliveCountMax=2 -o LogLevel=ERROR -o ControlMaster=no -o ControlPath=none -o ControlPersist=no}"
scp_options="${scp_options:--o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o BatchMode=yes -o ConnectTimeout=8 -o ConnectionAttempts=1 -o ServerAliveInterval=5 -o ServerAliveCountMax=2 -o LogLevel=ERROR -o ControlMaster=no -o ControlPath=none -o ControlPersist=no}"

remote_status_file="${remote_status_file:-${remote_work_dir}/status}"
DISC_PORT="${master_port:-${MASTER_PORT:-9999}}"

remote_experiment_output_dir="${remote_experiment_output_dir:-${remote_work_dir}/experiment-output}"
published_root="${published_root:-${PUBLISHED_ROOT:-${exp_data_dir}/published/experiment-output}}"

master_ip="$(awk 'NF>=4 && $4=="master"{print $2; exit}' "$instance_info_file" || true)"
if [[ -z "${master_ip}" ]]; then
  >&2 echo "deploy-remote.sh: could not obtain master ip from instance info file: $instance_info_file"
  exit 1
fi

cp -f "$instance_info_file" "$exp_data_dir/$instance_info_file_name"
echo "Using instance info file: $instance_info_file"
echo "       Master IP address: $master_ip"

# -----------------------------------------------------------------------------
# TLS (mantém o que você já tinha, mas sem logs novos)
# -----------------------------------------------------------------------------
tls_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/tls-data"
if [[ ! -d "${tls_dir}" ]]; then
  >&2 echo "deploy-remote.sh: tls-data not found: ${tls_dir}"
  exit 1
fi

mapfile -t all_ips < <(
  awk '{print $2"\n"$3}' "$instance_info_file" \
  | grep -Eo '([0-9]{1,3}\.){3}[0-9]{1,3}' \
  | sort -u
)

if [[ "${#all_ips[@]}" -eq 0 ]]; then
  >&2 echo "deploy-remote.sh: could not extract IPs from instance-info: $instance_info_file"
  exit 1
fi

(
  cd "${tls_dir}"
  [[ -x "./generate-auth.sh" ]] || { >&2 echo "deploy-remote.sh: generate-auth.sh not executable in ${tls_dir}"; exit 1; }
  ./generate-auth.sh "${all_ips[@]}"
)

# -----------------------------------------------------------------------------
# Generate master-commands-template.cmd (se não existir) + master-commands.cmd
# -----------------------------------------------------------------------------
template_path="$exp_data_dir/$local_master_command_template_file"
deployment_file="$exp_data_dir/deployment.dpl"

if [[ ! -f "$template_path" ]]; then
  [[ -f "$deployment_file" ]] || { >&2 echo "deploy-remote.sh: Deployment file não encontrado: $deployment_file"; exit 1; }

  # (mantém seu FIX de bin path, mas sem mudar a lógica)
  ISS_REMOTE_USER="${remote_user}" \
  ISS_REMOTE_BIN_DIR="${remote_bin_dir}" \
  python3 scripts/generate-master-commands.py remote "$deployment_file" "$template_path" "$exp_data_dir" \
    || { >&2 echo "deploy-remote.sh: Falha ao gerar master-commands-template.cmd"; exit 1; }
fi

export ssh_key_file="${remote_private_key_file:-}"
export own_public_ip="$master_ip"
export master_port="${DISC_PORT}"
export status_file="$remote_status_file"
export ready_file="${remote_ready_file:-${remote_work_dir}/master-ready}"

envsubst '$ssh_key_file $own_public_ip $master_port $status_file $ready_file' \
  < "$template_path" > "$exp_data_dir/$local_master_command_file"

echo -e "\nwrite-file $status_file DONE" >> "$exp_data_dir/$local_master_command_file"

# -----------------------------------------------------------------------------
# Kill everything and prune state (original-like)
# -----------------------------------------------------------------------------
echo "Killing everything that is alive and pruning state on the remote machines (including SSH) and removing potential bandwidth limit."

for ip in $(awk '{print $2}' "$instance_info_file"); do
  ssh $ssh_options root@$ip "kill -9 \$(ps -ef | grep 'analyze-continuously' | grep -v \$\$ | awk '{print \$2}')" &
  sleep 0.1
done
wait

echo -e "\nKilled continuous analysis scripts.\n"

# Mantém o layout que você precisa (/tmp e /users), mas no estilo do original (um SSH root com bloco)
for ip in $(awk '{print $2}' "$instance_info_file"); do
  ssh $ssh_options root@$ip "
    tc qdisc del dev eth0 root tbf rate 1gbit burst 320kbit latency 400ms 2>/dev/null || true
    killall -9 discoverymaster discoveryslave orderingpeer orderingclient scp rsync 2>/dev/null || true

    rm -rf '${remote_work_dir}' 2>/dev/null || true
    mkdir -p '${remote_work_dir}' '${remote_work_dir}/logs' '${remote_work_dir}/scripts' '${remote_work_dir}/experiment-output' '${remote_work_dir}/raw-results' 2>/dev/null || true

    mkdir -p '${remote_base_dir}' '${remote_base_dir}/experiment-config' '${remote_base_dir}/config' '${remote_base_dir}/tls-data' 2>/dev/null || true

    echo RUNNING > '${remote_status_file}' 2>/dev/null || true

    kill -9 \$(ps -ef | grep 'sshd: root@notty' | awk '{print \$2}') 2>/dev/null || true
  " &
  sleep 0.1
done
wait

echo -e "\n Reset machine state.\n"

# -----------------------------------------------------------------------------
# Start master + slaves + fetch results (original-like)
# -----------------------------------------------------------------------------
scripts/start-master.sh \
  "$remote_user" \
  "$master_ip" \
  "$remote_work_dir" \
  "$remote_bin_dir" \
  "$exp_data_dir" \
  "$exp_data_dir/$local_master_command_file" &

scripts/start-remote-slaves.sh "$exp_data_dir" 0 peers "$instance_info_file" &
scripts/start-remote-slaves.sh "$exp_data_dir" 0 1client "$instance_info_file" &

scripts/fetch-results.sh "$master_ip" "$exp_data_dir" > "$exp_data_dir/$local_result_fetching_log" 2>&1 &

echo "Waiting for deployment process and result fetching to finish."
echo "For progress on experiment result fetching, see $exp_data_dir/$local_result_fetching_log."
wait

# -----------------------------------------------------------------------------
# Cancel instances
# -----------------------------------------------------------------------------
if $cancel_instances; then
  scripts/cancel-cloud-instances.sh "$exp_data_dir/$instance_info_file_name"
else
  echo -e "Do not forget to cancel the used virtual servers using\n\n    scripts/cancel-cloud-instances.sh $exp_data_dir/$instance_info_file_name \n"
fi

exit 0

