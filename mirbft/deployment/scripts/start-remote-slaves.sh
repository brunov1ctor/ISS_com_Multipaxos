#!/bin/bash
#
# start-remote-slaves.sh
#
# Uso:
#   scripts/start-remote-slaves.sh <exp_data_dir> <ignored_num> <tag> <instance_info_file>
#
set -euo pipefail

this_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
deployment_dir="$(cd "${this_dir}/.." && pwd)"
repo_dir="$(cd "${deployment_dir}/.." && pwd)"

# global-vars pode definir: ssh_options, remote_user, remote_work_dir, etc.
if [[ -f "${this_dir}/global-vars.sh" ]]; then
  # shellcheck source=/dev/null
  source "${this_dir}/global-vars.sh"
fi

if [[ -z "${ssh_options:-}" ]]; then
  ssh_options="-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR"
fi

if [[ $# -ne 4 ]]; then
  echo "Uso: $0 <exp_data_dir> <ignored_num> <tag> <instance_info_file>" >&2
  exit 1
fi

exp_data_dir_arg="$1"
ignored_num="$2"   # compat, não usado
wanted_tag="$3"
instance_info_arg="$4"

resolve_exp_dir() {
  local exp_arg="$1"
  if [[ "$exp_arg" = /* && -d "$exp_arg" ]]; then
    echo "$exp_arg"; return 0
  fi
  local cand1="${deployment_dir}/${exp_arg}"
  local cand2="${repo_dir}/${exp_arg}"
  [[ -d "$cand1" ]] && { echo "$cand1"; return 0; }
  [[ -d "$cand2" ]] && { echo "$cand2"; return 0; }
  return 1
}

resolve_instance_info() {
  local info_arg="$1"
  if [[ "$info_arg" = /* && -f "$info_arg" ]]; then
    echo "$info_arg"; return 0
  fi
  local cand1="${deployment_dir}/${info_arg}"
  local cand2="${repo_dir}/${info_arg}"
  [[ -f "$cand1" ]] && { echo "$cand1"; return 0; }
  [[ -f "$cand2" ]] && { echo "$cand2"; return 0; }
  [[ -f "$info_arg" ]] && { echo "$info_arg"; return 0; }
  return 1
}

if ! exp_data_dir="$(resolve_exp_dir "$exp_data_dir_arg")"; then
  echo "[ERRO  ][$(date +"%Y-%m-%d %H:%M:%S")] exp_data_dir não encontrado: '$exp_data_dir_arg'" >&2
  exit 1
fi

if ! instance_info_file="$(resolve_instance_info "$instance_info_arg")"; then
  echo "[ERRO  ][$(date +"%Y-%m-%d %H:%M:%S")] instance-info não encontrado: '$instance_info_arg'" >&2
  exit 1
fi

remote_user="${remote_user:-${DEPL_REMOTE_USER:-$USER}}"
remote_gopath="${remote_gopath:-/users/${remote_user}/go}"
remote_bin_dir="${remote_bin_dir:-${remote_gopath}/bin}"
remote_work_dir="${remote_work_dir:-/users/${remote_user}/iss}"
remote_exp_dir="${remote_exp_dir:-${remote_work_dir}/current-deployment-data}"

# local bin dir correto (onde você compila no node-0)
local_bin_dir="${LOCAL_BIN_DIR:-${GOBIN:-${GOPATH:-$HOME/go}/bin}}"

scp_retries="${SCP_RETRIES:-10}"

echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] ==== [start-remote-slaves] Contexto ====="
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   exp_data_dir       = ${exp_data_dir}"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   instance_info_file = ${instance_info_file}"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   wanted_tag         = ${wanted_tag}"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   remote_user        = ${remote_user}"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   remote_work_dir    = ${remote_work_dir}"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   remote_bin_dir     = ${remote_bin_dir}"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   remote_exp_dir     = ${remote_exp_dir}"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   local_bin_dir      = ${local_bin_dir}"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] "

# Descobre master_ip
master_ip="$(awk 'NF>=4 && $4=="master" {print $2; exit}' "${instance_info_file}" 2>/dev/null || true)"
if [[ -z "${master_ip:-}" ]]; then
  echo "[ERRO  ][$(date +"%Y-%m-%d %H:%M:%S")] Não consegui detectar master_ip em ${instance_info_file}" >&2
  exit 1
fi
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] [start-remote-slaves] master_ip = ${master_ip}"

copy_required_assets() {
  local ctrl_ip="$1"

  # diretórios remotos
  ssh ${ssh_options} "${remote_user}@${ctrl_ip}" \
    "mkdir -p '${remote_work_dir}/scripts' '${remote_work_dir}/logs' '${remote_exp_dir}' '${remote_bin_dir}'" \
    </dev/null || true

  # copia scripts (ARQUIVO A ARQUIVO — evita cair em subdir)
  bash "${this_dir}/stubborn-scp.sh" "${scp_retries}" \
    "${this_dir}/start-slave.sh" \
    "${remote_user}@${ctrl_ip}:${remote_work_dir}/scripts/start-slave.sh"

  bash "${this_dir}/stubborn-scp.sh" "${scp_retries}" \
    "${this_dir}/stubborn-scp.sh" \
    "${remote_user}@${ctrl_ip}:${remote_work_dir}/scripts/stubborn-scp.sh"

  # copia binários (se existirem localmente)
  for bin in discoverymaster discoveryslave orderingpeer orderingclient; do
    if [[ -x "${local_bin_dir}/${bin}" ]]; then
      echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] [copy] ${ctrl_ip}: enviando binário ${bin}"
      bash "${this_dir}/stubborn-scp.sh" "${scp_retries}" \
        "${local_bin_dir}/${bin}" \
        "${remote_user}@${ctrl_ip}:${remote_bin_dir}/${bin}"
    else
      echo "[WARN  ][$(date +"%Y-%m-%d %H:%M:%S")] [copy] binário NÃO encontrado localmente: ${local_bin_dir}/${bin}"
    fi
  done

  # sanity check remoto
  ssh ${ssh_options} "${remote_user}@${ctrl_ip}" \
    "ls -la '${remote_work_dir}/scripts' && ls -la '${remote_bin_dir}' | head -n 50" \
    </dev/null || true
}

start_remote_slave() {
  local instance_id="$1"
  local ctrl_ip="$2"
  local data_ip="$3"
  local tag="$4"

  echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] [start-remote-slaves] Iniciando ${instance_id} (${tag}) @ ${ctrl_ip}"

  copy_required_assets "${ctrl_ip}"

  # garante executável + roda
  ssh ${ssh_options} "${remote_user}@${ctrl_ip}" " \
    chmod +x '${remote_work_dir}/scripts/start-slave.sh' '${remote_work_dir}/scripts/stubborn-scp.sh' || true; \
    if [[ ! -f '${remote_work_dir}/scripts/start-slave.sh' ]]; then \
      echo 'ERRO: start-slave.sh não existe em ${remote_work_dir}/scripts'; exit 2; \
    fi; \
    cd '${remote_work_dir}/scripts' && \
    /usr/bin/nohup bash ./start-slave.sh \
      '${tag}' \
      '${master_ip}' \
      '${ctrl_ip}' \
      '${data_ip}' \
      '${remote_exp_dir}' \
      > '${remote_work_dir}/logs/slave-${instance_id}.log' 2>&1 & \
  " </dev/null >/dev/null 2>&1 || true
}

total=0
matched=0

while read -r instance_id ctrl_ip data_ip role tag; do
  ((total++)) || true
  [[ -z "${instance_id:-}" ]] && continue
  [[ "${instance_id}" =~ ^# ]] && continue

  # Só inicia os da tag pedida
  if [[ "${tag}" != "${wanted_tag}" ]]; then
    continue
  fi

  ((matched++)) || true
  start_remote_slave "${instance_id}" "${ctrl_ip}" "${data_ip}" "${tag}"
done < "${instance_info_file}"

echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] [start-remote-slaves] Linhas processadas: ${total}, matches(tag=${wanted_tag}): ${matched}"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] ==== [start-remote-slaves] FIM ===="

