#!/bin/bash
#
# start-remote-slaves.sh
#
# Uso:
#   scripts/start-remote-slaves.sh <exp_data_dir> <ignored_num> <tag> <instance_info_file>
#
set -euo pipefail

this_dir="$(cd "$(dirname "$0")" && pwd)"
deployment_dir="$(cd "${this_dir}/.." && pwd)"
repo_dir="$(cd "${deployment_dir}/.." && pwd)"

source "${deployment_dir}/scripts/global-vars.sh"

if [[ $# -lt 4 ]]; then
  echo "Uso: $0 <exp_data_dir> <ignored_num> <tag> <instance_info_file>" >&2
  exit 1
fi

exp_data_dir="$1"
ignored_num="$2"
wanted_tag="$3"
wanted_tag="${wanted_tag//$'\r'/}"
wanted_tag="$(echo "$wanted_tag" | xargs)"
instance_info_file="$4"

master_ip="$(awk 'NF>=4 && $4=="master" {print $2; exit}' "${instance_info_file}" 2>/dev/null || true)"
if [[ -z "${master_ip:-}" ]]; then
  echo "[ERRO  ][$(date +"%Y-%m-%d %H:%M:%S")] [start-remote-slaves] Não consegui detectar master_ip em ${instance_info_file}" >&2
  exit 1
fi

remote_user="${remote_user:-$USER}"
remote_gopath="${remote_gopath:-${GOPATH:-$HOME/go}}"
remote_bin_dir="${remote_bin_dir:-${GOBIN:-${remote_gopath}/bin}}"
remote_work_dir="${remote_work_dir:-$HOME/iss}"
remote_exp_dir="${remote_exp_dir:-${remote_work_dir}/current-deployment-data}"

tmp_scripts_dir="${exp_data_dir}/tmp-scripts"
mkdir -p "${tmp_scripts_dir}"

cp -f "${this_dir}/start-slave.sh" "${tmp_scripts_dir}/"
cp -f "${this_dir}/stubborn-scp.sh" "${tmp_scripts_dir}/"

echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] ==== [start-remote-slaves] Diretórios detectados ====="
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   this_dir           = ${this_dir}"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   deployment_dir     = ${deployment_dir}"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   repo_dir           = ${repo_dir}"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   exp_data_dir       = ${exp_data_dir}"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   instance_info_file = ${instance_info_file}"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   wanted_tag        = ${wanted_tag}"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   remote_user        = ${remote_user}"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   remote_gopath      = ${remote_gopath}"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   remote_bin_dir     = ${remote_bin_dir}"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   remote_work_dir    = ${remote_work_dir}"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   remote_exp_dir     = ${remote_exp_dir}"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] "
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] [start-remote-slaves] master_ip detectado = ${master_ip}"

scp_retries="${scp_retries:-10}"
total_lines=0
matched=0

start_instance_line() {
  local line="$1"
  line="${line%$'\r'}"

  local instance_id ctrl_ip data_ip role tag
  read -r instance_id ctrl_ip data_ip role tag <<< "$line"

  instance_id="${instance_id%$'\r'}"
  ctrl_ip="${ctrl_ip%$'\r'}"
  data_ip="${data_ip%$'\r'}"
  role="${role%$'\r'}"
  tag="${tag%$'\r'}"
  tag="$(echo "$tag" | xargs)"

  [[ -z "${instance_id:-}" ]] && return 0
  [[ "${instance_id}" =~ ^# ]] && return 0

  if [[ "${tag}" != "${wanted_tag}" ]]; then
    if [[ "${DEBUG_START_REMOTE_SLAVES:-0}" == "1" ]]; then
      echo "[DEBUG ][$(date +"%Y-%m-%d %H:%M:%S")] Skip ${instance_id} tag='${tag}' (wanted='${wanted_tag}')"
    fi
    return 0
  fi

  echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] [start-remote-slaves] Iniciando slave '${instance_id}' (${tag}) em ${ctrl_ip}..."
  matched=$((matched+1))

  # CRÍTICO: nunca deixe ssh ler o STDIN do loop (consome o instance-info)
  ssh ${ssh_options} "${remote_user}@${ctrl_ip}" \
    "mkdir -p '${remote_work_dir}/scripts' '${remote_work_dir}/logs' '${remote_exp_dir}' '${remote_bin_dir}'" \
    </dev/null || true

  bash "${this_dir}/stubborn-scp.sh" \
    "${scp_retries}" \
    "${tmp_scripts_dir}/" \
    "${remote_user}@${ctrl_ip}:${remote_work_dir}/scripts/"

  local local_bin_dir="${LOCAL_BIN_DIR:-${remote_bin_dir}}"
  for bin in discoverymaster discoveryslave orderingpeer orderingclient; do
    if [[ -x "${local_bin_dir}/${bin}" ]]; then
      bash "${this_dir}/stubborn-scp.sh" \
        "${scp_retries}" \
        "${local_bin_dir}/${bin}" \
        "${remote_user}@${ctrl_ip}:${remote_bin_dir}/"
    fi
  done

  local public_ip="${ctrl_ip}"
  local private_ip="${data_ip}"

  ssh ${ssh_options} "${remote_user}@${ctrl_ip}" " \
    cd '${remote_work_dir}/scripts' && \
    /usr/bin/nohup bash ./start-slave.sh \
      '${tag}' \
      '${master_ip}' \
      '${public_ip}' \
      '${private_ip}' \
      '${remote_exp_dir}' \
      > '${remote_work_dir}/logs/slave-${instance_id}.log' 2>&1 & \
  " </dev/null >/dev/null 2>&1 || true
}

while IFS= read -r line; do
  total_lines=$((total_lines+1))
  start_instance_line "$line"
done < "${instance_info_file}"

echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] [start-remote-slaves] Linhas processadas: ${total_lines}, matches(tag=${wanted_tag}): ${matched}"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] "
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] ==== [start-remote-slaves] Todos os slaves disparados. ===="
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] ==== [start-remote-slaves] FIM ==========================================="

