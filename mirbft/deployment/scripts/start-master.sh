#!/usr/bin/env bash
set -euo pipefail

ts() { date +"%Y-%m-%d %H:%M:%S%z"; }
log() { echo "[start-master][$(ts)] $*"; }

# ---------------------------------------------------------------------------
# Args:
#   $1 = remote_user
#   $2 = master_ip
#   $3 = remote_work_dir
#   $4 = remote_bin_dir
#   $5 = exp_data_dir
#   $6 = local_master_cmd
# ---------------------------------------------------------------------------

remote_user="${1:-${remote_user:-${REMOTE_USER:-$USER}}}"
master_ip="${2:-${master_ip:-${MASTER_IP:-}}}"
remote_work_dir="${3:-${remote_work_dir:-${REMOTE_WORK_DIR:-/users/${remote_user}/iss}}}"
remote_bin_dir="${4:-${remote_bin_dir:-${REMOTE_BIN_DIR:-/users/${remote_user}/go/bin}}}"
exp_data_dir="${5:-${exp_data_dir:-${EXP_DATA_DIR:-}}}"
local_master_cmd="${6:-${local_master_cmd:-${LOCAL_MASTER_CMD:-}}}"

DISCOVERY_PORT="${DISCOVERY_PORT:-${master_port:-${MASTER_PORT:-9999}}}"
ssh_options="${ssh_options:-${SSH_OPTIONS:-"-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null"}}"

this_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Deduz exp_data_dir do PWD se possível
if [[ -z "${exp_data_dir}" ]]; then
  if [[ "$PWD" =~ deployment-data/remote-[0-9]{4}$ ]]; then
    exp_data_dir="$PWD"
  fi
fi

# Checagens
if [[ -z "${master_ip}" ]]; then
  echo "FATAL: master_ip vazio (configure MASTER_IP/master_ip)." >&2
  exit 1
fi
if [[ -z "${exp_data_dir}" ]]; then
  echo "FATAL: exp_data_dir vazio (configure EXP_DATA_DIR/exp_data_dir)." >&2
  exit 1
fi

if [[ -z "${local_master_cmd}" && -f "${exp_data_dir}/master-commands.cmd" ]]; then
  local_master_cmd="${exp_data_dir}/master-commands.cmd"
fi
if [[ -z "${local_master_cmd}" || ! -f "${local_master_cmd}" ]]; then
  echo "FATAL: não encontrei master-commands.cmd." >&2
  echo "  exp_data_dir=${exp_data_dir}" >&2
  echo "  local_master_cmd=${local_master_cmd:-<vazio>}" >&2
  exit 1
fi

debug_dir="${exp_data_dir}/_debug"
mkdir -p "${debug_dir}"
debug_log="${debug_dir}/start-master.${master_ip}.log"

# Loga tudo em arquivo + também mostra no terminal (mas sem spam)
exec > >(tee -a "${debug_log}") 2>&1

log "master_ip=${master_ip} port=${DISCOVERY_PORT} user=${remote_user}"
log "remote_work_dir=${remote_work_dir} remote_bin_dir=${remote_bin_dir}"
log "local_master_cmd=${local_master_cmd}"
log "debug_log=${debug_log}"

# ---------------------------------------------------------------------------
# Patch do master-commands.cmd (paths absolutos + layout remoto consistente)
# ---------------------------------------------------------------------------

patch_master_commands() {
  local f="$1"
  local bak="${f}.bak.$(date +%s)"

  cp -f "$f" "$bak"

  # Trocas robustas (perl)
  perl -0777 -pe "s#\\bstubborn-scp\\.sh\\b#${remote_work_dir}/scripts/stubborn-scp.sh#g" -i "$f"
  perl -0777 -pe "s#\\borderingpeer\\b#${remote_bin_dir}/orderingpeer#g" -i "$f"
  perl -0777 -pe "s#\\borderingclient\\b#${remote_bin_dir}/orderingclient#g" -i "$f"

  # Ajusta caminhos remotos usados no master-commands (mantendo $own_public_ip literal)
  perl -0777 -pe "s#\\\$own_public_ip:iss/experiment-config/#\\\$own_public_ip:${remote_work_dir}/experiment-config/#g" -i "$f"
  perl -0777 -pe "s#\\\$own_public_ip:iss/current-deployment-data/#\\\$own_public_ip:${remote_work_dir}/#g" -i "$f"

  # Garante criação dos diretórios essenciais no início
  if ! grep -q "mkdir -p ${remote_work_dir}/config" "$f"; then
    printf "exec-start __all__ /dev/null mkdir -p %s/config %s/logs %s/tls-data %s/experiment-output %s/raw-results\nexec-wait __all__ 2000\n\n" \
      "${remote_work_dir}" "${remote_work_dir}" "${remote_work_dir}" "${remote_work_dir}" "${remote_work_dir}" \
      | cat - "$f" > "${f}.tmp" && mv -f "${f}.tmp" "$f"
  fi

  log "master-commands patched (bak: $(basename "$bak"))"
}

patch_master_commands "${local_master_cmd}"

# ---------------------------------------------------------------------------
# Exec remoto helper
# ---------------------------------------------------------------------------

