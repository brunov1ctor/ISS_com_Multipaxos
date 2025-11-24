#!/usr/bin/env bash
#
# stubborn-scp.sh - wrapper "teimoso" para copiar arquivo remoto -> local
# usando ssh + cat (sem usar scp).
#
# Formato esperado de chamada pelo ISS:
#   stubborn-scp.sh <tentativas> -i <origem_remota> <destino_local>
#
# Exemplo real:
#   stubborn-scp.sh 10 -i 172.20.3.2:iss/experiment-config/config-0000.yml config/config.yml
#

set -euo pipefail

log() {
  echo "[$(date '+%Y-%m-%d %H:%M:%S')] $*" >&2
}

if [[ $# -lt 4 ]]; then
  log "Uso esperado: $0 <tentativas> -i <origem_remota> <destino_local>"
  exit 1
fi

retries="$1"
shift

dummy_flag="$1"   # normalmente "-i" (ignoramos)
src="$2"          # ex.: 172.20.3.2:iss/experiment-config/config-0000.yml
dst="$3"          # ex.: config/config.yml

# Separa host e caminho remoto: HOST:PATH
host="${src%%:*}"
remote_path="${src#*:}"

# Garante diretório de destino (ex.: config/)
mkdir -p "$(dirname "$dst")"

attempt=1
status=1

while (( attempt <= retries )); do
  log "Tentativa $attempt/$retries: ssh '$host' 'cat $remote_path' > '$dst'"

  # Copia conteúdo via ssh
  if ssh "$host" "cat '$remote_path'" > "$dst"; then
    log "Cópia concluída com sucesso."
    exit 0
  fi

  status=$?
  log "Falha na cópia (status $status). Tentando novamente..."
  sleep 1
  attempt=$((attempt + 1))
done

log "Desisti após $retries tentativas. Último status: $status"
exit "$status"

