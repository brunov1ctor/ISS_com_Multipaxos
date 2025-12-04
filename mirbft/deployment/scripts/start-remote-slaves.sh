#!/usr/bin/env bash

#------------------------------------------------------------------------------
# start-remote-slaves.sh
#
# Dispara os slaves remotos para um experimento remoto usando o Emulab.
#
# Parâmetros:
#   $1 = exp_data_dir (ex.: deployment-data/remote-0000)
#   $2 = tag          (ex.: peers, 1client, ...)
#   $3 = n            (número de instâncias para essa tag)
#   $4 = master_ip    (endereço de controle do master)
#   $5.. = lista de (instance_id, ctrl_ip, data_ip, role, tag) repetidos
#
#   Ou seja, a partir de $5 os argumentos vêm em grupos de 5:
#     instance_id ctrl_ip data_ip role tag
#------------------------------------------------------------------------------

set -euo pipefail

#-------------------- Utilitários de log --------------------------------------

log_info()  { echo "[start-remote-slaves][INFO ] $*"; }
log_warn()  { echo "[start-remote-slaves][WARN ] $*" >&2; }
log_error() { echo "[start-remote-slaves][ERROR] $*" >&2; }

#-------------------- Verificação de argumentos --------------------------------

if [ $# -lt 4 ]; then
  log_error "Uso: $0 <exp_data_dir> <tag> <n> <master_ip> (instance_id ctrl_ip data_ip role tag) ..."
  exit 1
fi

exp_data_dir="$1"
tag="$2"
n="$3"
master_ip="$4"
shift 4

if [ $(( $# % 5 )) -ne 0 ]; then
  log_error "Número de argumentos inválido; esperado múltiplos de 5 após master_ip (instance_id, ctrl_ip, data_ip, role, tag)."
  exit 1
fi

#-------------------- Diretórios base -----------------------------------------

# Caminho deste script
this_dir="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"

# Diretório deployment (um nível acima deste script)
deployment_dir="$( cd "${this_dir}/.." && pwd )"

# Diretório do repositório (um nível acima de deployment)
repo_dir="$( cd "${deployment_dir}/.." && pwd )"

# Tenta carregar variáveis globais, se existirem
if [ -f "${deployment_dir}/scripts/global-vars.sh" ]; then
  # shellcheck source=/dev/null
  . "${deployment_dir}/scripts/global-vars.sh"
fi

# Aplica defaults SE não vierem de global-vars.sh
if [ -z "${remote_user-}" ]; then
  remote_user="Bruno"
fi
if [ -z "${remote_gopath-}" ]; then
  remote_gopath="/users/${remote_user}/go"
fi
if [ -z "${remote_work_dir-}" ]; then
  remote_work_dir="/users/${remote_user}/iss"
fi
if [ -z "${remote_exp_dir-}" ]; then
  remote_exp_dir="${remote_work_dir}/current-deployment-data"
fi

remote_bin_dir="${remote_gopath}/bin"

# Arquivos locais que precisamos para os slaves
local_start_slave_script="${deployment_dir}/scripts/start-slave.sh"
local_scripts_dir="${deployment_dir}/scripts"

log_info "==== [start-remote-slaves] Diretórios detectados ====="
log_info "  this_dir       = ${this_dir}"
log_info "  deployment_dir = ${deployment_dir}"
log_info "  repo_dir       = ${repo_dir}"
log_info "  remote_user    = ${remote_user}"
log_info "  remote_gopath  = ${remote_gopath}"
log_info "  remote_bin_dir = ${remote_bin_dir}"
log_info "  remote_work_dir= ${remote_work_dir}"
log_info "  remote_exp_dir = ${remote_exp_dir}"
echo

#-------------------- APENAS verifica binários locais -------------------------

log_info "==== [start-remote-slaves] (LOCAL) Verificando binários em \$GOPATH/bin ===="
local_bin_dir="${GOPATH:-${remote_gopath}}/bin"
log_info "  remote_gopath = ${remote_gopath}"
log_info "  local_bin_dir = ${local_bin_dir}"

for bin in discoverymaster discoveryslave orderingpeer orderingclient; do
  if [ ! -x "${local_bin_dir}/${bin}" ]; then
    log_error "  [LOCAL] ERRO: binário não encontrado: ${local_bin_dir}/${bin}"
    log_error "          Compile manualmente com:  (no repo)  go install ./cmd/..."
    exit 1
  fi
  log_info "  [LOCAL] OK: ${local_bin_dir}/${bin}"
done
log_info "==== [start-remote-slaves] Binários verificados. ===="
echo

#-------------------- Função: garantir ambiente remoto ------------------------

ensure_remote_slave() {
  local instance_id="$1"
  local ctrl_ip="$2"

  log_info "---------------------------------------------------------------------"
  log_info "  [REMOTO] Garantindo ambiente em ${ctrl_ip}"
  log_info "           instance_id = ${instance_id}"
  log_info "           tag         = ${tag}"
  log_info "---------------------------------------------------------------------"

  ssh -o StrictHostKeyChecking=accept-new "${remote_user}@${ctrl_ip}" "
    mkdir -p '${remote_work_dir}' '${remote_exp_dir}' '${remote_work_dir}/status' &&
    echo RUNNING > '${remote_work_dir}/status/${instance_id}.status' &&
    rm -f '${remote_work_dir}/master.ready'
  " || {
    log_error "  [REMOTO] ERRO ao criar diretórios em ${ctrl_ip} (instance_id=${instance_id})."
    return 1
  }

  scp -o StrictHostKeyChecking=accept-new \
    \"${local_bin_dir}/discoverymaster\" \
    \"${local_bin_dir}/discoveryslave\" \
    \"${local_bin_dir}/orderingpeer\" \
    \"${local_bin_dir}/orderingclient\" \
    \"${remote_user}@${ctrl_ip}:${remote_bin_dir}/\" >/dev/null 2>&1 || {
      log_warn \"  [REMOTO] Aviso: não foi possível copiar binários para ${ctrl_ip}; assumindo que já existem.\"
  }

  scp -o StrictHostKeyChecking=accept-new \
    \"${local_start_slave_script}\" \
    \"${remote_user}@${ctrl_ip}:${remote_work_dir}/\" >/dev/null 2>&1 || {
      log_warn \"  [REMOTO] Aviso: não foi possível copiar start-slave.sh para ${ctrl_ip}; assumindo que já existe.\"
  }

  scp -r -o StrictHostKeyChecking=accept-new \
    \"${local_scripts_dir}\" \
    \"${remote_user}@${ctrl_ip}:${remote_work_dir}/\" >/dev/null 2>&1 || {
      log_warn \"  [REMOTO] Aviso: não foi possível copiar diretório scripts/ para ${ctrl_ip}; assumindo que já existe.\"
  }

  log_info "    [REMOTO] OK: ambiente garantido em ${ctrl_ip}."
}

