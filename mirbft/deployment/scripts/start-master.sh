#!/usr/bin/env bash
set -euo pipefail

ts() { date +"%Y-%m-%d %H:%M:%S%z"; }
log() { echo "[start-master][$(ts)] $*"; }

# ---------------------------------------------------------------------------
# 1) Entrada e defaults robustos
# ---------------------------------------------------------------------------

# Ordem dos parâmetros:
#   $1 = remote_user
#   $2 = master_ip
#   $3 = remote_work_dir
#   $4 = remote_bin_dir
#   $5 = exp_data_dir
#   $6 = local_master_cmd

remote_user="${1:-${remote_user:-${REMOTE_USER:-$USER}}}"
master_ip="${2:-${master_ip:-${MASTER_IP:-}}}"
remote_work_dir="${3:-${remote_work_dir:-${REMOTE_WORK_DIR:-/users/${remote_user}/iss}}}"
remote_bin_dir="${4:-${remote_bin_dir:-${REMOTE_BIN_DIR:-/users/${remote_user}/go/bin}}}"
exp_data_dir="${5:-${exp_data_dir:-${EXP_DATA_DIR:-}}}"
local_master_cmd="${6:-${local_master_cmd:-${LOCAL_MASTER_CMD:-}}}"

DISCOVERY_PORT="${DISCOVERY_PORT:-${master_port:-${MASTER_PORT:-9999}}}"
ssh_options="${ssh_options:-${SSH_OPTIONS:-"-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null"}}"

this_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Se exp_data_dir não vier por env/arg, tenta deduzir do PWD
if [[ -z "${exp_data_dir}" ]]; then
  if [[ "$PWD" =~ deployment-data/remote-[0-9]{4}$ ]]; then
    exp_data_dir="$PWD"
  fi
fi

# ---------------------------------------------------------------------------
# 2) Checagens básicas
# ---------------------------------------------------------------------------

if [[ -z "${master_ip}" ]]; then
  echo "FATAL: master_ip vazio (configure MASTER_IP/master_ip)." >&2
  exit 1
fi

if [[ -z "${exp_data_dir}" ]]; then
  echo "FATAL: exp_data_dir vazio (configure EXP_DATA_DIR/exp_data_dir)." >&2
  exit 1
fi

if [[ -z "${local_master_cmd}" ]]; then
  # fallback para o caminho padrão dentro de exp_data_dir
  if [[ -f "${exp_data_dir}/master-commands.cmd" ]]; then
    local_master_cmd="${exp_data_dir}/master-commands.cmd"
  fi
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

# Tudo logado nesse arquivo também
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

# ---------------------------------------------------------------------------
# 3) Patch do master-commands.cmd (não depender de PATH + config absoluto)
# ---------------------------------------------------------------------------

patch_master_commands() {
  local f="$1"
  local bak="${f}.bak.$(date +%s)"

  log "Ajustando master-commands.cmd (paths absolutos)..."
  cp -f "$f" "$bak"
  log "Backup salvo em: $bak"

  # 1) Forçar caminhos absolutos de scripts e binários no master
  sed -i \
    -e 's#\bstubborn-scp\.sh\b#'"${remote_work_dir}"'/scripts/stubborn-scp.sh#g' \
    -e 's#\borderingpeer\b#'"${remote_bin_dir}"'/orderingpeer#g' \
    -e 's#\borderingclient\b#'"${remote_bin_dir}"'/orderingclient#g' \
    "$f"

  # 2) Transformar destinos relativos "iss/..." em caminhos absolutos no master
  sed -i \
    -e "s#\\$own_public_ip:iss/experiment-config/#\\$own_public_ip:${remote_work_dir}/experiment-config/#g" \
    -e "s#\\$own_public_ip:iss/current-deployment-data/#\\$own_public_ip:${remote_work_dir}/current-deployment-data/#g" \
    "$f"

  # 3) Caminhos absolutos no lado do slave
  sed -i \
    -e 's#\bconfig/config\.yml\b#'"${remote_work_dir}"'/config/config.yml#g' \
    -e 's#\bexperiment-config/config-#'"${remote_work_dir}"'/experiment-config/config-#g' \
    "$f"

  # 4) Garantir criação do diretório config/
  if ! grep -q "exec-start __all__ /dev/null mkdir -p ${remote_work_dir}/config" "$f"; then
    log "Inserindo criação de ${remote_work_dir}/config via exec-start no início do master-commands."
    sed -i "1i exec-start __all__ /dev/null mkdir -p ${remote_work_dir}/config\nexec-wait __all__ 2000\n" "$f"
  else
    log "Criação de ${remote_work_dir}/config já existe no master-commands."
  fi

  log "Patch do master-commands concluído."
}

