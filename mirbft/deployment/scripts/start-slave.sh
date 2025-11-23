#!/bin/bash

# Script de inicialização do slave (peer ou client) para o ISS no Emulab.

TAG="$1"
MASTER_IP="$2"
PUBLIC_IP="$3"
PRIVATE_IP="$4"

# Diretório base no nó remoto onde o ISS está sendo executado
BASE_DIR="/users/Bruno/iss"

# PATH para encontrar discovery e orderingpeer no nó remoto
export GOPATH="/users/Bruno/go"
export GOROOT="/usr/local/go"
export PATH="$GOPATH/bin:$GOROOT/bin:$BASE_DIR/scripts:$BASE_DIR/deployment/scripts:$PATH"

# Arquivo de log local (no nó remoto)
LOG_FILE="$BASE_DIR/start-slave-$TAG.log"

# Função de log com timestamp
log() {
  echo "[$(date '+%Y-%m-%d %H:%M:%S')] $*"
}

# Garante que o diretório existe e redireciona stdout/stderr para o log
mkdir -p "$BASE_DIR" 2>/dev/null
exec >>"$LOG_FILE" 2>&1

log "==============================================="
log "Iniciando start-slave.sh"
log "TAG=$TAG MASTER_IP=$MASTER_IP PUBLIC_IP=$PUBLIC_IP PRIVATE_IP=$PRIVATE_IP"
log "BASE_DIR=$BASE_DIR"
log "PATH=$PATH"

# Arquivo de status (peers usa status-peers, resto usa status)
if [ "$TAG" = "peers" ]; then
  STATUS_FILE="$BASE_DIR/status-peers"
else
  STATUS_FILE="$BASE_DIR/status"
fi

log "STATUS_FILE=$STATUS_FILE"

########################################
# Inicializa o discoveryslave
########################################

log "Diretório base: $BASE_DIR"
cd "$BASE_DIR" || exit 1

log "Subindo discoveryslave..."
CMD="discoveryslave $TAG ${MASTER_IP}:9999 $PUBLIC_IP $PRIVATE_IP"
log "Comando: $CMD"

$CMD &
DISCOVERY_PID=$!
log "discoveryslave iniciado com PID=$DISCOVERY_PID"

# Espera alguns segundos para ver se o processo morreu ou continua vivo
sleep 5

if kill -0 "$DISCOVERY_PID" 2>/dev/null; then
  echo "1" > "$STATUS_FILE" 2>/dev/null || true
  log "discoveryslave ainda rodando, marcando STATUS=1"
else
  echo "0" > "$STATUS_FILE" 2>/dev/null || true
  log "discoveryslave não está rodando, STATUS=0"
fi

log "start-slave.sh finalizado"

