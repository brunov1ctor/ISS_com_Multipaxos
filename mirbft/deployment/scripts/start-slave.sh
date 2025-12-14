#!/usr/bin/env bash
set -euo pipefail

# -------------------------------------------------------------------
# start-slave.sh (robusto)
# Uso:
#   ./start-slave.sh <tag> <master_ip> <public_ip> <private_ip|auto|10.10.1.X> <remote_exp_dir>
#
# Exemplo peers:
#   ./start-slave.sh peers 172.20.6.3 172.20.5.2 10.10.1.3 /users/Bruno/iss/current-deployment-data
# Exemplo 1client:
#   ./start-slave.sh 1client 172.20.6.3 172.20.5.6 auto /users/Bruno/iss/current-deployment-data
# -------------------------------------------------------------------

TAG="${1:-}"
MASTER_IP="${2:-}"
PUB_IP="${3:-}"
PRIV_IP="${4:-}"
REMOTE_EXP_DIR="${5:-}"

WORKDIR="/users/Bruno/iss"
BINDIR="/users/Bruno/go/bin"
LOGDIR="${WORKDIR}/logs"
CFGDIR="${WORKDIR}/config"
PIDFILE="${WORKDIR}/.discoveryslave.${TAG}.pid"
SLAVELOG="${LOGDIR}/slave-${TAG}.log"

ts() { date +"%Y-%m-%d %H:%M:%S%z"; }
log() { echo "[$(ts)] $*" | tee -a "$SLAVELOG" >/dev/null; }

die() { log "FATAL: $*"; exit 1; }

[ -n "$TAG" ] || die "tag ausente"
[ -n "$MASTER_IP" ] || die "master_ip ausente"
[ -n "$PUB_IP" ] || die "public_ip ausente"
[ -n "$REMOTE_EXP_DIR" ] || die "remote_exp_dir ausente"

mkdir -p "$LOGDIR" "$CFGDIR" "$REMOTE_EXP_DIR" "$WORKDIR/experiment-output" "$WORKDIR/tmp" >/dev/null 2>&1 || true
touch "$SLAVELOG" >/dev/null 2>&1 || true

log "[start-slave] tag=${TAG} master=${MASTER_IP}:9999 pub=${PUB_IP} priv=${PRIV_IP:-<empty>} expdir=${REMOTE_EXP_DIR}"

# 1) autodetect priv_ip se vier "auto", vazio, ou 10.10.1.X
if [[ -z "${PRIV_IP}" || "${PRIV_IP}" == "auto" || "${PRIV_IP}" == "AUTO" || "${PRIV_IP}" == "10.10.1.X" ]]; then
  DETECTED="$(ip -4 -o addr show | awk '{print $4}' | cut -d/ -f1 | grep -E '^10\.10\.1\.' | head -n1 || true)"
  [ -n "$DETECTED" ] || die "nao consegui detectar IP privado 10.10.1.* (ip -4 addr show)"
  PRIV_IP="$DETECTED"
  log "[start-slave] priv_ip autodetectado: ${PRIV_IP}"
fi

# 2) garantir caminho relativo config/config.yml sempre válido
#    (symlink -> absoluto)
mkdir -p "${CFGDIR}" >/dev/null 2>&1 || true
if [[ ! -e "${CFGDIR}/config.yml" ]]; then
  # não cria conteúdo aqui; só garante o "arquivo alvo" possa existir.
  : > "${CFGDIR}/config.yml" || true
fi
mkdir -p "${WORKDIR}/config" >/dev/null 2>&1 || true
ln -sfn "${CFGDIR}/config.yml" "${WORKDIR}/config/config.yml" || true
log "[start-slave] symlink garantido: ${WORKDIR}/config/config.yml -> ${CFGDIR}/config.yml"

# 3) sanity: binarios existem?
for b in discoveryslave orderingpeer orderingclient; do
  if [[ ! -x "${BINDIR}/${b}" ]]; then
    die "binario ausente ou sem exec: ${BINDIR}/${b}"
  fi
done

# 4) matar processos antigos (só os relevantes)
log "[start-slave] matando processos antigos..."
pkill -9 -f "${BINDIR}/discoveryslave" 2>/dev/null || true
pkill -9 -f "discoveryslave ${TAG} " 2>/dev/null || true
pkill -9 -f "${BINDIR}/orderingpeer" 2>/dev/null || true
pkill -9 -f "${BINDIR}/orderingclient" 2>/dev/null || true
sleep 0.2

# 5) teste de reachability do master:9999
log "[start-slave] testando TCP -> ${MASTER_IP}:9999 ..."
if timeout 2 bash -lc "cat < /dev/null > /dev/tcp/${MASTER_IP}/9999" >/dev/null 2>&1; then
  log "[start-slave] OK_CONNECT ${MASTER_IP}:9999"
else
  log "[start-slave] WARN: FAIL_CONNECT ${MASTER_IP}:9999 (vai tentar subir mesmo assim)"
fi

# 6) subir discoveryslave
log "[start-slave] iniciando discoveryslave (nohup)..."
cd "${WORKDIR}"
nohup "${BINDIR}/discoveryslave" "${TAG}" "${MASTER_IP}:9999" "${PUB_IP}" "${PRIV_IP}" \
  >> "${SLAVELOG}" 2>&1 < /dev/null &
SLAVEPID="$!"
echo "$SLAVEPID" > "$PIDFILE" || true
sleep 0.3

if ps -p "$SLAVEPID" >/dev/null 2>&1; then
  log "[start-slave] discoveryslave rodando pid=${SLAVEPID} (pidfile=${PIDFILE})"
else
  log "[start-slave] ERRO: discoveryslave nao ficou de pe"
  tail -n 120 "${SLAVELOG}" || true
  exit 1
fi

log "[start-slave] pronto."
exit 0

