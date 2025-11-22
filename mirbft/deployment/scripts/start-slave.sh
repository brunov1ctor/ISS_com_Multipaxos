#!/usr/bin/env bash

set -euo pipefail

# Uso:
#   start-slave.sh <tag> <masterIP> <publicIP> <privateIP>
#
# Exemplo típico:
#   start-slave.sh peers 172.19.135.1 172.19.135.2 10.10.1.2

if [ "$#" -ne 4 ]; then
  echo "Uso: $0 <tag> <masterIP> <publicIP> <privateIP>" >&2
  exit 1
fi

TAG="$1"
MASTER_IP="$2"
PUBLIC_IP="$3"
PRIVATE_IP="$4"

# O deploy-remote.sh exporta status_file=$remote_status_file.
# Se não vier nada (teste manual), usamos um padrão.
STATUS_FILE="${status_file:-/users/$USER/iss/status-$TAG}"

BASE_DIR="/users/$USER/iss"
LOG_FILE="$BASE_DIR/start-slave-$TAG.log"

log() {
  echo "[$(date '+%F %T')] $*" | tee -a "$LOG_FILE"
}

log "==============================================="
log "Iniciando start-slave.sh"
log "TAG=$TAG MASTER_IP=$MASTER_IP PUBLIC_IP=$PUBLIC_IP PRIVATE_IP=$PRIVATE_IP"
log "STATUS_FILE=$STATUS_FILE"
log "PWD inicial: $(pwd)"

# Garante PATH com os binários Go (discoveryslave, orderingpeer, etc.)
export PATH="$PATH:/users/$USER/go/bin"

# Marca status 0 (inicializando)
log "Marcando STATUS=0 em $STATUS_FILE"
echo 0 > "$STATUS_FILE"

# Gera/atualiza certificados TLS para este nó
cd "$BASE_DIR/tls-data"
log "Executando TLS generate.sh para $PUBLIC_IP / $PRIVATE_IP"
./generate.sh "$PUBLIC_IP" "$PRIVATE_IP" >>"$LOG_FILE" 2>&1 || {
  log "ERRO ao executar generate.sh"
  exit 1
}

# Volta para o diretório base
cd "$BASE_DIR"
log "Diretório base: $(pwd)"

# Sobe o discoveryslave em background
log "Subindo discoveryslave..."
log "Comando: discoveryslave $TAG ${MASTER_IP}:9999 $PUBLIC_IP $PRIVATE_IP"

# Redireciona saída do discoveryslave para o mesmo log
discoveryslave "$TAG" "${MASTER_IP}:9999" "$PUBLIC_IP" "$PRIVATE_IP" >>"$LOG_FILE" 2>&1 &

SLAVE_PID=$!
log "discoveryslave iniciado com PID=$SLAVE_PID"

# Espera um pouco para dar tempo de registrar no master
sleep 5

# Se o processo ainda estiver vivo, consideramos OK e marcamos STATUS=1
if kill -0 "$SLAVE_PID" 2>/dev/null; then
  log "discoveryslave ainda rodando, marcando STATUS=1"
  echo 1 > "$STATUS_FILE"
else
  log "discoveryslave morreu logo após iniciar. Mantendo STATUS=0"
  echo 0 > "$STATUS_FILE"
fi

log "start-slave.sh finalizado"
exit 0

