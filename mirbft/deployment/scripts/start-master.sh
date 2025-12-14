#!/usr/bin/env bash
set -euo pipefail

# ==============================================================================
# start-master.sh
#
# Responsável por:
#  - Garantir workdir no master
#  - Patchar o master-commands.cmd para NÃO depender de PATH (caminhos absolutos)
#  - Copiar master-commands.cmd e configs gerados para o master
#  - Subir discoverymaster no master
#
# Compatibilidade:
#  - Lê variáveis de ambiente (preferencial) ou argumentos (fallback).
# ==============================================================================

ts() { date +"%Y-%m-%d %H:%M:%S%z"; }
log() { echo "[start-master][$(ts)] $*"; }

# -------------------------
# Entrada (env > args)
# -------------------------
remote_user="${remote_user:-${1:-Bruno}}"
master_ip="${master_ip:-${2:-}}"
remote_work_dir="${remote_work_dir:-${3:-/users/Bruno/iss}}"
remote_bin_dir="${remote_bin_dir:-${4:-/users/Bruno/go/bin}}"
exp_data_dir="${exp_data_dir:-${5:-}}"
local_master_cmd="${local_master_cmd:-${6:-}}"
DISCOVERY_PORT="${DISCOVERY_PORT:-${7:-9999}}"
ssh_options="${ssh_options:-"-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null"}"

if [[ -z "${master_ip}" ]]; then
  echo "FATAL: master_ip vazio. Passe via env master_ip=... ou args." >&2
  exit 1
fi
if [[ -z "${exp_data_dir}" ]]; then
  echo "FATAL: exp_data_dir vazio. Passe via env exp_data_dir=... ou args." >&2
  exit 1
fi
if [[ -z "${local_master_cmd}" ]]; then
  echo "FATAL: local_master_cmd vazio. Passe via env local_master_cmd=... ou args." >&2
  exit 1
fi
if [[ ! -f "${local_master_cmd}" ]]; then
  echo "FATAL: local_master_cmd não existe: ${local_master_cmd}" >&2
  exit 1
fi

debug_dir="${exp_data_dir}/_debug"
mkdir -p "${debug_dir}"
debug_log="${debug_dir}/start-master.${master_ip}.log"
exec > >(tee -a "${debug_log}") 2>&1

log "remote_user=${remote_user}"
log "master_ip=${master_ip}"
log "ssh_options=${ssh_options}"
log "remote_work_dir=${remote_work_dir}"
log "remote_bin_dir=${remote_bin_dir}"
log "exp_data_dir=${exp_data_dir}"
log "DISCOVERY_PORT=${DISCOVERY_PORT}"
log "local_master_cmd=${local_master_cmd}"
log "debug_log=${debug_log}"

# -------------------------
# Patch do master-commands
# -------------------------
patch_master_commands() {
  local f="$1"
  local bak="${f}.bak.$(date +%s)"

  log "Patching master-commands.cmd para caminhos absolutos (PATH-proof)..."
  cp -f "$f" "$bak"
  log "Backup criado em: $bak"

  # 1) scripts/bins sem PATH -> absolutos
  # stubborn-scp.sh
  sed -i \
    -e 's#\bstubborn-scp\.sh\b#/users/Bruno/iss/scripts/stubborn-scp.sh#g' \
    -e 's#\borderingpeer\b#/users/Bruno/go/bin/orderingpeer#g' \
    -e 's#\borderingclient\b#/users/Bruno/go/bin/orderingclient#g' \
    "$f"

  # 2) config relativo -> absoluto
  sed -i \
    -e 's#\bconfig/config\.yml\b#/users/Bruno/iss/config/config.yml#g' \
    "$f"

  # 3) Garantir mkdir do CFGDIR antes do PRIMEIRO fetch de config
  #    (insere antes da primeira ocorrência do stubborn-scp, já absolutizada)
  if grep -q "/users/Bruno/iss/scripts/stubborn-scp.sh" "$f"; then
    # Insere só uma vez (evitar duplicar se script rodar mais de uma vez)
    if ! grep -q "mkdir -p /users/Bruno/iss/config" "$f"; then
      # Inserção robusta: antes da primeira linha que contém stubborn-scp
      awk '
        BEGIN{inserted=0}
        {
          if(!inserted && $0 ~ /\/users\/Bruno\/iss\/scripts\/stubborn-scp\.sh/) {
            print "exec-start __all__ /dev/null mkdir -p /users/Bruno/iss/config"
            print "exec-wait __all__ 2000"
            inserted=1
          }
          print $0
        }
      ' "$f" > "${f}.tmp" && mv "${f}.tmp" "$f"
      log "Inserido mkdir -p /users/Bruno/iss/config antes do 1º fetch de config."
    else
      log "mkdir -p /users/Bruno/iss/config já presente (não duplicando)."
    fi
  else
    log "WARN: não achei stubborn-scp no master-commands.cmd (nada a inserir de mkdir)."
  fi

  # 4) Diagnóstico: mostrar as linhas críticas
  log "Trechos críticos (grep):"
  egrep -n "stubborn-scp|orderingpeer|orderingclient|config\.yml|mkdir -p /users/Bruno/iss/config" "$f" | head -n 120 || true

  log "Patch concluído."
}

