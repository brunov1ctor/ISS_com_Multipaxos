#!/usr/bin/env bash
#
# start-slave.sh
#
# Uso:
#   ./start-slave.sh <tag> <master_ip> <public_ip> <private_ip>
#
# Exemplo (como chamado pelo start-remote-slaves.sh):
#   ./start-slave.sh peers   172.19.143.1  172.19.143.2  10.10.1.2
#   ./start-slave.sh 1client 172.19.143.1  172.19.143.7  10.10.1.7
#
# Ele apenas inicia o discoveryslave apontando para o discoverymaster
# do master. O master, por meio de master-commands.cmd, usa comandos
# "exec-start" para mandar os slaves rodarem orderingpeer/orderingclient
# e gerarem os diretórios experiment-output/..., além de empacotar e
# enviar os resultados de volta para o master.
#

set -euo pipefail

if [[ $# -lt 4 ]]; then
  echo "Uso: $0 <tag> <master_ip> <public_ip> <private_ip>" >&2
  exit 1
fi

TAG="$1"         # peers | 1client | outro tag definido em master-commands
MASTER_IP="$2"   # IP público do master (ex: 172.19.143.1)
PUBLIC_IP="$3"   # IP público deste node (ex: 172.19.143.2)
PRIVATE_IP="$4"  # IP da rede de experimento (ex: 10.10.1.2)

###############################################################################
# Diretórios e ambiente
###############################################################################

# HOME no Emulab normalmente é /users/<usuario>, mas usamos $HOME se existir.
REMOTE_HOME="${HOME:-/users/Bruno}"

REMOTE_WORK_DIR="${REMOTE_HOME}/iss"
REMOTE_LOGS_DIR="${REMOTE_HOME}/iss-logs"
REMOTE_GOPATH="${REMOTE_HOME}/go"
REMOTE_GOBIN="${REMOTE_GOPATH}/bin"

# Porta do discoverymaster (vem do global-vars.sh, mas fixamos aqui também)
MASTER_PORT="${MASTER_PORT:-9999}"

# Garante diretórios base
mkdir -p "${REMOTE_WORK_DIR}" \
         "${REMOTE_LOGS_DIR}" \
         "${REMOTE_WORK_DIR}/config" \
         "${REMOTE_WORK_DIR}/experiment-output"

export GOPATH="${REMOTE_GOPATH}"
export GOBIN="${REMOTE_GOBIN}"

# IMPORTANTE:
# inclui também os diretórios de scripts no PATH para que
# "stubborn-scp.sh" (e outros) sejam encontrados pelos comandos exec-start
export PATH="${REMOTE_GOBIN}:/usr/local/go/bin:${REMOTE_WORK_DIR}/scripts:${REMOTE_WORK_DIR}/deployment/scripts:${PATH}"

cd "${REMOTE_WORK_DIR}"

LOG_FILE="${REMOTE_LOGS_DIR}/start-slave-${TAG}.log"

mkdir -p "$(dirname "${LOG_FILE}")"

{
  echo "[$(date '+%Y-%m-%d %H:%M:%S')] start-slave.sh: iniciado."
  echo "  TAG        = ${TAG}"
  echo "  MASTER_IP  = ${MASTER_IP}"
  echo "  PUBLIC_IP  = ${PUBLIC_IP}"
  echo "  PRIVATE_IP = ${PRIVATE_IP}"
  echo "  REMOTE_HOME      = ${REMOTE_HOME}"
  echo "  REMOTE_WORK_DIR  = ${REMOTE_WORK_DIR}"
  echo "  REMOTE_LOGS_DIR  = ${REMOTE_LOGS_DIR}"
  echo "  REMOTE_GOPATH    = ${REMOTE_GOPATH}"
  echo "  REMOTE_GOBIN     = ${REMOTE_GOBIN}"
  echo "  MASTER_PORT      = ${MASTER_PORT}"
  echo "  PATH             = ${PATH}"
  echo "  Verificando diretórios importantes..."
  ls -ld "${REMOTE_WORK_DIR}" || echo "  (faltando REMOTE_WORK_DIR?)"
  ls -ld "${REMOTE_WORK_DIR}/config" || echo "  (faltando config?)"
  ls -ld "${REMOTE_WORK_DIR}/experiment-output" || echo "  (faltando experiment-output?)"
} >> "${LOG_FILE}" 2>&1

# Para debugging: mostra se o discoveryslave existe
if [[ ! -x "${REMOTE_GOBIN}/discoveryslave" ]]; then
  echo "[$(date '+%Y-%m-%d %H:%M:%S')] start-slave.sh: ERRO - discoveryslave não encontrado em ${REMOTE_GOBIN}" >> "${LOG_FILE}"
  exit 1
fi

###############################################################################
# Inicia o discoveryslave
###############################################################################
# Inicia o discoveryslave em background. É ele que conversa com o discoverymaster
# e recebe comandos "exec-start" que vão criar:
#   experiment-output/<exp_id>/slave-XXX/peer.trc
#   experiment-output/<exp_id>/slave-XXX/prof
# e depois empacotar/enviar esses arquivos para o master.
nohup "${REMOTE_GOBIN}/discoveryslave" \
  "${TAG}" \
  "${MASTER_IP}:${MASTER_PORT}" \
  "${PUBLIC_IP}" \
  "${PRIVATE_IP}" \
  >> "${LOG_FILE}" 2>&1 &

echo "[$(date '+%Y-%m-%d %H:%M:%S')] start-slave.sh: discoveryslave iniciado em background." >> "${LOG_FILE}"

