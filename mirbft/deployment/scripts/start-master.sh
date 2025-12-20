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

# O deploy.sh DO SEU PROJETO já fornece exp_data_dir no ambiente,
# mas deixamos fallback por args se necessário.
exp_data_dir="${exp_data_dir:-${EXP_DATA_DIR:-${5:-}}}"

# local_master_cmd às vezes não é exportado pelo deploy.sh
local_master_cmd="${local_master_cmd:-${LOCAL_MASTER_CMD:-${6:-}}}"

DISCOVERY_PORT="${DISCOVERY_PORT:-${master_port:-${MASTER_PORT:-9999}}}"
ssh_options="${ssh_options:-${SSH_OPTIONS:-"-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null"}}"

# ------------------------------------------------------------------------------
# Fallbacks robustos (para não quebrar o deploy)
# ------------------------------------------------------------------------------
if [[ -z "${master_ip}" ]]; then
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
    -e "s#\bconfig/config.yml\b#${remote_work_dir}/config/config.yml#g" \
    -e "s#\bexperiment-config/config-#${remote_work_dir}/experiment-config/config-#g" \
    "$f"

  # 3) Garante mkdir -p do config no master
  if ! grep -q "mkdir -p /users/Bruno/iss/config" "$f"; then
    log "Inserindo mkdir -p /users/Bruno/iss/config no master-commands..."
    sed -i "1i mkdir -p /users/Bruno/iss/config" "$f"
  else
    log "mkdir -p /users/Bruno/iss/config já presente (não duplicando)."
  fi

  log "Trechos críticos (grep):"
  egrep -n "stubborn-scp|orderingpeer|orderingclient|config/config.yml|mkdir -p /users/Bruno/iss/config" "$f" | head -n 200 || true
  log "Patch OK."
}

patch_master_commands "${local_master_cmd}"

remote_exec() {
  ssh ${ssh_options} "${remote_user}@${master_ip}" "$@" < /dev/null
}

log "Ensuring remote workdir exists..."
remote_exec "mkdir -p '${remote_work_dir}' '${remote_work_dir}/logs' '${remote_work_dir}/config' '${remote_work_dir}/scripts'"

log "Copying master-commands.cmd to remote..."
scp ${ssh_options} -q "${local_master_cmd}" "${remote_user}@${master_ip}:${remote_work_dir}/master-commands.cmd"

log "Copying generated configs to master (experiment-config/) via scp..."
# Tenta caminhos comuns do seu pipeline
if [[ -d \"${exp_data_dir}/experiment-config\" ]]; then
  scp -r ${ssh_options} \"${exp_data_dir}/experiment-config\" \"${remote_user}@${master_ip}:${remote_work_dir}/\"
elif [[ -d \"${exp_data_dir}/config\" ]]; then
  scp -r ${ssh_options} \"${exp_data_dir}/config\" \"${remote_user}@${master_ip}:${remote_work_dir}/config\"
else
  log \"WARN: nenhum diretório experiment-config/ ou config/ encontrado em ${exp_data_dir}\"
fi

log \"Iniciando discoverymaster no master (nohup)...\"
remote_exec \"\
  mkdir -p '${remote_work_dir}/logs' '${remote_work_dir}/experiment-output'; \
  cd '${remote_work_dir}'
  rm -f '${remote_work_dir}/status' '${remote_work_dir}/master-ready' 2>/dev/null || true
  /usr/bin/nohup '${remote_bin_dir}/discoverymaster' master '${master_ip}:${DISCOVERY_PORT}' '${remote_work_dir}/master-commands.cmd' \
    > '${remote_work_dir}/logs/discoverymaster.log' 2>&1 < /dev/null &
  echo PID=\$!
  sleep 0.2
  pgrep -af discoverymaster 2>/dev/null || true
  tail -n 30 '${remote_work_dir}/logs/discoverymaster.log' 2>/dev/null || true
\"

log \"Verificando se o master está escutando na porta ${DISCOVERY_PORT}...\"
remote_exec \"ss -lntp | grep ':${DISCOVERY_PORT} ' >/dev/null 2>&1 || (echo 'Master parece não estar escutando na porta ${DISCOVERY_PORT}' && echo FAIL; ss -lntp | grep ':${DISCOVERY_PORT} ' || true; exit 1)\"

log \"Master started successfully.\"