# Executa o patch localmente ANTES de enviar para o master.
patch_master_commands "${local_master_cmd}"

# ---------------------------------------------------------------------------
# 4) Garantir diretórios e scripts no master
# ---------------------------------------------------------------------------

remote_exec() {
  ssh ${ssh_options} "${remote_user}@${master_ip}" "$@" < /dev/null
}

log "Criando diretórios básicos no master..."
remote_exec "mkdir -p '${remote_work_dir}' '${remote_work_dir}/logs' '${remote_work_dir}/config' '${remote_work_dir}/scripts' '${remote_work_dir}/experiment-output' '${remote_work_dir}/tls-data' '${remote_work_dir}/current-deployment-data/tls-data'"

# ---------------------------------------------------------------------------
# 4b) Copiar TLS (tls-data) para o master
# ---------------------------------------------------------------------------

local_tls_dir="$(cd "${this_dir}/.." && pwd)/tls-data"

if [[ -d "${local_tls_dir}" ]]; then
  log "Copiando TLS de ${local_tls_dir} para o master (${remote_work_dir}/tls-data e ${remote_work_dir}/current-deployment-data/tls-data)..."
  scp ${ssh_options} -r "${local_tls_dir}/"* "${remote_user}@${master_ip}:${remote_work_dir}/tls-data/"
  scp ${ssh_options} -r "${local_tls_dir}/"* "${remote_user}@${master_ip}:${remote_work_dir}/current-deployment-data/tls-data/"
  remote_exec "test -f '${remote_work_dir}/tls-data/ca.pem' -a -f '${remote_work_dir}/tls-data/auth.pem' -a -f '${remote_work_dir}/tls-data/auth.key'" || {
    echo "FATAL: tls-data não foi copiado corretamente para o master." >&2
    exit 1
  }
else
  echo "FATAL: local_tls_dir não encontrado: ${local_tls_dir}" >&2
  exit 1
fi

log "Copiando master-commands.cmd para o master..."
scp ${ssh_options} "${local_master_cmd}" "${remote_user}@${master_ip}:${remote_work_dir}/master-commands.cmd"

log "Copiando scripts auxiliares (stubborn-scp.sh, global-vars.sh)..."
scp ${ssh_options} "${this_dir}/stubborn-scp.sh" "${remote_user}@${master_ip}:${remote_work_dir}/scripts/stubborn-scp.sh"
scp ${ssh_options} "${this_dir}/global-vars.sh"   "${remote_user}@${master_ip}:${remote_work_dir}/scripts/global-vars.sh"
remote_exec "chmod +x '${remote_work_dir}/scripts/'*.sh || true"

# ---------------------------------------------------------------------------
# 5) Copiar experiment-config para o master
# ---------------------------------------------------------------------------

local_config_dir="/users/${remote_user}/iss/experiment-config"

if [[ -d "${local_config_dir}" ]]; then
  log "Copiando configs de ${local_config_dir} para o master..."
  scp ${ssh_options} -r "${local_config_dir}" "${remote_user}@${master_ip}:${remote_work_dir}/"
elif [[ -d "${exp_data_dir}/experiment-config" ]]; then
  log "Copiando configs de ${exp_data_dir}/experiment-config para o master..."
  scp ${ssh_options} -r "${exp_data_dir}/experiment-config" "${remote_user}@${master_ip}:${remote_work_dir}/"
else
  log "WARN: nenhum diretório experiment-config em ${local_config_dir} ou ${exp_data_dir}/experiment-config."
fi

# ---------------------------------------------------------------------------
# 6) Iniciar discoverymaster no master
# ---------------------------------------------------------------------------

log "Iniciando discoverymaster (nohup) no master..."
remote_exec "
  cd '${remote_work_dir}' && \
  rm -f '${remote_work_dir}/status' '${remote_work_dir}/master-ready' 2>/dev/null || true
  nohup '${remote_bin_dir}/discoverymaster' master '${master_ip}:${DISCOVERY_PORT}' '${remote_work_dir}/master-commands.cmd' \
    > '${remote_work_dir}/logs/discoverymaster.log' 2>&1 < /dev/null &
"

log "Checando se o master está escutando na porta ${DISCOVERY_PORT}..."
if ! remote_exec "ss -lntp | grep \":${DISCOVERY_PORT} \"" >/dev/null 2>&1; then
  log "ATENÇÃO: master não parece escutando na porta ${DISCOVERY_PORT} (veja discoverymaster.log no master)."
else
  log "Master ativo em ${master_ip}:${DISCOVERY_PORT}."
fi

