#!/bin/bash
set -euo pipefail

this_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
deployment_dir="$(cd "${this_dir}/.." && pwd)"
repo_dir="$(cd "${deployment_dir}/.." && pwd)"

if [[ -f "${this_dir}/global-vars.sh" ]]; then
  # shellcheck source=/dev/null
  source "${this_dir}/global-vars.sh"
fi

# SSH SEM INTERAÇÃO (evita travas)
ssh_options="${ssh_options:-} -T -o BatchMode=yes -o ConnectTimeout=8 -o ServerAliveInterval=5 -o ServerAliveCountMax=2 -o LogLevel=ERROR"
# Se você quiser manter known_hosts real, remova as duas opções abaixo.
# Eu mantenho aqui para ambiente “reseta muito”.
ssh_options="${ssh_options} -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null"

if [[ $# -ne 4 ]]; then
  echo "Uso: $0 <exp_data_dir> <ignored_num> <tag> <instance_info_file>" >&2
  exit 1
fi

exp_data_dir_arg="$1"
ignored_num="$2"
wanted_tag="$3"
instance_info_arg="$4"

ts() { date +"%Y-%m-%d %H:%M:%S"; }
info(){ echo "[INFO  ][$(ts)] $*"; }
warn(){ echo "[WARN  ][$(ts)] $*"; }
err(){  echo "[ERRO  ][$(ts)] $*" >&2; }

resolve_exp_dir() {
  local exp_arg="$1"
  if [[ "$exp_arg" = /* && -d "$exp_arg" ]]; then echo "$exp_arg"; return 0; fi
  local cand1="${deployment_dir}/${exp_arg}"
  local cand2="${repo_dir}/${exp_arg}"
  [[ -d "$cand1" ]] && { echo "$cand1"; return 0; }
  [[ -d "$cand2" ]] && { echo "$cand2"; return 0; }
  return 1
}

resolve_instance_info() {
  local info_arg="$1"
  if [[ "$info_arg" = /* && -f "$info_arg" ]]; then echo "$info_arg"; return 0; fi
  local cand1="${deployment_dir}/${info_arg}"
  local cand2="${repo_dir}/${info_arg}"
  [[ -f "$cand1" ]] && { echo "$cand1"; return 0; }
  [[ -f "$cand2" ]] && { echo "$cand2"; return 0; }
  [[ -f "$info_arg" ]] && { echo "$info_arg"; return 0; }
  return 1
}

if ! exp_data_dir="$(resolve_exp_dir "$exp_data_dir_arg")"; then
  err "exp_data_dir não encontrado: '$exp_data_dir_arg'"
  exit 1
fi

if ! instance_info_file="$(resolve_instance_info "$instance_info_arg")"; then
  err "instance-info não encontrado: '$instance_info_arg'"
  exit 1
fi

remote_user="${remote_user:-${DEPL_REMOTE_USER:-$USER}}"
remote_gopath="${remote_gopath:-/users/${remote_user}/go}"
remote_bin_dir="${remote_bin_dir:-${remote_gopath}/bin}"
remote_work_dir="${remote_work_dir:-/users/${remote_user}/iss}"
remote_exp_dir="${remote_exp_dir:-${remote_work_dir}/current-deployment-data}"

local_bin_dir="${LOCAL_BIN_DIR:-${GOBIN:-${GOPATH:-$HOME/go}/bin}}"
scp_retries="${SCP_RETRIES:-10}"

info "==== [start-remote-slaves] Contexto ====="
info "  exp_data_dir       = ${exp_data_dir}"
info "  instance_info_file = ${instance_info_file}"
info "  wanted_tag         = ${wanted_tag}"
info "  remote_user        = ${remote_user}"
info "  remote_work_dir    = ${remote_work_dir}"
info "  remote_bin_dir     = ${remote_bin_dir}"
info "  remote_exp_dir     = ${remote_exp_dir}"
info "  local_bin_dir      = ${local_bin_dir}"
info "  ssh_options        = ${ssh_options}"
info ""

master_ip="$(awk 'NF>=4 && $4=="master" {print $2; exit}' "${instance_info_file}" 2>/dev/null || true)"
if [[ -z "${master_ip:-}" ]]; then
  err "Não consegui detectar master_ip em ${instance_info_file}"
  exit 1
fi
info "[start-remote-slaves] master_ip = ${master_ip}"

remote_mkdirs() {
  local ip="$1"
  ssh ${ssh_options} "${remote_user}@${ip}" \
    "mkdir -p '${remote_work_dir}/scripts' '${remote_work_dir}/logs' '${remote_exp_dir}' '${remote_bin_dir}'" \
    </dev/null
}

copy_required_assets() {
  local ip="$1"

  remote_mkdirs "$ip"

  # scripts
  bash "${this_dir}/stubborn-scp.sh" "${scp_retries}" \
    "${this_dir}/start-slave.sh" \
    "${remote_user}@${ip}:${remote_work_dir}/scripts/start-slave.sh"

  bash "${this_dir}/stubborn-scp.sh" "${scp_retries}" \
    "${this_dir}/stubborn-scp.sh" \
    "${remote_user}@${ip}:${remote_work_dir}/scripts/stubborn-scp.sh"

  # binários
  for bin in discoverymaster discoveryslave orderingpeer orderingclient; do
    if [[ -f "${local_bin_dir}/${bin}" ]]; then
      info "[copy] ${ip}: enviando binário ${bin}"
      bash "${this_dir}/stubborn-scp.sh" "${scp_retries}" \
        "${local_bin_dir}/${bin}" \
        "${remote_user}@${ip}:${remote_bin_dir}/${bin}"
    else
      warn "[copy] binário NÃO encontrado localmente: ${local_bin_dir}/${bin}"
    fi
  done

  # chmod + diagnóstico simples (sem loops remotos)
  ssh ${ssh_options} "${remote_user}@${ip}" "\
    chmod +x \
      '${remote_work_dir}/scripts/start-slave.sh' \
      '${remote_work_dir}/scripts/stubborn-scp.sh' \
      '${remote_bin_dir}/discoverymaster' \
      '${remote_bin_dir}/discoveryslave' \
      '${remote_bin_dir}/orderingpeer' \
      '${remote_bin_dir}/orderingclient' \
    2>/dev/null || true; \
    echo '[remote-check] scripts:'; ls -la '${remote_work_dir}/scripts' | head -n 50; \
    echo '[remote-check] bins:'; ls -la '${remote_bin_dir}' | head -n 50; \
  " </dev/null

  # validação: comando único e direto
  ssh ${ssh_options} "${remote_user}@${ip}" "\
    test -x '${remote_bin_dir}/discoverymaster' && \
    test -x '${remote_bin_dir}/discoveryslave' && \
    test -x '${remote_bin_dir}/orderingpeer' && \
    test -x '${remote_bin_dir}/orderingclient' \
  " </dev/null
}

start_remote_slave() {
  local instance_id="$1"
  local ctrl_ip="$2"
  local data_ip="$3"
  local tag="$4"

  info "[start-remote-slaves] Iniciando ${instance_id} (${tag}) @ ${ctrl_ip}"

  if ! copy_required_assets "${ctrl_ip}"; then
    err "Falha ao preparar assets no host ${ctrl_ip} (node ${instance_id})."
    return 0
  fi

  # Comando bem explícito e logável
  remote_cmd="cd '${remote_work_dir}/scripts' && \
/usr/bin/nohup bash ./start-slave.sh \
'${tag}' '${master_ip}' '${ctrl_ip}' '${data_ip}' '${remote_exp_dir}' \
> '${remote_work_dir}/logs/slave-${instance_id}.log' 2>&1 & echo STARTED"

  info "[ssh] ${ctrl_ip}: ${remote_cmd}"

  if ! ssh ${ssh_options} "${remote_user}@${ctrl_ip}" "${remote_cmd}" </dev/null; then
    err "SSH falhou ao disparar slave ${instance_id} em ${ctrl_ip}."
    return 0
  fi
}

total=0
matched=0

while read -r instance_id ctrl_ip data_ip role tag; do
  ((total++)) || true
  [[ -z "${instance_id:-}" ]] && continue
  [[ "${instance_id}" =~ ^# ]] && continue

  if [[ "${tag}" != "${wanted_tag}" ]]; then
    continue
  fi

  ((matched++)) || true
  start_remote_slave "${instance_id}" "${ctrl_ip}" "${data_ip}" "${tag}"
done < "${instance_info_file}"

info "[start-remote-slaves] Linhas processadas: ${total}, matches(tag=${wanted_tag}): ${matched}"
info "==== [start-remote-slaves] FIM ===="

