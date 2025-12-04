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
# Este script só roda no *slave*. Ele:
#   - garante o diretório de trabalho (REMOTE_WORK_DIR),
#   - garante os subdiretórios usados pelo ISS (config/, current-deployment-data/, experiment-output/),
#   - exporta o PATH para achar os binários Go e scripts de deploy,
#   - liga o discoveryslave apontando para o discoverymaster do master.
#
# O discovery/master-commands.cmd é quem depois manda rodar:
#   orderingpeer <config/config.yml> <discovery> <public_ip> <private_ip> \
#                experiment-output/<exp>/slave-__id__/peer.trc \
#                experiment-output/<exp>/slave-__id__/prof
# e também orderingclient, empacotamento de logs, etc.
#

set -euo pipefail

#-------------------- Args --------------------
if [ $# -ne 4 ]; then
  echo "Uso: $0 <tag> <master_ip> <public_ip> <private_ip>" >&2
  exit 1
fi

TAG="$1"
MASTER_IP="$2"
PUBLIC_IP="$3"
PRIVATE_IP="$4"

MASTER_PORT="${MASTER_PORT:-10000}"

#-------------------- Diretórios / PATH --------------------

# Diretório de trabalho padrão no slave (mesmo usado em deploy-remote/start-remote-slaves)
REMOTE_WORK_DIR="${REMOTE_WORK_DIR:-/users/Bruno/iss}"
REMOTE_GOPATH="${REMOTE_GOPATH:-/users/Bruno/go}"
REMOTE_GOBIN="${REMOTE_GOBIN:-${REMOTE_GOPATH}/bin}"

# Onde ficam os scripts copiados pelo start-remote-slaves.sh
SCRIPTS_DIR="${REMOTE_WORK_DIR}/scripts"

LOG_DIR="${REMOTE_WORK_DIR}/logs"
LOG_FILE="${LOG_DIR}/start-slave-${TAG}.log"

mkdir -p "${REMOTE_WORK_DIR}" "${LOG_DIR}"

# Exporta PATH para achar os binários e scripts
export GOPATH="${REMOTE_GOPATH}"
export GOBIN="${REMOTE_GOBIN}"
export PATH="${REMOTE_GOBIN}:/usr/local/go/bin:${SCRIPTS_DIR}:${REMOTE_WORK_DIR}:${PATH}"

{
  echo "[$(date '+%Y-%m-%d %H:%M:%S')] start-slave.sh: início"
  echo "  TAG        = ${TAG}"
  echo "  MASTER_IP  = ${MASTER_IP}"
  echo "  PUBLIC_IP  = ${PUBLIC_IP}"
  echo "  PRIVATE_IP = ${PRIVATE_IP}"
  echo "  REMOTE_WORK_DIR = ${REMOTE_WORK_DIR}"
  echo "  REMOTE_GOBIN    = ${REMOTE_GOBIN}"
  echo "  SCRIPTS_DIR     = ${SCRIPTS_DIR}"
} >> "${LOG_FILE}"

cd "${REMOTE_WORK_DIR}"

#-------------------- Diretórios exigidos pelo ISS --------------------
#
# IMPORTANTE:
#   - pushConfigFiles() (generate-master-commands.py) faz:
#       stubborn-scp.sh ... $own_public_ip:iss/experiment-config/config-000X.yml config/config.yml
#     ou seja, *assume* que o diretório "config/" já existe no slave.
#   - createLogDir() cria experiment-output/<exp>/slave-__id__/ antes de
#     chamar orderingpeer / orderingclient.
#
# Se config/ não existir, o stubborn-scp.sh/scp quebra silenciosamente e
# orderingpeer cai com FATAL ao tentar abrir config/config.yml, sem nunca
# chegar a habilitar tracing, por isso não aparecem *.trc nem perfis.

mkdir -p \
  "${REMOTE_WORK_DIR}/config" \
  "${REMOTE_WORK_DIR}/current-deployment-data" \
  "${REMOTE_WORK_DIR}/experiment-output"

echo "[$(date '+%Y-%m-%d %H:%M:%S')] start-slave.sh: diretórios garantidos (config/, current-deployment-data/, experiment-output/)" >> "${LOG_FILE}"

#-------------------- Sanidade: binário discoveryslave --------------------

if [ ! -x "${REMOTE_GOBIN}/discoveryslave" ]; then
  echo "[$(date '+%Y-%m-%d %H:%M:%S')] ERRO: discoveryslave não encontrado em ${REMOTE_GOBIN}." >> "${LOG_FILE}"
  exit 1
fi

#-------------------- Inicia discoveryslave --------------------

echo "[$(date '+%Y-%m-%d %H:%M:%S')] start-slave.sh: iniciando discoveryslave..." >> "${LOG_FILE}"

nohup "${REMOTE_GOBIN}/discoveryslave" \
  "${TAG}" \
  "${MASTER_IP}:${MASTER_PORT}" \
  "${PUBLIC_IP}" \
  "${PRIVATE_IP}" \
  >> "${LOG_FILE}" 2>&1 &

echo "[$(date '+%Y-%m-%d %H:%M:%S')] start-slave.sh: discoveryslave iniciado em background (PID=$!)." >> "${LOG_FILE}"

