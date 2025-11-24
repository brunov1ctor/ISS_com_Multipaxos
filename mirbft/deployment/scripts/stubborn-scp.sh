#!/usr/bin/env bash
#
# stubborn-scp.sh - versão simplificada para o ISS/Emulab
# Formato esperado de chamada:
#   stubborn-scp.sh <tentativas> -i <origem_remota> <destino_local>
# Exemplo real:
#   stubborn-scp.sh 10 -i 172.20.3.2:iss/experiment-config/config-0000.yml config/config.yml
#

set -euo pipefail

log() {
  echo "[$(date '+%Y-%m-%d %H:%M:%S')] $*"
}

if [[ $# -lt 4 ]]; then
  log "Uso esperado: $0 <tentativas> -i <origem_remota> <destino_local>"
  exit 1
fi

retries="$1"
shift

# Agora esperamos: -i SRC DST
dummy_flag="$1"   # normalmente "-i" (ignoramos)
src="$2"
dst="$3"

# Garante diretório de destino (ex.: config/)
mkdir -p "$(dirname "$dst")"

attempt=1
status=1

while (( attempt <= retries )); do
  log "Tentativa $attempt/$retries: scp '$src' '$dst'"

  if scp "$src" "$dst"; then
    log "scp concluído com sucesso."
    exit 0
  fi

  status=$?
  log "scp falhou com status $status. Tentando novamente..."
  sleep 1
  attempt=$((attempt + 1))
done

log "Desisti após $retries tentativas. Último status: $status"
exit "$status"

