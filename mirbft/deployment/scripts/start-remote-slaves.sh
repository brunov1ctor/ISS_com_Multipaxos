#!/usr/bin/env bash

#
# start-remote-slaves.sh
#
# Uso:
#   start-remote-slaves.sh <exp_data_dir> <desired_count> <wanted_tag> <instance_info_file>
#
# - Lê o instance-info (id ctrl_ip data_ip role tag)
# - Inicia APENAS 'desired_count' instâncias com tag = wanted_tag
# - Copia scripts, binários e TLS para o remoto (atomic)
# - Dispara start-slave.sh via nohup (não pode travar)
#

set -euo pipefail

ts() { date +"%Y-%m-%d %H:%M:%S%z"; }
info(){ echo "[INFO  ][$(ts)] $*"; }
warn(){ echo "[WARN  ][$(ts)] $*"; }
err(){  echo "[ERRO  ][$(ts)] $*" >&2; }

if [[ $# -lt 4 ]]; then
  echo "Uso: $0 <exp_data_dir> <desired_count> <wanted_tag> <instance_info_file>" >&2
  exit 2
fi

exp_data_dir="$1"
desired_count="$2"
wanted_tag="$3"
instance_info_file="$4"

this_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Usuário remoto (cai para REMOTE_USER ou USER se nada for definido)
remote_user="${remote_user:-${REMOTE_USER:-${USER}}}"

# Opções SSH padrão (coerente com deploy-remote.sh)
if [[ -z "${ssh_options:-}" ]]; then
  ssh_options="-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
-T -o BatchMode=yes -o ConnectTimeout=8 -o ConnectionAttempts=1 \
-o ServerAliveInterval=5 -o ServerAliveCountMax=2 -o LogLevel=ERROR \
-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
-o ControlMaster=no -o ControlPath=none -o ControlPersist=no"
fi

SSH_START_TIMEOUT="${SSH_START_TIMEOUT:-12s}"

# Diretórios remotos padrão (podem ser sobrescritos via ambiente/global-vars)
remote_work_dir="${remote_work_dir:-/users/${remote_user}/iss}"
remote_bin_dir="${remote_bin_dir:-/users/${remote_user}/go/bin}"
local_bin_dir="${local_bin_dir:-${GOBIN:-${HOME}/go/bin}}"      # onde discovery*/ordering* estão instalados
remote_exp_dir="${remote_exp_dir:-${remote_work_dir}/current-deployment-data}"

scp_retries="${scp_retries:-10}"

# TLS local (seguindo deploy-local.sh: cp -r tls-data $exp_data_dir)
# Aqui usamos tls-data que fica em deployment/, não o da raiz do repo.
local_tls_dir="$(cd "${this_dir}/.." && pwd)/tls-data"

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
info "  SSH_START_TIMEOUT  = ${SSH_START_TIMEOUT}"
info ""

remote_mkdirs() {
  local ip="$1"
  ssh ${ssh_options} "${remote_user}@${ip}" "\
    mkdir -p '${remote_work_dir}' \
             '${remote_work_dir}/scripts' \
             '${remote_work_dir}/logs' \
             '${remote_work_dir}/config' \
             '${remote_work_dir}/tls-data' \
             '${remote_exp_dir}' \
             '${remote_exp_dir}/tls-data' \
             '${remote_bin_dir}' \
    2>/dev/null || true
  " </dev/null
}

remote_kill_bins() {
  local ip="$1"
  ssh ${ssh_options} "${remote_user}@${ip}" "\
    for p in discoverymaster discoveryslave orderingpeer orderingclient; do
      pkill -9 -f '${remote_bin_dir}/'\"\$p\" 2>/dev/null || true
      pkill -9 -f '\\b'\"\$p\"'\\b' 2>/dev/null || true
    done
  " </dev/null || true
}

copy_bin_atomic() {
  local ip="$1"
  local bin="$2"

  local local_path="${local_bin_dir}/${bin}"
  local remote_path="${remote_bin_dir}/${bin}"
  local remote_tmp="${remote_bin_dir}/.${bin}.tmp"

  if [[ ! -x "${local_path}" ]]; then
    warn "[copy] binário NÃO encontrado localmente: ${local_path}"
    return 0
  fi

  ssh ${ssh_options} "${remote_user}@${ip}" "rm -f '${remote_tmp}'" </dev/null || true

  info "[copy] ${ip}: enviando binário ${bin} (atomic)"
  bash "${this_dir}/stubborn-scp.sh" "${scp_retries}" \
    "${local_path}" \
    "${remote_user}@${ip}:${remote_tmp}"

  ssh ${ssh_options} "${remote_user}@${ip}" "\
    mv -f '${remote_tmp}' '${remote_path}' && chmod +x '${remote_path}'
  " </dev/null
}

copy_tls_assets() {
  local ip="$1"

  if [[ ! -d "${local_tls_dir}" ]]; then
    warn "[copy] Diretório TLS local NÃO encontrado em ${local_tls_dir}. TLS vai falhar nos slaves."
    return 0
  fi

  # Garante destino no exp_dir (PWD dos binários) e no work_dir (para scripts que usem)
  ssh ${ssh_options} "${remote_user}@${ip}" "\
    mkdir -p '${remote_exp_dir}/tls-data' '${remote_work_dir}/tls-data' 2>/dev/null || true
  " </dev/null || true

  for f in "${local_tls_dir}"/*; do
    [[ -f "${f}" ]] || continue
    local base
    base="$(basename "${f}")"
    info "[copy] ${ip}: TLS ${base} -> ${remote_exp_dir}/tls-data/"
    bash "${this_dir}/stubborn-scp.sh" "${scp_retries}" \
      "${f}" \
      "${remote_user}@${ip}:${remote_exp_dir}/tls-data/${base}"
  done
}

remote_check_assets() {
  local ip="$1"
  ssh ${ssh_options} "${remote_user}@${ip}" "\
    echo '[remote-check] scripts:'; ls -la '${remote_work_dir}/scripts' | head -n 60; \
    echo '[remote-check] bins:'; ls -la '${remote_bin_dir}' | head -n 60; \
    echo '[remote-check] tls-data:'; ls -la '${remote_exp_dir}/tls-data' 2>/dev/null || echo '(sem tls-data no exp_dir)'; \
    test -x '${remote_bin_dir}/discoverymaster' && \
    test -x '${remote_bin_dir}/discoveryslave' && \
    test -x '${remote_bin_dir}/orderingpeer' && \
    test -x '${remote_bin_dir}/orderingclient'
  " </dev/null || true
}

copy_required_assets() {
  local ip="$1"

  remote_mkdirs "${ip}"

  # Scripts auxiliares
  bash "${this_dir}/stubborn-scp.sh" "${scp_retries}" \
    "${this_dir}/start-slave.sh" \
    "${remote_user}@${ip}:${remote_work_dir}/scripts/start-slave.sh"

  bash "${this_dir}/stubborn-scp.sh" "${scp_retries}" \
    "${this_dir}/global-vars.sh" \
    "${remote_user}@${ip}:${remote_work_dir}/scripts/global-vars.sh"

  bash "${this_dir}/stubborn-scp.sh" "${scp_retries}" \
    "${this_dir}/stubborn-scp.sh" \
    "${remote_user}@${ip}:${remote_work_dir}/scripts/stubborn-scp.sh"

  ssh ${ssh_options} "${remote_user}@${ip}" "\
    chmod +x '${remote_work_dir}/scripts/start-slave.sh' \
             '${remote_work_dir}/scripts/stubborn-scp.sh' \
    2>/dev/null || true
  " </dev/null || true

  # Limpa binários antigos e copia novos
  remote_kill_bins "${ip}"
  copy_bin_atomic "${ip}" discoverymaster
  copy_bin_atomic "${ip}" discoveryslave
  copy_bin_atomic "${ip}" orderingpeer
  copy_bin_atomic "${ip}" orderingclient

  # TLS (novo)
  copy_tls_assets "${ip}"

  # Sanity check remoto
  remote_check_assets "${ip}"
}

start_remote_slave() {
  local instance_id="$1"
  local ctrl_ip="$2"
  local data_ip="$3"
  local role="$4"
  local tag="$5"

  info "Preparando slave ${instance_id} @ ${ctrl_ip} (role=${role}, tag=${tag})"

  copy_required_assets "${ctrl_ip}"

  # master_ip vem do ambiente (definido em deploy-remote.sh)
  local _master_ip="${master_ip:-}"
  if [[ -z "${_master_ip}" ]]; then
    err "Variável master_ip não definida no ambiente. Abortando start_remote_slave."
    return 1
  fi

  local remote_cmd="
    cd '${remote_work_dir}'
    echo '[start-remote-slaves] Disparando slave ${instance_id} (tag=${tag}) em ${ctrl_ip}...' >> '${remote_work_dir}/logs/start-remote-slaves-${instance_id}.log'
    /usr/bin/nohup '${remote_work_dir}/scripts/start-slave.sh' '${tag}' '${_master_ip}' '${ctrl_ip}' '${data_ip}' '${remote_exp_dir}' \\
      >> '${remote_work_dir}/logs/start-slave-${instance_id}.log' 2>&1 < /dev/null &
    echo STARTED
  "

  info "SSH para ${ctrl_ip} (disparar slave ${instance_id})..."

  # IMPORTANT: nunca trava aqui. Se timeout acontecer, mas tiver STARTED, consideramos OK.
  local out=""
  out="$( (timeout "${SSH_START_TIMEOUT}" ssh ${ssh_options} "${remote_user}@${ctrl_ip}" "${remote_cmd}" </dev/null) 2>&1 || true )"

  if echo "${out}" | grep -q "STARTED"; then
    echo "${out}"
    return 0
  fi

  err "Falha ao disparar slave ${instance_id} em ${ctrl_ip}. Saída:"
  echo "${out}" >&2
  return 0
}

total=0
matched=0
started=0

# Leitura do instance-info: id ctrl_ip data_ip role tag
while read -r instance_id ctrl_ip data_ip role tag rest; do
  # ignora linhas vazias ou comentários
  [[ -z "${instance_id:-}" ]] && continue
  [[ "${instance_id}" =~ ^# ]] && continue

  ((total++)) || true

  # filtra por tag desejada
  if [[ "${tag}" != "${wanted_tag}" ]]; then
    continue
  fi

  ((matched++)) || true

  # Respeita desired_count (0 = ilimitado)
  if [[ "${desired_count}" -gt 0 ]] && (( started >= desired_count )); then
    break
  fi

  start_remote_slave "${instance_id}" "${ctrl_ip}" "${data_ip}" "${role}" "${tag}"
  ((started++)) || true
done < "${instance_info_file}"

info "Resumo start-remote-slaves:"
info "  total linhas lidas   = ${total}"
info "  com tag=${wanted_tag} = ${matched}"
info "  efetivamente startados = ${started}"