#-------------------- Marca slaves que serão usados ---------------------------

log_info "==== [start-remote-slaves] (REMOTO) Garantindo binários e scripts ===="

args=( "$@" )
total="${#args[@]}"
used=0
i=0

while [ "${i}" -lt "${total}" ]; do
  instance_id="${args[$i]}";   i=$((i+1))
  ctrl_ip="${args[$i]}";       i=$((i+1))
  data_ip="${args[$i]}";       i=$((i+1))
  role="${args[$i]}";          i=$((i+1))
  itag="${args[$i]}";          i=$((i+1))

  if [ "${role}" != "slave" ] || [ "${itag}" != "${tag}" ]; then
    continue
  fi

  ensure_remote_slave "${instance_id}" "${ctrl_ip}" || {
    log_error "  [REMOTO] ERRO ao preparar ambiente em ${ctrl_ip} (instance_id=${instance_id})."
  }

  used=$((used+1))
done

if [ "${used}" -lt "${n}" ]; then
  log_warn "Apenas ${used} slaves com tag=${tag} foram preparados, mas n=${n} foi solicitado."
fi

log_info "==== [start-remote-slaves] Distribuição concluída. ===="
echo

#-------------------- Dispara os slaves da TAG pedida -------------------------

log_info "==== [start-remote-slaves] Iniciando slaves da TAG '${tag}' (n = ${n}) ===="

i=0
started=0
while [ "${i}" -lt "${total}" ]; do
  instance_id="${args[$i]}";   i=$((i+1))
  ctrl_ip="${args[$i]}";       i=$((i+1))
  data_ip="${args[$i]}";       i=$((i+1))
  role="${args[$i]}";          i=$((i+1))
  itag="${args[$i]}";          i=$((i+1))

  if [ "${role}" != "slave" ] || [ "${itag}" != "${tag}" ]; then
    continue
  fi

  log_info "  [DEPLOY] Iniciando slave em ${ctrl_ip} (instance_id=${instance_id}, tag=${itag})"

  ssh -o StrictHostKeyChecking=accept-new "${remote_user}@${ctrl_ip}" "
    cd '${remote_work_dir}' &&
    chmod +x ./start-slave.sh &&
    ./start-slave.sh '${exp_data_dir}' '${instance_id}' '${master_ip}'
  " >/dev/null 2>&1 || {
    log_error "  [DEPLOY] ERRO ao disparar slave em ${ctrl_ip} (instance_id=${instance_id})."
    continue
  }

  started=$((started+1))
done

log_info
log_info "==== [start-remote-slaves] Todos os slaves disparados. ===="
log_info "==== [start-remote-slaves] FIM ==========================================="

