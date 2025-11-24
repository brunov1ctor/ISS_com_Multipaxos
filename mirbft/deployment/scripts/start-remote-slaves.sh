#!/usr/bin/env bash

# Não usa -e pra não abortar o script inteiro se um ssh/scp falhar em um nó
set -uo pipefail

exp_data_dir="$1"
tag="$2"
n="$3"
master_ip="$4"

echo "====================================================================="
echo "=== [start-remote-slaves] INÍCIO ===================================="
echo "  exp_data_dir = ${exp_data_dir}"
echo "  tag          = ${tag}"
echo "  n            = ${n}"
echo "  master_ip    = ${master_ip}"
echo "====================================================================="
echo

# ----------------------------------------------------------------------
# Caminhos locais e remotos
# ----------------------------------------------------------------------
remote_gopath="/users/Bruno/go"
remote_bin_dir="${remote_gopath}/bin"
remote_work_dir="/users/Bruno/iss"
remote_logs_dir="/users/Bruno/iss-logs"

local_bin_dir="/users/Bruno/go/bin"
local_discoverymaster="${local_bin_dir}/discoverymaster"
local_discoveryslave="${local_bin_dir}/discoveryslave"
local_orderingpeer="${local_bin_dir}/orderingpeer"
local_orderingclient="${local_bin_dir}/orderingclient"

# Script start-slave que será copiado para os slaves
local_start_slave_script="scripts/start-slave.sh"

# Arquivo de descrição das instâncias (modo remote)
instance_info_file="scripts/instance-info"

if [[ ! -f "${instance_info_file}" ]]; then
  echo "ERRO: arquivo ${instance_info_file} não encontrado!"
  exit 1
fi

echo "Usando ${instance_info_file}: $(readlink -f "${instance_info_file}")"
echo

# ----------------------------------------------------------------------
# Verificação dos binários locais
# ----------------------------------------------------------------------
echo "==== [start-remote-slaves] (LOCAL) Verificando binários em ${local_bin_dir} ===="
echo "  remote_gopath = ${remote_gopath}"
echo "  local_bin_dir = ${local_bin_dir}"

for bin in "${local_discoverymaster}" "${local_discoveryslave}" "${local_orderingpeer}" "${local_orderingclient}"; do
  if [[ ! -x "${bin}" ]]; then
    echo "  [LOCAL] ERRO: binário não encontrado ou não executável: ${bin}"
    exit 1
  else
    echo "  [LOCAL] OK     : ${bin}"
  fi
done

if [[ ! -f "${local_start_slave_script}" ]]; then
  echo "  [LOCAL] ERRO: script ${local_start_slave_script} não encontrado!"
  exit 1
fi

echo "==== [start-remote-slaves] Binários locais OK. ===="
echo

# ----------------------------------------------------------------------
# Função para garantir binários e start-slave.sh em um slave
# ----------------------------------------------------------------------
ensure_remote_slave() {
  local instance_id="$1"
  local public_ip="$2"
  local private_ip="$3"
  local role="$4"
  local slave_tag="$5"

  # Só trata nodes com role=slave
  if [[ "${role}" != "slave" ]]; then
    echo "    [REMOTO] role='${role}' (não é slave); ignorando."
    return
  fi

  echo "---------------------------------------------------------------------"
  echo "  [REMOTO] Garantindo binários e start-slave.sh em ${public_ip}"
  echo "           remote_gopath   = ${remote_gopath}"
  echo "           remote_bin_dir  = ${remote_bin_dir}"
  echo "           remote_work_dir = ${remote_work_dir}"
  echo "           remote_logs_dir = ${remote_logs_dir}"
  echo "---------------------------------------------------------------------"

  # Cria diretórios remotos
  ssh -o StrictHostKeyChecking=accept-new "Bruno@${public_ip}" "
    mkdir -p '${remote_bin_dir}' '${remote_work_dir}' '${remote_logs_dir}'
  " >/dev/null 2>&1

  if [[ $? -ne 0 ]]; then
    echo "    [REMOTO-ERRO] Falha ao criar diretórios em ${public_ip} (ssh). Continuando para próximo nó."
    return
  fi

  # Copia binários
  scp -o StrictHostKeyChecking=accept-new \
    "${local_discoverymaster}" \
    "${local_discoveryslave}" \
    "${local_orderingpeer}" \
    "${local_orderingclient}" \
    "Bruno@${public_ip}:${remote_bin_dir}/" >/dev/null 2>&1

  if [[ $? -ne 0 ]]; then
    echo "    [REMOTO-ERRO] Falha ao copiar binários para ${public_ip}. Continuando para próximo nó."
    return
  fi

  # Copia start-slave.sh
  scp -o StrictHostKeyChecking=accept-new \
    "${local_start_slave_script}" \
    "Bruno@${public_ip}:${remote_work_dir}/start-slave.sh" >/dev/null 2>&1

  if [[ $? -ne 0 ]]; then
    echo "    [REMOTO-ERRO] Falha ao copiar start-slave.sh para ${public_ip}. Continuando para próximo nó."
    return
  fi

  # Garante permissão de execução
  ssh -o StrictHostKeyChecking=accept-new "Bruno@${public_ip}" "
    chmod +x '${remote_bin_dir}/discoverymaster' \
             '${remote_bin_dir}/discoveryslave' \
             '${remote_bin_dir}/orderingpeer' \
             '${remote_bin_dir}/orderingclient' \
             '${remote_work_dir}/start-slave.sh'
  " >/dev/null 2>&1

  if [[ $? -ne 0 ]]; then
    echo "    [REMOTO-ERRO] Falha ao ajustar permissões em ${public_ip}. Continuando para próximo nó."
    return
  fi

  echo "    [REMOTO] OK: binários e start-slave.sh garantidos em ${public_ip}."
}

