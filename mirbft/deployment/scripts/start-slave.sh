#!/usr/bin/env bash

#
# start-slave.sh
#
# Script executado DENTRO da máquina remota (slave) para:
#   - inicializar discoveryslave
#   - aguardar o master ficar READY
#   - (no modelo ISS) servir de endpoint para os comandos do master
#

set -euo pipefail

tag="$1"
master_ip="$2"
public_ip="$3"
private_ip="$4"

this_dir="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
remote_work_dir="${this_dir}"
remote_home="$(dirname "${this_dir}")"

# Usuário remoto usado nos SSH de volta para o master.
# Isso substitui o antigo 'root@...' e evita usuário fixo.
remote_user="${DEPL_REMOTE_USER:-$USER}"

source "${this_dir}/global-vars.sh"

#
# Logging básico do lado do slave
#
log_file="start-slave-${tag}.log"

echo "================================================================" >> "${log_file}"
echo "[start-slave][${tag}] Host : $(hostname)" >> "${log_file}"
echo "[start-slave][${tag}] User : ${remote_user}" >> "${log_file}"
echo "[start-slave][${tag}] Data : $(date)" >> "${log_file}"
echo "[start-slave][${tag}] Args : tag=${tag} master_ip=${master_ip} public_ip=${public_ip} private_ip=${private_ip}" >> "${log_file}"
echo "[start-slave][${tag}] remote_home        = ${remote_home}" >> "${log_file}"
echo "[start-slave][${tag}] remote_work_dir    = ${remote_work_dir}" >> "${log_file}"
echo "[start-slave][${tag}] remote_status_file = ${remote_status_file}" >> "${log_file}"
echo "[start-slave][${tag}] remote_gopath      = ${remote_gopath}" >> "${log_file}"
echo "[start-slave][${tag}] master_port        = ${master_port}" >> "${log_file}"
echo "================================================================" >> "${log_file}"

# ---------------------------------------------------------------------------
# 1) Inicia o discoveryslave localmente
# ---------------------------------------------------------------------------

echo "[start-slave][${tag}] Iniciando discoveryslave..." >> "${log_file}"

export GOPATH="${remote_gopath}"
export PATH="${remote_bin_dir}:${PATH}"

discoveryslave \
  -master "${master_ip}:${master_port}" \
  -public "${public_ip}" \
  -private "${private_ip}" \
  >> "${remote_work_dir}/discoveryslave-${tag}.log" 2>&1 &

disc_pid=$!

echo "[start-slave][${tag}] discoveryslave iniciado (pid=${disc_pid})." >> "${log_file}"

# ---------------------------------------------------------------------------
# 2) Espera o master criar o arquivo READY
# ---------------------------------------------------------------------------

echo "[start-slave][${tag}] Aguardando master criar READY file (${remote_ready_file})..." >> "${log_file}"

while true; do
  if ssh ${ssh_options} -q -o "ConnectTimeout=10" "${remote_user}@${master_ip}" "test -f '${remote_ready_file}'"; then
    echo "[start-slave][${tag}] Master READY detectado." >> "${log_file}"
    break
  else
    echo "Host key verification failed." >> "${log_file}" 2>/dev/null || true
    echo "[start-slave][${tag}] Master ainda não READY, tentando novamente em 5s..." >> "${log_file}"
    sleep 5
  fi
done

# ---------------------------------------------------------------------------
# 3) Fim
#    (O restante do controle é feito pelo master via discovery/exec-start)
# ---------------------------------------------------------------------------

echo "[start-slave][${tag}] Slave inicializado. Aguardando comandos do master..." >> "${log_file}"

