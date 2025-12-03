#!/usr/bin/env bash
#
# start-remote-slaves.sh
#
# Script para iniciar slaves em instâncias remotas usando o discovery master.
#
# Uso:
#   ./scripts/start-remote-slaves.sh <exp_data_dir> <tag> <n> <master_ip> <args...>
#
# Onde:
#   <exp_data_dir>  diretório base dos dados do experimento (ex.: deployment-data/remote-0000)
#   <tag>           tag dos slaves a serem iniciados (ex.: peers, 1client, etc.)
#   <n>             número de slaves a iniciar
#   <master_ip>     IP do master (onde está discoverymaster)
#   <args...>       pares (instance_id ctrl_ip role tag) lidos de scripts/instance-info
#
set -euo pipefail

# ----------------------------------------------------------------------
# Parâmetros
# ----------------------------------------------------------------------
exp_data_dir="$1"
tag="$2"
n="$3"
master_ip="$4"
shift 4

# Diretório local de configuração gerado pelo generate-config.sh
# (deployment-data/remote-0000/experiment-config)
local_config_dir="${exp_data_dir}/experiment-config"

# ----------------------------------------------------------------------
# Importa variáveis globais
# ----------------------------------------------------------------------
# (remote_work_dir, remote_bin_dir, remote_status_file, etc.)
source "$(dirname "$0")/global-vars.sh"

# ----------------------------------------------------------------------
# Funções auxiliares
# ----------------------------------------------------------------------

usage() {
  echo "Uso: $0 <exp_data_dir> <tag> <n> <master_ip> <instance_id ctrl_ip role tag>..."
  exit 1
}

if [[ -z "${exp_data_dir:-}" || -z "${tag:-}" || -z "${n:-}" || -z "${master_ip:-}" ]]; then
  usage
fi

# Espera por argumentos com múltiplos de 4: instance_id, ctrl_ip, role, tag
if (( "$#" % 4 != 0 )); then
  echo "ERRO: número de argumentos inválido; esperado múltiplos de 4 (instance_id, ctrl_ip, role, tag)." >&2
  exit 1
fi

# Opções de SSH/SCP
ssh_options="-o BatchMode=yes -o StrictHostKeyChecking=no"
scp_options="${ssh_options}"

# Diretórios locais (no node-0)
local_bin_dir="${GOPATH}/bin"
local_scripts_dir="$(cd "$(dirname "$0")/.." && pwd)/scripts"

# ----------------------------------------------------------------------
# Garante ambiente em um slave remoto
#   Parâmetros:
#     $1 = instance_id
#     $2 = ctrl_ip
#     $3 = role      (master | slave)
#     $4 = tag       (peers, 1client, etc.)
# ----------------------------------------------------------------------
ensure_remote_slave() {
  local instance_id="$1"
  local ctrl_ip="$2"
  local role="$3"
  local node_tag="$4"

  # Só nos interessam os slaves com a tag desejada
  if [[ "${role}" != "slave" || "${node_tag}" != "${tag}" ]]; then
    return 0
  fi

  local public_ip="${ctrl_ip}"

  echo "---------------------------------------------------------------------"
  echo "  [REMOTO] Garantindo ambiente em ${public_ip}"
  echo "           instance_id = ${instance_id}"
  echo "           tag         = ${node_tag}"
  echo "---------------------------------------------------------------------"

  # 1) Garante diretórios base no remoto
  ssh ${ssh_options} "Bruno@${public_ip}" "
    mkdir -p '${remote_work_dir}' \
             '${remote_bin_dir}' \
             '${remote_exp_dir}'
    mkdir -p \"\$(dirname '${remote_status_file}')\"
    echo RUNNING > '${remote_status_file}'
  " >/dev/null 2>&1

  # 2) Copiar start-slave.sh
  local start_slave_script="${local_scripts_dir}/start-slave.sh"
  scp ${scp_options} \
    "${start_slave_script}" \
    "Bruno@${public_ip}:${remote_work_dir}/start-slave.sh" >/dev/null 2>&1

  # 3) Copiar diretório scripts/
  scp -r ${scp_options} \
    "${local_scripts_dir}" \
    "Bruno@${public_ip}:${remote_work_dir}/" >/dev/null 2>&1

  # 4) Copiar binários (discoverymaster, discoveryslave, orderingpeer, orderingclient)
  #    (assume que go install ./cmd/... já foi executado e local_bin_dir está atualizado)
  scp ${scp_options} \
    "${local_bin_dir}/discoverymaster" \
    "${local_bin_dir}/discoveryslave" \
    "${local_bin_dir}/orderingpeer" \
    "${local_bin_dir}/orderingclient" \
    "Bruno@${public_ip}:${remote_bin_dir}/" >/dev/null 2>&1

  # 5) Copiar configuração do experimento para o diretório 'config/' no slave
  #    Isso é essencial para que o comando:
  #      orderingpeer config/config.yml ...
  #    (no master-commands.cmd) funcione em TODOS os slaves.
  if [[ -d "${local_config_dir}" ]]; then
    echo "    [REMOTO] Copiando configs (experiment-config -> config) para ${public_ip}..."
    ssh ${ssh_options} "Bruno@${public_ip}" "
      mkdir -p '${remote_work_dir}/config'
    " >/dev/null 2>&1

    # Copia todos os arquivos de configuração (incluindo config.yml) para /users/Bruno/iss/config/
    scp ${scp_options} \
      "${local_config_dir}/"* \
      "Bruno@${public_ip}:${remote_work_dir}/config/" >/dev/null 2>&1
  else
    echo "    [REMOTO] AVISO: diretório de configs '${local_config_dir}' não encontrado; 'config/config.yml' não será criado no slave."
  fi

  # 6) Ajusta permissões
  ssh ${ssh_options} "Bruno@${public_ip}" "
    chmod +x '${remote_bin_dir}/discoverymaster' \
             '${remote_bin_dir}/discoveryslave' \
             '${remote_bin_dir}/orderingpeer' \
             '${remote_bin_dir}/orderingclient' \
             '${remote_work_dir}/start-slave.sh' \
             '${remote_work_dir}/scripts/'*.sh 2>/dev/null || true
  " >/dev/null 2>&1

  echo "    [REMOTO] OK: ambiente garantido em ${public_ip}."
}

