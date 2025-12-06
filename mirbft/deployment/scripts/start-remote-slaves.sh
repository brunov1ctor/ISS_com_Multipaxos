#!/bin/bash
#
# scripts/start-remote-slaves.sh
#
# Dispara slaves remotos de acordo com o deployment e instance-info.
#
# Uso:
#   ./start-remote-slaves.sh <exp_data_dir> <tag> <num> <master_ip> <instance_info_file>
#

set -e

source scripts/global-vars.sh

if [ $# -lt 5 ]; then
  echo "Uso: $0 <exp_data_dir> <tag> <num> <master_ip> <instance_info_file>"
  exit 1
fi

exp_data_dir="$1"
tag="$2"
num="$3"
master_ip="$4"
instance_info_file="$5"

log_info()  { echo "[start-remote-slaves][INFO ] $*"; }
log_error() { echo "[start-remote-slaves][ERROR] $*" >&2; }

log_info "==== [start-remote-slaves] Diretórios detectados ====="
log_info "  this_dir       = $(cd "$(dirname "$0")" && pwd)"
log_info "  deployment_dir = $(pwd)"
log_info "  repo_dir       = $(cd .. && pwd)"
log_info "  remote_user    = Bruno"
log_info "  remote_gopath  = $remote_gopath"
log_info "  remote_bin_dir = $remote_gopath/bin"
log_info "  remote_work_dir= $remote_work_dir"
log_info "  remote_exp_dir = $remote_exp_dir"
log_info

###############################################################################
# Verifica binários localmente (para evitar deploy quebrado)
###############################################################################

log_info "==== [start-remote-slaves] (LOCAL) Verificando binários em \$GOPATH/bin ===="
log_info "  remote_gopath = $remote_gopath"
local_bin_dir="$remote_gopath/bin"
log_info "  local_bin_dir = $local_bin_dir"

for bin in discoverymaster discoveryslave orderingpeer orderingclient; do
  if [ -x "$local_bin_dir/$bin" ]; then
    log_info "  [LOCAL] OK: $local_bin_dir/$bin"
  else
    log_error "  [LOCAL] ERRO: binário não encontrado ou não executável: $local_bin_dir/$bin"
    exit 1
  fi
done
log_info "==== [start-remote-slaves] Binários verificados. ===="
log_info

###############################################################################
# Garante ambiente remoto (binários + scripts) em cada slave
###############################################################################

log_info "==== [start-remote-slaves] (REMOTO) Garantindo binários e scripts ===="

started=0

# Lê instance-info e filtra pelo tag pedido
while read -r line; do
  [[ -z "$line" || "$line" =~ ^# ]] && continue

  host=$(echo "$line"     | sed 's/.*host=\([^ ]*\).*/\1/')
  ctrl_ip=$(echo "$line"  | sed 's/.*ctrl_ip=\([^ ]*\).*/\1/')
  data_ip=$(echo "$line"  | sed 's/.*data_ip=\([^ ]*\).*/\1/')
  role=$(echo "$line"     | sed 's/.*role=\([^ ]*\).*/\1/')
  itag=$(echo "$line"     | sed 's/.*tag=\([^ ]*\).*/\1/')
  instance_id="$host"

  if [ "$role" != "slave" ]; then
    continue
  fi

  if [ "$itag" != "$tag" ]; then
    continue
  fi

  log_info "---------------------------------------------------------------------"
  log_info "  [REMOTO] Garantindo ambiente em $ctrl_ip"
  log_info "           instance_id = $instance_id"
  log_info "           tag         = $itag"
  log_info "---------------------------------------------------------------------"

  # Garante diretórios e copia scripts/binários
  ssh $ssh_options "Bruno@$ctrl_ip" "
    mkdir -p '$remote_work_dir' '$remote_exp_dir'
  " || {
    log_error "  [REMOTO] ERRO: não foi possível garantir diretórios em $ctrl_ip."
    continue
  }

  # Copia scripts necessários
  scp $ssh_options \
    scripts/start-slave.sh \
    scripts/global-vars.sh \
    scripts/remote-machine-status.sh \
    scripts/stubborn-scp.sh \
    "Bruno@$ctrl_ip:$remote_work_dir/scripts/" || {
      log_error "  [REMOTO] ERRO ao copiar scripts para $ctrl_ip."
      continue
    }

  # Copia binários (se quiser garantir binários idênticos em todos)
  scp $ssh_options \
    "$remote_gopath/bin/discoverymaster" \
    "$remote_gopath/bin/discoveryslave" \
    "$remote_gopath/bin/orderingpeer" \
    "$remote_gopath/bin/orderingclient" \
    "Bruno@$ctrl_ip:$remote_gopath/bin/" || {
      log_error "  [REMOTO] ERRO ao copiar binários para $ctrl_ip."
      continue
    }

  log_info "    [REMOTO] OK: ambiente garantido em $ctrl_ip."
done < "$instance_info_file"

log_info "==== [start-remote-slaves] Distribuição concluída. ===="
log_info

###############################################################################
# Dispara os slaves da TAG pedida
###############################################################################

log_info "==== [start-remote-slaves] Iniciando slaves da TAG '$tag' (n = $num) ===="

started=0

# Precisamos re-iterar para disparar na ordem certa
mapfile -t lines < <(grep -v '^\s*#' "$instance_info_file" | grep -v '^\s*$')

i=0
while [ $started -lt "$num" ] && [ $i -lt "${#lines[@]}" ]; do
  line="${lines[$i]}"
  i=$((i+1))

  host=$(echo "$line"     | sed 's/.*host=\([^ ]*\).*/\1/')
  ctrl_ip=$(echo "$line"  | sed 's/.*ctrl_ip=\([^ ]*\).*/\1/')
  data_ip=$(echo "$line"  | sed 's/.*data_ip=\([^ ]*\).*/\1/')
  role=$(echo "$line"     | sed 's/.*role=\([^ ]*\).*/\1/')
  itag=$(echo "$line"     | sed 's/.*tag=\([^ ]*\).*/\1/')
  instance_id="$host"

  if [ "$role" != "slave" ] || [ "$itag" != "$tag" ]; then
    continue
  fi

  log_info "  [DEPLOY] Iniciando slave em ${ctrl_ip} (instance_id=${instance_id}, tag=${itag})"

  # Chamada CORRETA para start-slave.sh:
  #   ./start-slave.sh <tag> <master_ip> <public_ip> <private_ip>
  # Rodamos em background no slave via nohup, para o SSH não ficar preso.
  ssh -o StrictHostKeyChecking=accept-new "${remote_user}@${ctrl_ip}" "
    cd '${remote_work_dir}' &&
    chmod +x ./start-slave.sh &&
    nohup ./start-slave.sh '${tag}' '${master_ip}' '${ctrl_ip}' '${data_ip}' \
      > '${remote_work_dir}/start-slave-${tag}.log' 2>&1 &
  " || {
    log_error "  [DEPLOY] ERRO ao disparar slave em ${ctrl_ip} (instance_id=${instance_id})."
    continue
  }

  started=$((started+1))
done

log_info
log_info "==== [start-remote-slaves] Todos os slaves disparados. ===="
log_info "==== [start-remote-slaves] FIM ==========================================="

