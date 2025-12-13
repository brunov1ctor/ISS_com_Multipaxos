#!/usr/bin/env bash

#
# start-slave.sh
#
# Script executado DENTRO da máquina remota (slave) para:
#   - inicializar discoveryslave
#   - aguardar o master ficar READY
#   - registrar logs detalhados (para evitar "sucesso" falso)
#

set -euo pipefail

ts() { date +"%Y-%m-%d %H:%M:%S"; }
log(){ echo "[start-slave][$(ts)] $*"; }
warn(){ log "WARN: $*"; }
die(){ log "ERRO: $*"; exit 1; }

if [[ $# -lt 5 ]]; then
  echo "Uso: $0 <tag> <master_ip> <public_ip> <private_ip> <remote_exp_dir>" >&2
  exit 2
fi

tag="$1"
master_ip="$2"
public_ip="$3"
private_ip="$4"
remote_exp_dir_arg="$5"

this_dir="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"

# IMPORTANTE:
# Não fixe remote_work_dir antes de dar source no global-vars.sh.
# Se remote_work_dir estiver setado, global-vars.sh respeita o valor existente.
unset remote_work_dir || true

# Usuário remoto usado nos SSH de volta para o master.
remote_user="${DEPL_REMOTE_USER:-$USER}"

# shellcheck source=/dev/null
source "${this_dir}/global-vars.sh"

# Se start-remote-slaves passou um remote_exp_dir explícito, respeitar.
remote_exp_dir="${remote_exp_dir_arg:-${remote_exp_dir}}"

# Diretórios mínimos (nunca falhar aqui)
mkdir -p "${remote_work_dir}/logs" "${remote_work_dir}/config" "${remote_exp_dir}" 2>/dev/null || true

log_file="${remote_work_dir}/logs/start-slave-${tag}.log"

{
  echo "================================================================"
  log "Host           : $(hostname)"
  log "User           : ${remote_user}"
  log "Data           : $(date)"
  log "Args           : tag=${tag} master_ip=${master_ip} public_ip=${public_ip} private_ip=${private_ip} remote_exp_dir=${remote_exp_dir}"
  log "this_dir        = ${this_dir}"
  log "remote_work_dir = ${remote_work_dir}"
  log "remote_bin_dir  = ${remote_bin_dir}"
  log "remote_exp_dir  = ${remote_exp_dir}"
  log "remote_ready_file  = ${remote_ready_file}"
  log "remote_status_file = ${remote_status_file}"
  log "master_port     = ${master_port}"
  log "ssh_options     = ${ssh_options}"
  echo "================================================================"
} >> "${log_file}"

# Garantir PATH/GOPATH para os binários
export GOPATH="${remote_gopath}"
export PATH="${remote_bin_dir}:${PATH}"

# Diagnóstico rápido de binários
if [[ ! -x "${remote_bin_dir}/discoveryslave" ]]; then
  {
    log "ERRO: discoveryslave não é executável em ${remote_bin_dir}/discoveryslave"
    log "Listando ${remote_bin_dir}:"
    ls -la "${remote_bin_dir}" | head -n 120
  } >> "${log_file}"
  die "discoveryslave ausente/não executável"
fi

# ---------------------------------------------------------------------------
# 1) Inicia o discoveryslave localmente
# ---------------------------------------------------------------------------

{
  log "Iniciando discoveryslave..."
  log "Comando: discoveryslave -master ${master_ip}:${master_port} -public ${public_ip} -private ${private_ip}"
} >> "${log_file}"

cd "${remote_work_dir}" 2>/dev/null || true

/usr/bin/nohup "${remote_bin_dir}/discoveryslave" \
  -master "${master_ip}:${master_port}" \
  -public "${public_ip}" \
  -private "${private_ip}" \
  >> "${remote_work_dir}/logs/discoveryslave-${tag}.log" 2>&1 < /dev/null &

disc_pid=$!

{
  log "discoveryslave iniciado (pid=${disc_pid})."
  log "tail -n 50 discoveryslave log:"
  tail -n 50 "${remote_work_dir}/logs/discoveryslave-${tag}.log" 2>/dev/null || true
} >> "${log_file}"

# ---------------------------------------------------------------------------
# 2) Espera o master criar o arquivo READY
# ---------------------------------------------------------------------------

{
  log "Aguardando master READY file (${remote_ready_file})..."
} >> "${log_file}"

ready_timeout_sec="${SLAVE_READY_TIMEOUT_SEC:-120}"
start_ts="$(date +%s)"

while true; do
  now="$(date +%s)"
  if (( now - start_ts > ready_timeout_sec )); then
    {
      log "ERRO: timeout (${ready_timeout_sec}s) esperando READY no master."
      log "Debug: tentando ler status_file:"
    } >> "${log_file}"

    ssh ${ssh_options} -q -o "ConnectTimeout=10" "${remote_user}@${master_ip}" "cat '${remote_status_file}' 2>/dev/null || true" \
      >> "${log_file}" 2>&1 || true

    die "timeout esperando master READY"
  fi

  if ssh ${ssh_options} -q -o "ConnectTimeout=10" "${remote_user}@${master_ip}" "test -f '${remote_ready_file}'" </dev/null; then
    {
      log "Master READY detectado."
    } >> "${log_file}"
    break
  else
    {
      log "Master ainda não READY, tentando novamente em 5s..."
    } >> "${log_file}"
    sleep 5
  fi
done

# ---------------------------------------------------------------------------
# 3) Fim (o restante do controle é feito pelo master)
# ---------------------------------------------------------------------------

{
  log "Slave inicializado. Aguardando comandos do master..."
} >> "${log_file}"

