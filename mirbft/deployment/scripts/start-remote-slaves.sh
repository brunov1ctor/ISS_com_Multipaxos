#!/bin/bash
#
# scripts/start-remote-slaves.sh
#
# Dispara slaves remotos de acordo com o deployment e instance-info.
#
# Uso:
#   ./start-remote-slaves.sh <exp_data_dir> <tag> <num> <master_ip> <instance_info_file>
#

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

# Usuário remoto genérico: por padrão o mesmo que está rodando o deploy,
# mas pode ser sobrescrito com REMOTE_SSH_USER no ambiente.
remote_user="${REMOTE_SSH_USER:-$USER}"

log_info()  { echo "[start-remote-slaves][INFO ] $*"; }
log_error() { echo "[start-remote-slaves][ERROR] $*" >&2; }

log_info "==== [start-remote-slaves] Diretórios detectados ====="
log_info "  this_dir       = $(cd "$(dirname "$0")" && pwd)"
log_info "  deployment_dir = $(pwd)"
log_info "  repo_dir       = $(cd .. && pwd)"
log_info "  remote_user    = $remote_user"
log_info "  remote_gopath  = $remote_gopath"
log_info "  remote_bin_dir = $remote_gopath/bin"
log_info "  remote_work_dir= $remote_work_dir"
log_info "  remote_exp_dir = $remote_exp_dir"
log_info

###############################################################################
# 1) Distribui scripts auxiliares e binários para TODOS os slaves
###############################################################################

log_info "==== [start-remote-slaves] Distribuindo scripts/binários aos slaves ===="

while read -r instance_id ctrl_ip data_ip role itag; do
  # Só mexe em máquinas marcadas como "slave" (peers ou 1client).
  if [ "$role" != "slave" ]; then
    continue
  fi

  log_info "---------------------------------------------------------------------"
  log_info "  [REMOTO] Garantindo ambiente em $ctrl_ip"
  log_info "           instance_id = $instance_id"
  log_info "           tag         = $itag"
  log_info "---------------------------------------------------------------------"

  # Garante diretórios básicos em TODOS os slaves
  ssh $ssh_options "${remote_user}@$ctrl_ip" "
    mkdir -p '$remote_work_dir' '$remote_exp_dir' '$remote_gopath/bin' '$remote_work_dir/scripts'
  " || {
    log_error "  [REMOTO] ERRO: não foi possível garantir diretórios em $ctrl_ip."
    continue
  }

  # Copia scripts auxiliares para subdiretório scripts/
  scp $ssh_options \
    scripts/global-vars.sh \
    scripts/remote-machine-status.sh \
    scripts/stubborn-scp.sh \
    "${remote_user}@$ctrl_ip:$remote_work_dir/scripts/" || {
      log_error "  [REMOTO] ERRO ao copiar scripts auxiliares para $ctrl_ip."
      continue
    }

  # Copia binários do GOPATH remoto (assumindo que já foram buildados lá)
  scp $ssh_options \
    "$remote_gopath/bin/discoverymaster" \
    "$remote_gopath/bin/discoveryslave" \
    "$remote_gopath/bin/orderingpeer" \
    "$remote_gopath/bin/orderingclient" \
    "${remote_user}@$ctrl_ip:$remote_gopath/bin/" || {
      log_error "  [REMOTO] ERRO ao copiar binários para $ctrl_ip."
      continue
    }

  log_info "    [REMOTO] OK: ambiente garantido em $ctrl_ip."
done < "$instance_info_file"

log_info "==== [start-remote-slaves] Distribuição concluída. ===="
log_info

###############################################################################
# 2) Dispara os slaves da TAG pedida (peers ou 1client)
###############################################################################

log_info "==== [start-remote-slaves] Disparando slaves da tag '$tag' ===="

started=0

while read -r instance_id ctrl_ip data_ip role itag; do
  if [ "$started" -ge "$num" ]; then
    break
  fi

  if [ "$role" != "slave" ] || [ "$itag" != "$tag" ]; then
    continue
  fi

  log_info "  [DEPLOY] Iniciando slave em ${ctrl_ip} (instance_id=${instance_id}, tag=${itag})"

  # Garante que o diretório de trabalho remoto exista (defensivo)
  ssh $ssh_options "${remote_user}@${ctrl_ip}" "
    mkdir -p '${remote_work_dir}'
  " || {
    log_error "  [DEPLOY] ERRO ao garantir remote_work_dir em ${ctrl_ip}."
    continue
  }

  # Copia SEMPRE o start-slave.sh para o diretório base de trabalho daquele nó,
  # garantindo que exista antes do chmod/exec (evita o erro 'No such file').
  scp $ssh_options \
    scripts/start-slave.sh \
    "${remote_user}@${ctrl_ip}:${remote_work_dir}/" || {
      log_error "  [DEPLOY] ERRO ao copiar start-slave.sh para ${ctrl_ip}."
      continue
    }

  # Executa o start-slave.sh no nó remoto
  ssh $ssh_options "${remote_user}@${ctrl_ip}" "
    cd '${remote_work_dir}' &&
    chmod +x ./start-slave.sh &&
    nohup ./start-slave.sh '${tag}' '${master_ip}' '${ctrl_ip}' '${data_ip}' \
      > '${remote_work_dir}/start-slave-${tag}.log' 2>&1 &
  " || {
    log_error "  [DEPLOY] ERRO ao disparar slave em ${ctrl_ip} (instance_id=${instance_id})."
    continue
  }

  started=$((started+1))
done < "$instance_info_file"

log_info
log_info "==== [start-remote-slaves] Todos os slaves disparados. ===="
log_info "==== [start-remote-slaves] FIM ==========================================="