# ----------------------------------------------------------------------
# Garante binários + start-slave.sh em TODOS os slaves do instance-info
# ----------------------------------------------------------------------
echo "==== [start-remote-slaves] (REMOTO) Garantindo binários e script em todos os slaves (${instance_info_file}) ===="

while read -r instance_id public_ip private_ip role slave_tag; do
  # ignora linha vazia ou comentário
  [[ -z "${instance_id}" ]] && continue
  [[ "${instance_id}" =~ ^# ]] && continue

  echo "---------------------------------------------------------------------"
  echo "  [REMOTO] Nó ${instance_id} (${public_ip} / ${private_ip})"
  echo "           role=${role} tag=${slave_tag}"
  echo "---------------------------------------------------------------------"

  ensure_remote_slave "${instance_id}" "${public_ip}" "${private_ip}" "${role}" "${slave_tag}"
done < "${instance_info_file}"

echo "==== [start-remote-slaves] Distribuição/garantia remota concluída. ===="
echo

# ----------------------------------------------------------------------
# Agora de fato inicia os slaves com a TAG solicitada (peers, 1client, etc.)
# ----------------------------------------------------------------------
echo "==== [start-remote-slaves] Iniciando loop pelos nós (n = ${n}, tag='${tag}') ===="

started=0

while read -r instance_id public_ip private_ip role slave_tag; do
  # ignora linha vazia ou comentário
  [[ -z "${instance_id}" ]] && continue
  [[ "${instance_id}" =~ ^# ]] && continue

  echo "---------------------------------------------------------------------"
  echo "  [LOOP] instance_id=${instance_id}"
  echo "         public_slave_ip=${public_ip}"
  echo "         private_slave_ip=${private_ip}"
  echo "         slave_role=${role}"
  echo "         slave_tag=${slave_tag}"
  echo "         tag alvo=${tag}, n restante=${n}"
  echo "---------------------------------------------------------------------"

  # Só iniciamos se for slave e a tag do nó bater com a tag alvo
  if [[ "${role}" != "slave" || "${slave_tag}" != "${tag}" ]]; then
    echo "  [LOOP] role/tag não batem (role='${role}', tag='${slave_tag}'); ignorando esse nó."
    continue
  fi

  if (( started >= n )); then
    echo "  [LOOP] Já atingimos n=${n} nós iniciados para tag='${tag}'; ignorando nós extras."
    continue
  fi

  log_file="${exp_data_dir}/ssh-${tag}-${public_ip}.log"
  echo "  [DEPLOY] Vai iniciar slave em ${public_ip} (${instance_id}), tag=${tag}"
  echo "  [DEPLOY] Log remoto será gravado em: ${log_file}"

  # Dispara o start-slave.sh no host remoto em background
  (
    ssh -o StrictHostKeyChecking=accept-new "Bruno@${public_ip}" "
      cd '${remote_work_dir}' || exit 1
      nohup ./start-slave.sh '${tag}' '${master_ip}' '${public_ip}' '${private_ip}' \
        > '${remote_logs_dir}/start-slave-${tag}.log' 2>&1 &
    "
  ) > "${log_file}" 2>&1 &

  started=$((started + 1))
  echo "  [DEPLOY] n restante agora = $((n - started))"
  echo "  [DEPLOY] SSH disparado para ${public_ip} em background."
  echo "  [DEPLOY] Aguardando pequena pausa para não sobrecarregar o SSH."
  sleep 0.2

done < "${instance_info_file}"

echo
echo "==== [start-remote-slaves] Todos os SSHs disparados. Chamando 'wait' para aguardar término dos comandos locais. ===="
wait || true
echo "==== [start-remote-slaves] FIM ==========================================="
echo "====================================================================="

