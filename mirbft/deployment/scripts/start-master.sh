#!/usr/bin/env bash
set -euo pipefail

ts() { date +"%Y-%m-%d %H:%M:%S%z"; }
log() { echo "[start-master][$(ts)] $*"; }

# ------------------------------------------------------------------------------
# Entrada: tenta env primeiro; se não tiver, tenta args; se não tiver, faz fallback
# ------------------------------------------------------------------------------
remote_user="${remote_user:-${REMOTE_USER:-${1:-Bruno}}}"
master_ip="${master_ip:-${MASTER_IP:-${2:-}}}"
remote_work_dir="${remote_work_dir:-${REMOTE_WORK_DIR:-${3:-/users/Bruno/iss}}}"
remote_bin_dir="${remote_bin_dir:-${REMOTE_BIN_DIR:-${4:-/users/Bruno/go/bin}}}"

# O deploy.sh DO SEU PROJETO já imprime exp_data_dir no initialize-deployment,
# então normalmente ele existe no ambiente. Mesmo assim, adiciono fallback por args.
exp_data_dir="${exp_data_dir:-${EXP_DATA_DIR:-${5:-}}}"

# local_master_cmd às vezes não é exportado pelo deploy.sh
local_master_cmd="${local_master_cmd:-${LOCAL_MASTER_CMD:-${6:-}}}"

DISCOVERY_PORT="${DISCOVERY_PORT:-${master_port:-${MASTER_PORT:-9999}}}"
ssh_options="${ssh_options:-${SSH_OPTIONS:-"-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null"}}"

# ------------------------------------------------------------------------------
# Fallbacks robustos (para não quebrar o deploy)
# ------------------------------------------------------------------------------
if [[ -z "${master_ip}" ]]; then
  # Alguns fluxos setam MASTER_IP no env e passam args vazios. Se ainda assim está vazio, falha.
  echo "FATAL: master_ip vazio. O deploy.sh precisa fornecer MASTER_IP/master_ip." >&2
  exit 1
fi

if [[ -z "${exp_data_dir}" ]]; then
  # Tenta deduzir por estar rodando dentro de deployment-data/remote-xxxx
  # (não é perfeito, mas ajuda)
  if [[ "${PWD}" =~ deployment-data/remote-[0-9]{4}$ ]]; then
    exp_data_dir="${PWD}"
  fi
fi

if [[ -z "${exp_data_dir}" ]]; then
  echo "FATAL: exp_data_dir vazio. O deploy.sh precisa fornecer EXP_DATA_DIR/exp_data_dir." >&2
  exit 1
fi

# Se local_master_cmd não veio, assume o caminho padrão gerado pelo pipeline
if [[ -z "${local_master_cmd}" ]]; then
  if [[ -f "${exp_data_dir}/master-commands.cmd" ]]; then
    local_master_cmd="${exp_data_dir}/master-commands.cmd"
  fi
fi

if [[ -z "${local_master_cmd}" || ! -f "${local_master_cmd}" ]]; then
  echo "FATAL: não consegui localizar master-commands.cmd." >&2
  echo "  exp_data_dir=${exp_data_dir}" >&2
  echo "  tentei: ${exp_data_dir}/master-commands.cmd" >&2
  echo "  local_master_cmd=${local_master_cmd:-<vazio>}" >&2
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

# ------------------------------------------------------------------------------
# Patch do master-commands.cmd para NÃO depender de PATH e para usar config absoluto
# ------------------------------------------------------------------------------
patch_master_commands() {
  local f="$1"
  local bak="${f}.bak.$(date +%s)"

  log "Patching master-commands.cmd (PATH-proof + config absoluto)..."
  cp -f "$f" "$bak"
  log "Backup: $bak"

  # 1) Comandos que não podem depender de PATH
  sed -i \
    -e 's#\bstubborn-scp\.sh\b#/users/Bruno/iss/scripts/stubborn-scp.sh#g' \
    -e 's#\borderingpeer\b#/users/Bruno/go/bin/orderingpeer#g' \
    -e 's#\borderingclient\b#/users/Bruno/go/bin/orderingclient#g' \
    "$f"

  # 2) Caminho de config absoluto (evita cwd / resolve bug original)
  sed -i \
    -e 's#\bconfig/config\.yml\b#/users/Bruno/iss/config/config.yml#g' \
    "$f"

  # 3) Garantir mkdir do /users/Bruno/iss/config antes do PRIMEIRO fetch de config
  if grep -q "/users/Bruno/iss/scripts/stubborn-scp.sh" "$f"; then
    if ! grep -q "mkdir -p /users/Bruno/iss/config" "$f"; then
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
    log "WARN: não achei stubborn-scp no master-commands.cmd (não inseri mkdir)."
  fi

  log "Trechos críticos (grep):"
  egrep -n "stubborn-scp|orderingpeer|orderingclient|config\.yml|mkdir -p /users/Bruno/iss/config" "$f" | head -n 200 || true
  log "Patch OK."
}

patch_master_commands "${local_master_cmd}"

remote_exec() {
  ssh ${ssh_options} "${remote_user}@${master_ip}" "$@" < /dev/null
}

log "Ensuring remote workdir exists..."
remote_exec "mkdir -p '${remote_work_dir}' '${remote_work_dir}/logs' '${remote_work_dir}/experiment-config' '${remote_work_dir}/config' '${remote_work_dir}/scripts'"

log "Copying master-commands.cmd to remote..."
scp ${ssh_options} -q "${local_master_cmd}" "${remote_user}@${master_ip}:${remote_work_dir}/master-commands.cmd"

log "Copying generated configs to master (experiment-config/) via scp..."
# Tenta caminhos comuns do seu pipeline
if [[ -d "${exp_data_dir}/experiment-config" ]]; then
  scp ${ssh_options} -q "${exp_data_dir}/experiment-config/"*.yml "${remote_user}@${master_ip}:${remote_work_dir}/experiment-config/" 2>/dev/null || true
fi
if [[ -d "${exp_data_dir}/config" ]]; then
  scp ${ssh_options} -q "${exp_data_dir}/config/"*.yml "${remote_user}@${master_ip}:${remote_work_dir}/experiment-config/" 2>/dev/null || true
fi
# fallback: qualquer config-*.yml no exp_data_dir (maxdepth 2)
mapfile -t cfgs < <(find "${exp_data_dir}" -maxdepth 2 -type f -name 'config-*.yml' 2>/dev/null || true)
if [[ ${#cfgs[@]} -gt 0 ]]; then
  scp ${ssh_options} -q "${cfgs[@]}" "${remote_user}@${master_ip}:${remote_work_dir}/experiment-config/" 2>/dev/null || true
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

log "Starting discoverymaster on :${DISCOVERY_PORT} ..."
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

log "Verificando se o master está escutando na porta ${DISCOVERY_PORT}..."
remote_exec "ss -lntp | grep ':${DISCOVERY_PORT} ' >/dev/null && echo OK || (echo FAIL; ss -lntp | grep ':${DISCOVERY_PORT} ' || true; exit 1)"

log "Master started successfully."

