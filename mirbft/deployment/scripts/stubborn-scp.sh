#!/usr/bin/env bash
#
# Versão corrigida para uso no ISS/Emulab
# - ignora "-i" inválido enviado automaticamente pelo ISS
# - cria diretório de destino automaticamente
# - executa scp corretamente com retry
#

set -euo pipefail

log() {
  echo "[$(date '+%Y-%m-%d %H:%M:%S')] $*"
}

if [[ $# -lt 3 ]]; then
  log "Uso: $0 <tentativas> [opções scp...] <origem> <destino>"
  exit 1
fi

retries="$1"
shift

# O ISS manda algo tipo:
#   stubborn-scp.sh 10 -i 172.20.3.2:iss/... config/config.yml
# Esse "-i" sozinho quebra o scp → ignoramos
if [[ "$#" -ge 1 && "$1" == "-i" ]]; then
  shift
fi

if [[ "$#" -lt 2 ]]; then
  log "Erro: argumentos insuficientes (origem/destino)"
  exit 1
fi

src="${1}"
dst="${2}"

# Criar diretório de destino (ex.: config/)
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

