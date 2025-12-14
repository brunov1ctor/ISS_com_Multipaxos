#!/usr/bin/env bash
set -euo pipefail

# start-slave.sh
# Inicia discoveryslave no nó remoto.

tag="$1"             # peers ou 1client
master_ip="$2"       # ip público master
own_public_ip="$3"   # ip público deste nó
own_private_ip="$4"  # ip privado (10.10.1.X) deste nó
remote_exp_dir="$5"  # /users/<user>/iss/current-deployment-data

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# shellcheck source=global-vars.sh
source "${SCRIPT_DIR}/global-vars.sh"

log() { echo "[$(date '+%F %T')] $*"; }

log "start-slave.sh: tag=${tag} master_ip=${master_ip} own_public_ip=${own_public_ip} own_private_ip=${own_private_ip} remote_exp_dir=${remote_exp_dir}"
log "PATH antes: $PATH"

# Garante PATH contendo binários e scripts
export PATH="${remote_bin_dir}:${GOROOT}/bin:${remote_work_dir}/scripts:${remote_work_dir}/deployment/scripts:${PATH}"

# Variáveis que discoveryslave espera usar em comandos (printenv etc)
export master_port="${master_port}"
export own_public_ip="${own_public_ip}"
export own_private_ip="${own_private_ip}"
export GOPATH="${remote_gopath}"

log "PATH depois: $PATH"
log "GOPATH=$GOPATH"
log "remote_bin_dir=$remote_bin_dir"
log "remote_work_dir=$remote_work_dir"

# Diretórios essenciais
mkdir -p "${remote_work_dir}/logs" "${remote_work_dir}/config" "${remote_exp_dir}"

# (IMPORTANTE) create config/ antes do fluxo de copiar config
mkdir -p "${remote_work_dir}/config"
mkdir -p "${remote_work_dir}/config" 2>/dev/null || true
mkdir -p "config" 2>/dev/null || true

# Log files
SLAVE_LOG="${remote_work_dir}/logs/discoveryslave-${tag}.log"
PID_FILE="${remote_work_dir}/.discoveryslave-${tag}.pid"

# Mata qualquer discoveryslave antigo
pkill -9 -f "discoveryslave ${tag}" 2>/dev/null || true
pkill -9 -f "${remote_bin_dir}/discoveryslave" 2>/dev/null || true

# Inicia discoveryslave
log "Starting discoveryslave..."
/usr/bin/nohup "${remote_bin_dir}/discoveryslave" "${tag}" "${master_ip}:${master_port}" "${own_public_ip}" "${own_private_ip}" \
  > "${SLAVE_LOG}" 2>&1 < /dev/null &

echo $! > "${PID_FILE}"

sleep 0.2
if ! pgrep -af discoveryslave >/dev/null 2>&1; then
  log "ERROR: discoveryslave não está rodando. Últimas linhas do log:"
  tail -n 80 "${SLAVE_LOG}" || true
  exit 1
fi

log "discoveryslave iniciado. PID=$(cat "${PID_FILE}")"
log "Log: ${SLAVE_LOG}"