patch_master_commands "${local_master_cmd}"

# -------------------------
# Ações no master remoto
# -------------------------
remote_exec() {
  ssh ${ssh_options} "${remote_user}@${master_ip}" "$@" < /dev/null
}

log "Ensuring remote workdir exists..."
remote_exec "mkdir -p '${remote_work_dir}' '${remote_work_dir}/logs' '${remote_work_dir}/experiment-config' '${remote_work_dir}/config' '${remote_work_dir}/scripts'"

log "Copying master-commands.cmd to remote..."
scp ${ssh_options} -q "${local_master_cmd}" "${remote_user}@${master_ip}:${remote_work_dir}/master-commands.cmd"

log "Copying generated configs to master (experiment-config/) via scp..."
# Copia configs gerados no exp_data_dir/configs (ou config/), tenta ambos.
if [[ -d "${exp_data_dir}/experiment-config" ]]; then
  scp ${ssh_options} -q "${exp_data_dir}/experiment-config/"*.yml "${remote_user}@${master_ip}:${remote_work_dir}/experiment-config/" || true
fi
if [[ -d "${exp_data_dir}/config" ]]; then
  scp ${ssh_options} -q "${exp_data_dir}/config/"*.yml "${remote_user}@${master_ip}:${remote_work_dir}/experiment-config/" || true
fi
# fallback: tenta achar configs no exp_data_dir
found_cfgs="$(find "${exp_data_dir}" -maxdepth 2 -type f -name 'config-*.yml' 2>/dev/null | head -n 1 || true)"
if [[ -n "${found_cfgs}" ]]; then
  scp ${ssh_options} -q "${exp_data_dir}"/config-*.yml "${remote_user}@${master_ip}:${remote_work_dir}/experiment-config/" 2>/dev/null || true
fi

log "remote experiment-config:"
remote_exec "ls -la '${remote_work_dir}/experiment-config' || true"

log "Killing previous discoverymaster (if any)..."
set +e
remote_exec "
  pkill -9 -f '${remote_bin_dir}/discoverymaster' 2>/dev/null || true
  pkill -9 -f 'discoverymaster ' 2>/dev/null || true
  sleep 0.2
  pgrep -af discoverymaster 2>/dev/null || true
"
rc=$?
set -e
if [[ $rc -ne 0 ]]; then
  log "WARN: Kill step returned rc=${rc} (continuando)"
fi

log "Starting discoverymaster in MASTER mode (file-based commands)..."
remote_exec "
  set -e
  cd '${remote_work_dir}'
  rm -f '${remote_work_dir}/status' '${remote_work_dir}/master-ready' 2>/dev/null || true
  /usr/bin/nohup '${remote_bin_dir}/discoverymaster' master ':${DISCOVERY_PORT}' '${remote_work_dir}/master-commands.cmd' \
    > '${remote_work_dir}/logs/discoverymaster.log' 2>&1 < /dev/null &
  echo PID=\$!
  sleep 0.2
  pgrep -af discoverymaster 2>/dev/null || true
  tail -n 30 '${remote_work_dir}/logs/discoverymaster.log' 2>/dev/null || true
"

log "Verificando se o master está vivo e escutando na porta ${DISCOVERY_PORT}..."
remote_exec "ss -lntp | grep ':${DISCOVERY_PORT} ' >/dev/null && echo OK || (echo FAIL; ss -lntp | grep ':${DISCOVERY_PORT} ' || true; exit 1)"

log "Master started successfully."

