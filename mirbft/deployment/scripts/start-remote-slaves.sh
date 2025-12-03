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
#
# Exemplo de chamada (log que você vê no deploy):
#   tag          = 1client
#   n            = 1
#   master_ip    = 172.20.4.4
#   args rest    = node-0 172.20.4.4 10.10.1.1 master master \
#                  node-1 172.20.4.5 10.10.1.2 slave  peers  \
#                  node-2 172.20.4.6 10.10.1.3 slave  peers  \
#                  node-3 172.20.4.7 10.10.1.4 slave  peers  \
#                  node-4 172.20.3.5 10.10.1.5 slave  peers  \
#                  node-5 172.20.4.8 10.10.1.6 slave  peers  \
#                  node-6 172.20.4.9 10.10.1.7 slave  1client
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

# Carrega variáveis globais (remotas/local)
#  Espera definir:
#   remote_user
#   remote_gopath
#   remote_work_dir
#   remote_exp_dir
#   remote_status_dir
#   remote_status_file_template
#   remote_ready_file
# etc.
if [ -f "${deployment_dir}/scripts/global-vars.sh" ]; then
  # shellcheck source=/dev/null
  . "${deployment_dir}/scripts/global-vars.sh"
else
  log_warn "global-vars.sh não encontrado; usando defaults básicos."
  remote_user="${remote_user:-Bruno}"
  remote_gopath="${remote_gopath:-/users/${remote_user}/go}"
  remote_work_dir="${remote_work_dir:-/users/${remote_user}/iss}"
  remote_exp_dir="${remote_exp_dir:-${remote_work_dir}/current-deployment-data}"
fi

remote_bin_dir="${remote_gopath}/bin"

# Arquivos locais que precisamos para os slaves
local_start_slave_script="${deployment_dir}/scripts/start-slave.sh"
local_scripts_dir="${deployment_dir}/scripts"

log_info "==== [start-remote-slaves] Diretórios detectados ====="
log_info "  this_dir       = ${this_dir}"
log_info "  deployment_dir = ${deployment_dir}"
log_info "  repo_dir       = ${repo_dir}"
log_info "  remote_gopath  = ${remote_gopath}"
log_info "  remote_bin_dir = ${remote_bin_dir}"
log_info "  remote_work_dir= ${remote_work_dir}"
log_info "  remote_exp_dir = ${remote_exp_dir}"
echo

#-------------------- Compila binários localmente -----------------------------

log_info "==== [start-remote-slaves] (LOCAL) Compilando binários ====="
log_info "  Repositório: ${repo_dir}"
(
  cd "${repo_dir}"
  go install ./cmd/...
)
log_info "  [LOCAL] Compilação concluída."
echo

# Verifica binários locais
log_info "==== [start-remote-slaves] (LOCAL) Verificando binários em \$GOPATH/bin ===="
local_bin_dir="${GOPATH:-${remote_gopath}}/bin"
log_info "  remote_gopath = ${remote_gopath}"
log_info "  local_bin_dir = ${local_bin_dir}"

for bin in discoverymaster discoveryslave orderingpeer orderingclient; do
  if [ ! -x "${local_bin_dir}/${bin}" ]; then
    log_error "  [LOCAL] ERRO: binário não encontrado: ${local_bin_dir}/${bin}"
    exit 1
  fi
  log_info "  [LOCAL] OK: ${local_bin_dir}/${bin}"
done
log_info "==== [start-remote-slaves] Binários verificados. ===="
echo

#-------------------- Função: garantir ambiente remoto ------------------------

# Esta função:
#  - garante diretórios remotos (/users/Bruno/iss, current-deployment-data, status)
#  - copia binários e scripts se necessário
#  - NÃO faz o script falhar se o SSH der erro; apenas loga o problema.
ensure_remote_slave() {
  local instance_id="$1"
  local ctrl_ip="$2"

  log_info "---------------------------------------------------------------------"
  log_info "  [REMOTO] Garantindo ambiente em ${ctrl_ip}"
  log_info "           instance_id = ${instance_id}"
  log_info "           tag         = ${tag}"
  log_info "---------------------------------------------------------------------"

  # 1) Cria diretórios remotos básicos e marca RUNNING
  ssh -o StrictHostKeyChecking=accept-new "${remote_user}@${ctrl_ip}" "
    mkdir -p '${remote_work_dir}' '${remote_exp_dir}' '${remote_work_dir}/status' &&
    echo RUNNING > '${remote_work_dir}/status/${instance_id}.status' &&
    rm -f '${remote_work_dir}/master.ready'
  " || {
    log_error "  [REMOTO] ERRO ao criar diretórios em ${ctrl_ip} (instance_id=${instance_id})."
    return 1
  }

  # 2) Copia binários (se ainda não estiverem lá)
  scp -o StrictHostKeyChecking=accept-new \
    "${local_bin_dir}/discoverymaster" \
    "${local_bin_dir}/discoveryslave" \
    "${local_bin_dir}/orderingpeer" \
    "${local_bin_dir}/orderingclient" \
    "${remote_user}@${ctrl_ip}:${remote_bin_dir}/" >/dev/null 2>&1 || {
      log_warn "  [REMOTO] Aviso: não foi possível copiar binários para ${ctrl_ip}; assumindo que já existem."
  }

  # 3) Copia scripts (start-slave.sh etc.)
  scp -o StrictHostKeyChecking=accept-new \
    "${local_start_slave_script}" \
    "${remote_user}@${ctrl_ip}:${remote_work_dir}/" >/dev/null 2>&1 || {
      log_warn "  [REMOTO] Aviso: não foi possível copiar start-slave.sh para ${ctrl_ip}; assumindo que já existe."
  }

  scp -r -o StrictHostKeyChecking=accept-new \
    "${local_scripts_dir}" \
    "${remote_user}@${ctrl_ip}:${remote_work_dir}/" >/dev/null 2>&1 || {
      log_warn "  [REMOTO] Aviso: não foi possível copiar diretório scripts/ para ${ctrl_ip}; assumindo que já existe."
  }

  log_info "    [REMOTO] OK: ambiente garantido em ${ctrl_ip}."
}

#-------------------- Marca slaves que serão usados ---------------------------

log_info "==== [start-remote-slaves] (REMOTO) Garantindo binários e scripts ===="

# args restantes são grupos de 5: instance_id, ctrl_ip, data_ip, role, tag
used=0
args=( "$@" )
total="${#args[@]}"

i=0
while [ "${i}" -lt "${total}" ]; do
  instance_id="${args[$i]}";   i=$((i+1))
  ctrl_ip="${args[$i]}";       i=$((i+1))
  data_ip="${args[$i]}";       i=$((i+1))
  role="${args[$i]}";          i=$((i+1))
  itag="${args[$i]}";          i=$((i+1))

  # Só mexe nos slaves com a tag que nos interessa
  if [ "${role}" != "slave" ] || [ "${itag}" != "${tag}" ]; then
    continue
  fi

  ensure_remote_slave "${instance_id}" "${ctrl_ip}" || {
    log_error "  [REMOTO] ERRO ao preparar ambiente em ${ctrl_ip} (instance_id=${instance_id})."
    # NÃO damos exit aqui; tentamos os outros slaves mesmo assim.
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

# Agora realmente dispara os processos remotos via start-slave.sh
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

  exp_dir_for_slave="${exp_data_dir}/experiment-output/0000/slave-${instance_id#node-}"  # exemplo de uso
  # Na prática, o start-slave.sh cuida de criar o subdir certo com base em master-commands.cmd.
  # Aqui só usamos exp_data_dir para manter compatibilidade com a chamada anterior.

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

