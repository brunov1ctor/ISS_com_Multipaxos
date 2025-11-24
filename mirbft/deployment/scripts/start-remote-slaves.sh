#!/usr/bin/env bash
set -euo pipefail

###############################################################################
# start-remote-slaves.sh
#
# Uso:
#   scripts/start-remote-slaves.sh EXP_DATA_DIR TAG N MASTER_IP
#
# Este script (modo REMOTE):
#   - Lê scripts/instance-info (fonte única!)
#   - Para cada linha "instance_id public_ip private_ip role tag":
#       * Se role == slave, copia:
#           - /users/$USER/go/bin/{discoverymaster,discoveryslave,orderingpeer,orderingclient}
#           - /users/$USER/iss/start-slave.sh
#         para o nó remoto.
#   - Depois, inicia até N nós cujo tag == TAG chamando remotamente:
#       /users/$USER/iss/start-slave.sh TAG MASTER_IP PUBLIC_IP PRIVATE_IP
###############################################################################

if [ "$#" -lt 4 ]; then
  echo "Uso: $0 EXP_DATA_DIR TAG N MASTER_IP" >&2
  exit 1
fi

exp_data_dir="$1"
tag="$2"
n="$3"
master_ip="$4"

echo "====================================================================="
echo "=== [start-remote-slaves] INÍCIO ===================================="
echo "  exp_data_dir = $exp_data_dir"
echo "  tag          = $tag"
echo "  n            = $n"
echo "  master_ip    = $master_ip"
echo "====================================================================="
echo

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root_dir="$(cd "$script_dir/.." && pwd)"

instance_info_file="$root_dir/scripts/instance-info"
if [ ! -f "$instance_info_file" ]; then
  echo "ERRO: scripts/instance-info não encontrado em $instance_info_file" >&2
  exit 1
fi

echo "Usando scripts/instance-info: $instance_info_file"
echo

# GOPATH/GOBIN remoto (e local)
user_name="${USER:-Bruno}"
remote_gopath="/users/${user_name}/go"
remote_bin_dir="${remote_gopath}/bin"
local_bin_dir="${remote_bin_dir}"

remote_work_dir_default="/users/${user_name}/iss"
remote_work_dir="${remote_work_dir:-$remote_work_dir_default}"

: "${ssh_options:=}"

echo "==== [start-remote-slaves] (LOCAL) Verificando binários em ${local_bin_dir} ===="
echo "  remote_gopath = ${remote_gopath}"
echo "  local_bin_dir = ${local_bin_dir}"

for bin in discoverymaster discoveryslave orderingpeer orderingclient; do
  if [ ! -x "${local_bin_dir}/${bin}" ]; then
    echo "  [LOCAL] ERRO  : ${local_bin_dir}/${bin} não encontrado ou não é executável." >&2
    exit 1
  fi
  echo "  [LOCAL] OK     : ${local_bin_dir}/${bin}"
done
echo "==== [start-remote-slaves] Binários locais OK. ===="
echo

ssh_call() {
  local host="$1"
  shift
  if [ -n "$ssh_options" ]; then
    eval ssh $ssh_options "\"${host}\"" "\"$@\""
  else
    ssh "${host}" "$@"
  fi
}

scp_call() {
  local src="$1"
  local dst="$2"
  if [ -n "$ssh_options" ]; then
    eval scp $ssh_options "\"${src}\"" "\"${dst}\""
  else
    scp "${src}" "${dst}"
  fi
}

###############################################################################
# 1) Garante binários + start-slave.sh em TODOS OS SLAVES (scripts/instance-info)
###############################################################################
echo "==== [start-remote-slaves] (REMOTO) Garantindo binários e script em todos os slaves (scripts/instance-info) ===="

local_start_slave="${script_dir}/start-slave.sh"
if [ ! -f "$local_start_slave" ]; then
  echo "ERRO: ${local_start_slave} não existe no master." >&2
  exit 1
fi

