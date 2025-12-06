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

# Descobre diretório onde o script está rodando
# - no master, quando rodar manualmente, será algo tipo: .../mirbft/deployment/scripts
# - nos slaves (via SSH), será: /users/$USER/iss
this_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Tenta localizar e carregar o global-vars.sh
if [ -f "${this_dir}/scripts/global-vars.sh" ]; then
  # Caso remoto: this_dir = /users/$USER/iss
  # scripts/global-vars.sh -> /users/$USER/iss/scripts/global-vars.sh
  . "${this_dir}/scripts/global-vars.sh"
elif [ -f "${this_dir}/global-vars.sh" ]; then
  # Caso local: this_dir = .../deployment/scripts
  # global-vars.sh -> .../deployment/scripts/global-vars.sh
  . "${this_dir}/global-vars.sh"
else
  echo "[start-slave][$tag] ERRO: global-vars.sh não encontrado a partir de '${this_dir}'" >&2
  exit 1
fi

# Usa as variáveis definidas em global-vars.sh.
# No seu global-vars.sh elas já estão definidas assim (resumo):
#   remote_home="/users/Bruno"            # (você pode generalizar depois)
#   remote_work_dir="${remote_home}/iss"
#   remote_status_file="$remote_work_dir/status"
#   remote_ready_file="$remote_work_dir/master-ready"
#   master_port=9999
#   machine_status_poll_period=5
remote_home="${remote_home:-/users/${USER}}"
remote_work_dir="${remote_work_dir:-${remote_home}/iss}"
remote_status_file="${remote_status_file:-${remote_work_dir}/status}"
remote_ready_file="${remote_ready_file:-${remote_work_dir}/master-ready}"
master_port="${master_port:-9999}"
machine_status_poll_period="${machine_status_poll_period:-5}"

start_slave_log="${remote_work_dir}/start-slave-${tag}.log"

# Garante diretório de trabalho
mkdir -p "${remote_work_dir}"
cd "${remote_work_dir}"

# Atualiza status básico (arquivo 'status' remoto)
echo "STARTING" > "${remote_status_file}"

{
  echo "================================================================"
  echo "[start-slave][$tag] Host: $(hostname)"
  echo "[start-slave][$tag] User: ${USER}"
  echo "[start-slave][$tag] Data : $(date)"
  echo "[start-slave][$tag] Args : tag=${tag} master_ip=${master_ip} public_ip=${public_ip} private_ip=${private_ip}"
  echo "[start-slave][$tag] remote_work_dir    = ${remote_work_dir}"
  echo "[start-slave][$tag] remote_home        = ${remote_home}"
  echo "[start-slave][$tag] remote_status_file = ${remote_status_file}"
  echo "[start-slave][$tag] remote_ready_file  = ${remote_ready_file}"
  echo "[start-slave][$tag] master_port        = ${master_port}"
  echo "================================================================"
} >> "${start_slave_log}" 2>&1

# Espera o master escrever o arquivo READY (master-ready) no host do master.
# OBS: usamos $USER para SSH, não 'root'.
echo "[start-slave][$tag] Aguardando master READY em ${master_ip}:${remote_ready_file}" >> "${start_slave_log}"

while true; do
  if ssh -o BatchMode=yes -o ConnectTimeout=5 "${USER}@${master_ip}" "test -f '${remote_ready_file}'" \
       >> "${start_slave_log}" 2>&1; then
    echo "[start-slave][$tag] Master READY detectado." >> "${start_slave_log}"
    break
  fi

  echo "[start-slave][$tag] Master ainda não READY, tentando novamente em ${machine_status_poll_period}s..." \
       >> "${start_slave_log}"
  sleep "${machine_status_poll_period}"
done

# Marca status como RUNNING (já conectado ao master)
echo "RUNNING" > "${remote_status_file}"

# Ajusta PATH para achar os binários (discoveryslave, orderingpeer, etc.)
# Pelo start-remote-slaves.sh, os binários são copiados para:
#   remote_gopath/bin  (ex: /users/Bruno/go/bin)
remote_gopath="${remote_gopath:-${remote_home}/go}"
export GOPATH="${remote_gopath}"
export PATH="${remote_gopath}/bin:${PATH}"

echo "[start-slave][$tag] Iniciando discoveryslave..." >> "${start_slave_log}"

# Lança discoveryslave em background, conectado ao master.
# Assinatura (do seu código original): discoveryslave <tag> <master_ip:port> <public_ip> <private_ip>
nohup discoveryslave "${tag}" "${master_ip}:${master_port}" "${public_ip}" "${private_ip}" \
  >> "${start_slave_log}" 2>&1 &

slave_pid=$!
echo "[start-slave][$tag] discoveryslave iniciado (pid=${slave_pid})." >> "${start_slave_log}"

# Aqui o script termina, o discoveryslave fica rodando em background.
exit 0

