#!/usr/bin/env bash
#
# start-slave.sh
#
# Uso:
#   ./start-slave.sh <tag> <master_ip> <public_ip> <private_ip> [<master_port>]
#
#   <tag>         : peers, 1client, etc.
#   <master_ip>   : IP do master (onde roda discoverymaster)
#   <public_ip>   : IP público deste slave (aquele que o master conhece)
#   <private_ip>  : IP privado deste slave (rede de dados, ex: 10.10.1.x)
#   <master_port> : (opcional) porta do discoverymaster
#
# Porta do discoverymaster (sem hard-code):
#   1) se passado como 5º argumento, usa esse;
#   2) senão, se MASTER_PORT estiver no ambiente, usa esse;
#   3) senão, descobre automaticamente lendo o arquivo READY no master:
#        <REMOTE_WORK_DIR>/READY
#
# O script:
#   - garante REMOTE_WORK_DIR (ex: /users/$USER/iss),
#   - cria config/, current-deployment-data/, experiment-output/,
#   - configura GOPATH/GOBIN/PATH,
#   - sobe discoveryslave apontando para master_ip:master_port.
#

set -euo pipefail

#-------------------- Args --------------------
if [ "$#" -lt 4 ] || [ "$#" -gt 5 ]; then
  echo "Uso: $0 <tag> <master_ip> <public_ip> <private_ip> [<master_port>]" >&2
  exit 1
fi

TAG="$1"
MASTER_IP="$2"
PUBLIC_IP="$3"
PRIVATE_IP="$4"

#-------------------- Usuário / diretórios base (genéricos) --------------------

# Usuário remoto (por padrão, o próprio usuário atual)
REMOTE_USER="${REMOTE_USER:-${USER:-$(whoami)}}"

# Home remoto (por padrão, o $HOME da conta que está rodando)
REMOTE_HOME="${REMOTE_HOME:-${HOME}}"

# Diretório de trabalho do ISS (sem "Bruno" cravado)
REMOTE_WORK_DIR="${REMOTE_WORK_DIR:-${REMOTE_HOME}/iss}"

# GOPATH e GOBIN também genéricos
REMOTE_GOPATH="${REMOTE_GOPATH:-${REMOTE_HOME}/go}"
REMOTE_GOBIN="${REMOTE_GOBIN:-${REMOTE_GOPATH}/bin}"

# Scripts (copiados pelo start-remote-slaves.sh)
SCRIPTS_DIR="${REMOTE_WORK_DIR}/scripts"
DEPLOY_SCRIPTS_DIR="${REMOTE_WORK_DIR}/deployment/scripts"

# Diretório de logs (visível e genérico)
REMOTE_LOGS_DIR="${REMOTE_LOGS_DIR:-${REMOTE_HOME}/iss-logs}"
LOG_FILE="${REMOTE_LOGS_DIR}/start-slave-${TAG}.log"

mkdir -p "${REMOTE_WORK_DIR}" "${REMOTE_LOGS_DIR}"

#-------------------- Descoberta da porta do master (sem hard-code) --------------------
#
# Ordem de resolução:
#   1. 5º argumento -> MASTER_PORT explícito
#   2. variável de ambiente MASTER_PORT
#   3. arquivo READY no master (REMOTE_WORK_DIR/READY)
#

if [ "$#" -ge 5 ]; then
  MASTER_PORT="$5"
elif [ -n "${MASTER_PORT:-}" ]; then
  MASTER_PORT="${MASTER_PORT}"
else
  echo "[$(date '+%Y-%m-%d %H:%M:%S')] start-slave.sh: descobrindo porta do discoverymaster no master (${MASTER_IP})..." >> "${LOG_FILE}"

  MASTER_READY_PATH="${REMOTE_WORK_DIR}/READY"

  # copia READY do master -> /tmp/READY.<pid>
  READY_TMP="/tmp/READY.$$"
  if ! scp -q "${REMOTE_USER}@${MASTER_IP}:${MASTER_READY_PATH}" "${READY_TMP}" ; then
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] ERRO: não consegui copiar ${MASTER_READY_PATH} de ${REMOTE_USER}@${MASTER_IP} para descobrir a porta." >> "${LOG_FILE}"
    echo "ERRO: não foi possível descobrir MASTER_PORT automaticamente (READY ausente ou inacessível)." >&2
    exit 1
  fi

  # arquivo READY deve conter algo como:
  #   MASTER_IP=...
  #   MASTER_PORT=...
  # usamos 'source' pra carregar essas variáveis
  # shellcheck source=/dev/null
  source "${READY_TMP}"
  rm -f "${READY_TMP}"

  if [ -z "${MASTER_PORT:-}" ]; then
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] ERRO: READY não definiu MASTER_PORT." >> "${LOG_FILE}"
    echo "ERRO: arquivo READY não contém MASTER_PORT válido." >&2
    exit 1
  fi

  echo "[$(date '+%Y-%m-%d %H:%M:%S')] start-slave.sh: MASTER_PORT detectado automaticamente = ${MASTER_PORT}" >> "${LOG_FILE}"
