#!/usr/bin/env bash
set -euo pipefail

###############################################################################
# start-slave.sh
#
# Uso (chamado pelo start-remote-slaves.sh):
#   ./start-slave.sh <tag> <master_ip> <public_ip> <private_ip>
#
# - tag        : "peers", "1client", etc.
# - master_ip  : IP de controle do master (ex: 172.20.5.3)
# - public_ip  : IP público/controle deste slave
# - private_ip : IP de dados deste slave
###############################################################################

if [ "$#" -ne 4 ]; then
  echo "Uso: $0 <tag> <master_ip> <public_ip> <private_ip>" >&2
  exit 1
fi

tag="$1"
master_ip="$2"
public_ip="$3"
private_ip="$4"

# Diretório em que este script está
this_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Tenta localizar e carregar global-vars.sh
if [ -f "${this_dir}/scripts/global-vars.sh" ]; then
  # Caso remoto: this_dir = /users/$USER/iss
  . "${this_dir}/scripts/global-vars.sh"
elif [ -f "${this_dir}/global-vars.sh" ]; then
  # Caso local: this_dir = .../deployment/scripts
  . "${this_dir}/global-vars.sh"
fi

# Valores padrão genéricos (sem usuário hardcoded)
remote_home="${remote_home:-/users/${USER}}"
remote_gopath="${remote_gopath:-${remote_home}/go}"
remote_work_dir="${remote_work_dir:-${remote_home}/iss}"
remote_status_file="${remote_status_file:-${remote_work_dir}/status}"
master_port="${master_port:-9999}"

# Diretório de trabalho do experimento no slave
mkdir -p "${remote_work_dir}"
cd "${remote_work_dir}"

start_slave_log="${remote_work_dir}/start-slave-${tag}.log"

# Status inicial
echo "STARTING" > "${remote_status_file}"

{
  echo "================================================================"
  echo "[start-slave][$tag] Host : $(hostname)"
  echo "[start-slave][$tag] User : ${USER}"
  echo "[start-slave][$tag] Data : $(date)"
  echo "[start-slave][$tag] Args : tag=${tag} master_ip=${master_ip} public_ip=${public_ip} private_ip=${private_ip}"
  echo "[start-slave][$tag] remote_home        = ${remote_home}"
  echo "[start-slave][$tag] remote_work_dir    = ${remote_work_dir}"
  echo "[start-slave][$tag] remote_status_file = ${remote_status_file}"
  echo "[start-slave][$tag] remote_gopath      = ${remote_gopath}"
  echo "[start-slave][$tag] master_port        = ${master_port}"
  echo "================================================================"
} >> "${start_slave_log}" 2>&1

# Ajusta PATH para achar os binários (discoveryslave, orderingpeer, etc.)
export GOPATH="${remote_gopath}"
export PATH="${remote_gopath}/bin:${PATH}"

echo "[start-slave][$tag] Iniciando discoveryslave..." >> "${start_slave_log}"

# Lança discoveryslave em background e sai.
# Assinatura: discoveryslave <tag> <master_ip:port> <public_ip> <private_ip>
nohup discoveryslave "${tag}" "${master_ip}:${master_port}" "${public_ip}" "${private_ip}" \
  >> "${start_slave_log}" 2>&1 &

slave_pid=$!
echo "[start-slave][$tag] discoveryslave iniciado (pid=${slave_pid})." >> "${start_slave_log}"

# Marca status como RUNNING e termina (ssh volta imediatamente para o start-remote-slaves.sh)
echo "RUNNING" > "${remote_status_file}"

exit 0