remote_exec() {
  ssh ${ssh_options} "${remote_user}@${master_ip}" "$@" < /dev/null
}

# ---------------------------------------------------------------------------
# Diretórios básicos no master
# ---------------------------------------------------------------------------

remote_exec "mkdir -p \
  '${remote_work_dir}' \
  '${remote_work_dir}/logs' \
  '${remote_work_dir}/config' \
  '${remote_work_dir}/scripts' \
  '${remote_work_dir}/tls-data' \
  '${remote_work_dir}/experiment-output' \
  '${remote_work_dir}/raw-results' \
"

# ---------------------------------------------------------------------------
# Copiar TLS (tls-data) para o master
# ---------------------------------------------------------------------------

local_tls_dir="$(cd "${this_dir}/.." && pwd)/tls-data"
if [[ ! -d "${local_tls_dir}" ]]; then
  echo "FATAL: local_tls_dir não encontrado: ${local_tls_dir}" >&2
  exit 1
fi

scp ${ssh_options} -r "${local_tls_dir}/"* "${remote_user}@${master_ip}:${remote_work_dir}/tls-data/"

remote_exec "test -f '${remote_work_dir}/tls-data/ca.pem' -a -f '${remote_work_dir}/tls-data/auth.pem' -a -f '${remote_work_dir}/tls-data/auth.key'" || {
  echo "FATAL: tls-data não foi copiado corretamente para ${remote_work_dir}/tls-data." >&2
  remote_exec "ls -la '${remote_work_dir}/tls-data' || true"
  exit 1
}

# ---------------------------------------------------------------------------
# Copiar master-commands.cmd e scripts auxiliares
# ---------------------------------------------------------------------------

scp ${ssh_options} "${local_master_cmd}" "${remote_user}@${master_ip}:${remote_work_dir}/master-commands.cmd"

scp ${ssh_options} "${this_dir}/stubborn-scp.sh" "${remote_user}@${master_ip}:${remote_work_dir}/scripts/stubborn-scp.sh"
scp ${ssh_options} "${this_dir}/global-vars.sh"   "${remote_user}@${master_ip}:${remote_work_dir}/scripts/global-vars.sh"
remote_exec "chmod +x '${remote_work_dir}/scripts/'*.sh || true"

# ---------------------------------------------------------------------------
# Copiar experiment-config para o master
# ---------------------------------------------------------------------------

if [[ -d "${exp_data_dir}/experiment-config" ]]; then
  scp ${ssh_options} -r "${exp_data_dir}/experiment-config" "${remote_user}@${master_ip}:${remote_work_dir}/"
else
  log "WARN: ${exp_data_dir}/experiment-config não encontrado."
fi

# ---------------------------------------------------------------------------
# Iniciar discoverymaster no master
# Assinatura correta:
#   discoverymaster <PORT> file <MASTER_COMMANDS_FILE>
#
# WRAPPER ROBUSTO:
#   - cria status sempre
#   - registra EXIT rc=...
#   - não depende de trap
# ---------------------------------------------------------------------------

remote_status_file="${remote_status_file:-${REMOTE_STATUS_FILE:-${remote_work_dir}/status}}"

log "starting discoverymaster..."

remote_exec "
  set -euo pipefail
  cd '${remote_work_dir}'
  rm -f '${remote_status_file}' '${remote_work_dir}/master-ready' '\$ready_file' 2>/dev/null || true

  nohup bash -lc '
    set +e
    STATUS_FILE=\"${remote_status_file}\"
    # Garante que o diretório do status existe (evita falha se alguém removeu o arquivo ou o diretório pai).
    mkdir -p \"\$(dirname \"\$STATUS_FILE\")\" 2>/dev/null || true
    : > \"\$STATUS_FILE\" 2>/dev/null || true
    chmod 664 \"\$STATUS_FILE\" 2>/dev/null || true
    echo STARTING at=\$(date -Iseconds) > \"\$STATUS_FILE\"

    \"${remote_bin_dir}/discoverymaster\" \"${DISCOVERY_PORT}\" file \"${remote_work_dir}/master-commands.cmd\"
    rc=\$?

    echo EXIT rc=\$rc at=\$(date -Iseconds) > \"\$STATUS_FILE\"
    exit \$rc
  ' > '${remote_work_dir}/logs/discoverymaster.log' 2>&1 < /dev/null &
"

# Espera curtinho o LISTEN (2s) e valida. Se morrer rápido, status vai ter EXIT.
sleep 2

if ! remote_exec "ss -lntp | grep -q \":${DISCOVERY_PORT}\"" >/dev/null 2>&1; then
  log "WARN: discoverymaster não está LISTEN em :${DISCOVERY_PORT} (pode ter encerrado)."
  remote_exec "ls -la '${remote_status_file}' 2>/dev/null || true; tail -n 5 '${remote_status_file}' 2>/dev/null || true"
  remote_exec "tail -n 120 '${remote_work_dir}/logs/discoverymaster.log' || true"
  exit 1
fi

log "discoverymaster LISTEN on ${master_ip}:${DISCOVERY_PORT}"
exit 0

