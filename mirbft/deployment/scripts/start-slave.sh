#!/usr/bin/env bash
#
# start-slave.sh
#
# Script executado em CADA slave (via start-remote-slaves.sh).
# Responsabilidade:
#   - Ajustar PATH (Go + scripts do ISS).
#   - Garantir diretório de logs.
#   - Subir o discoveryslave adequado (peers ou 1client) apontando para o master.
#
# Quem cuida de:
#   - enviar master-commands.cmd,
#   - disparar orderingpeer/orderingclient,
#   - usar stubborn-scp.sh para buscar config,
# é o MASTER (via discovery + master-commands).
#
# Argumentos:
#   $1 = tag       (peers | 1client)
#   $2 = master_ip (IP público do master, ex: 172.19.124.1)
#   $3 = public_ip (IP público deste nó)
#   $4 = private_ip (IP privado deste nó, ex: 10.10.1.X)
#

set -euo pipefail

if [[ $# -lt 4 ]]; then
  echo "Uso: $0 <tag> <master_ip> <public_ip> <private_ip>" >&2
  exit 1
fi

tag="$1"
master_ip="$2"
public_ip="$3"
private_ip="$4"

# ----------------------------------------------------------------------
# Caminhos fixos (iguais aos usados no restante do deploy)
# ----------------------------------------------------------------------
remote_gopath="/users/Bruno/go"
remote_bin_dir="${remote_gopath}/bin"
remote_work_dir="/users/Bruno/iss"
remote_logs_dir="/users/Bruno/iss-logs"

# ----------------------------------------------------------------------
# Função de log simples (vai pra stderr, útil nos logs de SSH)
# ----------------------------------------------------------------------
log() {
  echo "[$(date '+%Y-%m-%d %H:%M:%S')] [start-slave] $*" >&2
}

log "Iniciando start-slave com:"
log "  tag       = ${tag}"
log "  master_ip = ${master_ip}"
log "  public_ip = ${public_ip}"
log "  private_ip= ${private_ip}"

# ----------------------------------------------------------------------
# Ajuste de PATH / ambiente
# ----------------------------------------------------------------------
export GOPATH="${remote_gopath}"
export GOBIN="${remote_bin_dir}"

# PATH:
#   - Go binários (discoverymaster, discoveryslave, orderingpeer, orderingclient)
#   - /usr/local/go/bin (caso o Go esteja aqui)
#   - scripts do ISS (incluindo stubborn-scp.sh)
#   - scripts de deployment, se precisarem ser invocados indiretamente
export PATH="${GOBIN}:/usr/local/go/bin:${remote_work_dir}:${remote_work_dir}/scripts:${remote_work_dir}/deployment/scripts:${PATH}"

log "PATH configurado: ${PATH}"

# ----------------------------------------------------------------------
# Garante diretório de logs no slave
# ----------------------------------------------------------------------
mkdir -p "${remote_logs_dir}"

# ----------------------------------------------------------------------
# Sobe o discoveryslave correto (peers ou 1client)
# ----------------------------------------------------------------------
case "${tag}" in
  peers|1client)
    log "Subindo discoveryslave para tag='${tag}' com master=${master_ip}:9999"

    # Importante: rodar em background e manter log separado
    # para não depender da sessão SSH que disparou o start-slave.sh.
    nohup discoveryslave "${tag}" "${master_ip}:9999" "${public_ip}" "${private_ip}" \
      > "${remote_logs_dir}/discoveryslave-${tag}.log" 2>&1 &

    ds_pid=$!
    log "discoveryslave iniciado com PID=${ds_pid}, log em ${remote_logs_dir}/discoveryslave-${tag}.log"
    ;;

  *)
    log "ERRO: tag desconhecida '${tag}'. Esperado: 'peers' ou '1client'."
    exit 1
    ;;
esac

log "start-slave.sh concluído com sucesso para tag='${tag}'."
exit 0