while read -r instance_id public_ip private_ip role node_tag; do
  # Ignora comentários e linhas vazias
  if [ -z "${instance_id:-}" ] || [[ "$instance_id" =~ ^# ]]; then
    continue
  fi

  echo "---------------------------------------------------------------------"
  echo "  [REMOTO] Nó ${instance_id} (${public_ip} / ${private_ip})"
  echo "           role=${role} tag=${node_tag}"

  if [ "$role" != "slave" ]; then
    echo "    [REMOTO] role='${role}' (não é slave); ignorando."
    continue
  fi

  echo "---------------------------------------------------------------------"
  echo "  [REMOTO] Garantindo binários e start-slave.sh em ${public_ip}"
  echo "           remote_gopath   = ${remote_gopath}"
  echo "           remote_bin_dir  = ${remote_bin_dir}"
  echo "           remote_work_dir = ${remote_work_dir}"
  echo "---------------------------------------------------------------------"

  ssh_call "$public_ip" "mkdir -p \"${remote_bin_dir}\" \"${remote_work_dir}\""

  for bin in discoverymaster discoveryslave orderingpeer orderingclient; do
    echo "    [REMOTO-binary] Copiando '${bin}' para ${public_ip}:${remote_bin_dir}/..."
    scp_call "${local_bin_dir}/${bin}" "${public_ip}:${remote_bin_dir}/${bin}"
  done

  echo "    [REMOTO-script] Copiando start-slave.sh para ${public_ip}:${remote_work_dir}/start-slave.sh..."
  scp_call "${local_start_slave}" "${public_ip}:${remote_work_dir}/start-slave.sh"

  ssh_call "$public_ip" "chmod +x \"${remote_work_dir}/start-slave.sh\""

  echo "    [REMOTO] OK: binários e start-slave.sh garantidos em ${public_ip}."
done < "$instance_info_file"

echo "==== [start-remote-slaves] Distribuição/garantia remota concluída. ===="
echo

###############################################################################
# 2) Inicia até N slaves cujo tag == TAG
###############################################################################
echo "==== [start-remote-slaves] Iniciando loop pelos nós (n = ${n}, tag='${tag}') ===="

started=0

while read -r instance_id public_ip private_ip role node_tag; do
  if [ -z "${instance_id:-}" ] || [[ "$instance_id" =~ ^# ]]; then
    continue
  fi

  echo "---------------------------------------------------------------------"
  echo "  [LOOP] instance_id=${instance_id}"
  echo "         public_slave_ip=${public_ip}"
  echo "         private_slave_ip=${private_ip}"
  echo "         slave_role=${role}"
  echo "         slave_tag=${node_tag}"
  echo "         tag alvo=${tag}, n restante=$((n - started))"

  if [ "$started" -ge "$n" ]; then
    echo "  [LOOP] Já atingimos n=${n} nós iniciados para tag='${tag}'; ignorando nós extras."
    continue
  fi

  if [ "$role" != "slave" ] || [ "$node_tag" != "$tag" ]; then
    echo "  [LOOP] role/tag não batem (role='${role}', tag='${node_tag}'); ignorando esse nó."
    continue
  fi

  log_file="${exp_data_dir}/ssh-${tag}-${public_ip}.log"
  echo "  [DEPLOY] Vai iniciar slave em ${public_ip} (${instance_id}), tag=${tag}"
  echo "  [DEPLOY] Log remoto será gravado em: ${log_file}"

  (
    echo "[LOCAL] SSH -> ${public_ip} chamando ${remote_work_dir}/start-slave.sh ${tag} ${master_ip} ${public_ip} ${private_ip}"
    ssh_call "$public_ip" "bash -lc '\"${remote_work_dir}/start-slave.sh\" \"${tag}\" \"${master_ip}\" \"${public_ip}\" \"${private_ip}\"'"
  ) >"${log_file}" 2>&1 &

  started=$((started + 1))
  echo "  [DEPLOY] n restante agora = $((n - started))"
  echo "  [DEPLOY] SSH disparado para ${public_ip} em background."
  echo "  [DEPLOY] Aguardando pequena pausa para não sobrecarregar o SSH."
  sleep 1
done < "$instance_info_file"

echo
echo "==== [start-remote-slaves] Todos os SSHs disparados. Chamando 'wait' para aguardar término dos comandos locais. ===="
wait || true
echo "==== [start-remote-slaves] FIM ==========================================="
echo "====================================================================="