# ----------------------------------------------------------------------
# Iniciar slaves da tag desejada
# ----------------------------------------------------------------------
start_slaves() {
  echo "==== [start-remote-slaves] INICIANDO SLAVES (tag='${tag}', n=${n}) ===="

  # Garante ambiente em todos os slaves primeiro
  echo "==== [start-remote-slaves] (REMOTO) Garantindo binários e scripts ===="
  local total_args="$#"
  local i=1

  while (( i <= total_args )); do
    local instance_id="${!i}"; ((i++))
    local ctrl_ip="${!i}";      ((i++))
    local role="${!i}";         ((i++))
    local node_tag="${!i}";     ((i++))

    ensure_remote_slave "${instance_id}" "${ctrl_ip}" "${role}" "${node_tag}"
  done

  echo "==== [start-remote-slaves] Distribuição concluída. ===="

  # Agora de fato dispara os slaves da tag desejada
  echo
  echo "==== [start-remote-slaves] Iniciando slaves da TAG '${tag}' (n = ${n}) ===="

  i=1
  local started=0

  while (( i <= total_args )); do
    local instance_id="${!i}"; ((i++))
    local ctrl_ip="${!i}";      ((i++))
    local role="${!i}";         ((i++))
    local node_tag="${!i}";     ((i++))

    if [[ "${role}" == "slave" && "${node_tag}" == "${tag}" ]]; then
      local public_ip="${ctrl_ip}"
      echo "  [DEPLOY] Iniciando slave em ${public_ip}"
      ssh ${ssh_options} "Bruno@${public_ip}" "
        cd '${remote_work_dir}'
        nohup ./start-slave.sh '${master_ip}' '${exp_data_dir}' '${tag}' '${instance_id}' \
          > '${remote_work_dir}/start-slave-${instance_id}.log' 2>&1 &
      " >/dev/null 2>&1
      ((started++))
      if (( started == n )); then
        break
      fi
    fi
  done

  echo
  echo "==== [start-remote-slaves] Todos os slaves disparados. ===="
  echo "==== [start-remote-slaves] FIM ==========================================="
}

# ----------------------------------------------------------------------
# MAIN
# ----------------------------------------------------------------------
echo "====================================================================="
echo "=== [start-remote-slaves] INÍCIO ===================================="
echo "  exp_data_dir = ${exp_data_dir}"
echo "  tag          = ${tag}"
echo "  n            = ${n}"
echo "  master_ip    = ${master_ip}"
echo "  args rest    = $*"
echo "====================================================================="
echo

start_slaves "$@"