fi

#-------------------- PATH / env --------------------

export GOPATH="${REMOTE_GOPATH}"
export GOBIN="${REMOTE_GOBIN}"
export PATH="${REMOTE_GOBIN}:/usr/local/go/bin:${SCRIPTS_DIR}:${DEPLOY_SCRIPTS_DIR}:${REMOTE_WORK_DIR}:${PATH}"

{
  echo "[$(date '+%Y-%m-%d %H:%M:%S')] start-slave.sh: início"
  echo "  REMOTE_USER    = ${REMOTE_USER}"
  echo "  TAG            = ${TAG}"
  echo "  MASTER_IP      = ${MASTER_IP}"
  echo "  MASTER_PORT    = ${MASTER_PORT}"
  echo "  PUBLIC_IP      = ${PUBLIC_IP}"
  echo "  PRIVATE_IP     = ${PRIVATE_IP}"
  echo "  REMOTE_HOME    = ${REMOTE_HOME}"
  echo "  REMOTE_WORK_DIR= ${REMOTE_WORK_DIR}"
  echo "  REMOTE_GOPATH  = ${REMOTE_GOPATH}"
  echo "  REMOTE_GOBIN   = ${REMOTE_GOBIN}"
  echo "  SCRIPTS_DIR    = ${SCRIPTS_DIR}"
  echo "  DEPLOY_SCRIPTS = ${DEPLOY_SCRIPTS_DIR}"
  echo "  REMOTE_LOGS_DIR= ${REMOTE_LOGS_DIR}"
  echo "  PATH           = ${PATH}"
} >> "${LOG_FILE}"

cd "${REMOTE_WORK_DIR}"

#-------------------- Diretórios exigidos pelo ISS --------------------

mkdir -p \
  "${REMOTE_WORK_DIR}/config" \
  "${REMOTE_WORK_DIR}/current-deployment-data" \
  "${REMOTE_WORK_DIR}/experiment-output"

echo "[$(date '+%Y-%m-%d %H:%M:%S')] start-slave.sh: diretórios garantidos (config/, current-deployment-data/, experiment-output/)" >> "${LOG_FILE}"

#-------------------- Sanidade: binário discoveryslave --------------------

if [ ! -x "${REMOTE_GOBIN}/discoveryslave}" ] && [ ! -x "${REMOTE_GOBIN}/discoveryslave" ]; then
  echo "[$(date '+%Y-%m-%d %H:%M:%S')] ERRO: discoveryslave não encontrado em ${REMOTE_GOBIN}." >> "${LOG_FILE}"
  exit 1
fi

#-------------------- Inicia discoveryslave --------------------

echo "[$(date '+%Y-%m-%d %H:%M:%S')] start-slave.sh: iniciando discoveryslave..." >> "${LOG_FILE}"
echo "[$(date '+%Y-%m-%d %H:%M:%S')] start-slave.sh: comando = ${REMOTE_GOBIN}/discoveryslave \"${TAG}\" \"${MASTER_IP}:${MASTER_PORT}\" \"${PUBLIC_IP}\" \"${PRIVATE_IP}\"" >> "${LOG_FILE}"

nohup "${REMOTE_GOBIN}/discoveryslave" \
  "${TAG}" \
  "${MASTER_IP}:${MASTER_PORT}" \
  "${PUBLIC_IP}" \
  "${PRIVATE_IP}" \
  >> "${LOG_FILE}" 2>&1 &

echo "[$(date '+%Y-%m-%d %H:%M:%S')] start-slave.sh: discoveryslave iniciado em background (PID=$!)." >> "${LOG_FILE}"

