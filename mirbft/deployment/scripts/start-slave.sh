#!/bin/bash

# Script de inicialização do slave (peer ou client) para o ISS no Emulab.

# Parâmetros:
#   $1 = TAG         (ex.: "peers", "1client")
#   $2 = MASTER_IP   (ex.: 172.19.135.1)
#   $3 = PUBLIC_IP   (ex.: 172.19.135.2)
#   $4 = PRIVATE_IP  (ex.: 10.10.1.2)

TAG="$1"
MASTER_IP="$2"
PUBLIC_IP="$3"
PRIVATE_IP="$4"

# Diretório base no nó remoto onde o ISS está sendo executado
BASE_DIR="/users/Bruno/iss"

# Arquivo de log local (no nó remoto)
LOG_FILE="$BASE_DIR/start-slave-$TAG.log"

# Função de log com timestamp
log() {
  echo "[$(date '+%Y-%m-%d %H:%M:%S')] $*"
}

# Redireciona stdout/stderr para o log
mkdir -p "$BASE_DIR" 2>/dev/null
exec >>"$LOG_FILE" 2>&1

log "==============================================="
log "Iniciando start-slave.sh"
log "TAG=$TAG MASTER_IP=$MASTER_IP PUBLIC_IP=$PUBLIC_IP PRIVATE_IP=$PRIVATE_IP"

# Arquivo de status
if [ "$TAG" = "peers" ]; then
  STATUS_FILE="$BASE_DIR/status-peers"
else
  STATUS_FILE="$BASE_DIR/status"
fi

log "STATUS_FILE=$STATUS_FILE"
log "PWD inicial: $(pwd)"

# Marca STATUS=0 (inicializando)
echo "0" > "$STATUS_FILE" 2>/dev/null || true
log "Marcando STATUS=0 em $STATUS_FILE"

########################################
# Configuração de ambiente / PATH
########################################

# GOPATH padrão que você está usando
export GOPATH="/users/Bruno/go"

# Adiciona:
#   - $GOPATH/bin         (onde ficam os binários Go instalados)
#   - /usr/local/go/bin   (caso necessário)
#   - diretório dos scripts de deployment (p/ stubborn-scp.sh)
export PATH="$GOPATH/bin:/usr/local/go/bin:/users/Bruno/go/src/github.com/hyperledger-labs/mirbft/deployment/scripts:$PATH"

log "PATH=$PATH"

########################################
# Geração de certificados TLS
########################################

TLS_DIR="$BASE_DIR/tls-data"

if [ -d "$TLS_DIR" ] && [ -x "$TLS_DIR/generate.sh" ]; then
  log "Executando TLS generate.sh para $PUBLIC_IP / $PRIVATE_IP"
  (
    cd "$TLS_DIR" || exit 1
    ./generate.sh "$PUBLIC_IP" "$PRIVATE_IP"
  )
else
  log "Aviso: diretório TLS ou script generate.sh não encontrado em $TLS_DIR"
fi

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

