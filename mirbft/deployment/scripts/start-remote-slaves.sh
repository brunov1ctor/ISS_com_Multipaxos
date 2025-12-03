#!/usr/bin/env bash

set -euo pipefail

# --------------------------------------------------------------------
# start-remote-slaves.sh
#
# Inicia remotamente os slaves de uma certa TAG, em um experimento
# identificado por exp_data_dir.
#
# Parâmetros:
#   $1 = exp_data_dir (ex.: deployment-data/remote-0000)
#   $2 = tag (ex.: peers, 1client, etc.)
#   $3 = n (número de slaves com essa TAG a iniciar)
#   $4 = master_ip
#   $5.. = lista de nós, em grupos de 5:
#           instance_id ctrl_ip private_ip role tag
#
# Exemplo de lista de nós:
#   node-0 172.20.4.4 10.10.1.1 master master \
#   node-1 172.20.4.5 10.10.1.2 slave  peers  \
#   node-2 172.20.4.6 10.10.1.3 slave  peers  \
#   ...
#
# IMPORTANTE:
#   - O script compila os binários localmente (go install ./cmd/...)
#   - Copia os binários e scripts necessários para todos os slaves
#   - Depois dispara apenas os slaves da TAG pedida (por ex. "peers")
# --------------------------------------------------------------------

if [[ $# -lt 4 ]]; then
  echo "USO: $0 <exp_data_dir> <tag> <n> <master_ip> [instance_id ctrl_ip private_ip role tag]..." >&2
  exit 1
fi

exp_data_dir="$1"
tag="$2"
n="$3"
master_ip="$4"
shift 4

rest=( "$@" )

echo "====================================================================="
echo "=== [start-remote-slaves] INÍCIO ===================================="
echo "  exp_data_dir = ${exp_data_dir}"
echo "  tag          = ${tag}"
echo "  n            = ${n}"
echo "  master_ip    = ${master_ip}"
echo "  args rest    = ${rest[*]}"
echo "====================================================================="
echo

# --------------------------------------------------------------------
# Determina diretórios
# --------------------------------------------------------------------
# Este script fica em mirbft/deployment/scripts,
# então o repositório raiz é dois níveis acima.
this_dir="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
deployment_dir="$( cd "${this_dir}/.." && pwd )"
repo_dir="$( cd "${deployment_dir}/.." && pwd )"

echo "==== [start-remote-slaves] Diretórios detectados ====="
echo "  this_dir       = ${this_dir}"
echo "  deployment_dir = ${deployment_dir}"
echo "  repo_dir       = ${repo_dir}"
echo

# --------------------------------------------------------------------
# Compila binários localmente
# --------------------------------------------------------------------
echo "==== [start-remote-slaves] (LOCAL) Compilando binários ====="
echo "  Repositório: ${repo_dir}"
(
  cd "${repo_dir}"
  echo "  Executando: go install ./cmd/..."
  if ! go install ./cmd/...; then
    echo "  [LOCAL] ERRO: falha ao compilar binários!"
    exit 1
  fi
)
echo "  [LOCAL] Compilação concluída."
echo

# --------------------------------------------------------------------
# Verifica binários localmente (no $GOPATH/bin)
# --------------------------------------------------------------------
echo "==== [start-remote-slaves] (LOCAL) Verificando binários em \$GOPATH/bin ===="

# Detecta GOPATH de forma robusta
GOPATH_LOCAL="${GOPATH:-$(go env GOPATH)}"
remote_gopath="${GOPATH_LOCAL}"

echo "  remote_gopath = ${remote_gopath}"

local_bin_dir="${remote_gopath}/bin"
echo "  local_bin_dir = ${local_bin_dir}"

check_bin() {
  local name="$1"
  if [[ -x "${local_bin_dir}/${name}" ]]; then
    echo "  [LOCAL] OK: ${local_bin_dir}/${name}"
  else
    echo "  [LOCAL] ERRO: binário não encontrado ou não executável: ${local_bin_dir}/${name}"
    exit 1
  fi
}

check_bin discoverymaster
check_bin discoveryslave
check_bin orderingpeer
check_bin orderingclient

echo "==== [start-remote-slaves] Binários + scripts verificados. ===="
echo

# --------------------------------------------------------------------
# Função para garantir ambiente remoto
# --------------------------------------------------------------------
ensure_remote_slave_env() {
  local instance_id="$1"
  local ctrl_ip="$2"
  local private_ip="$3"
  local role="$4"
  local slave_tag="$5"

  local ssh_options="-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null"

  # Diretórios remotos fixos (ajuste se necessário)
  local remote_base_dir="/users/Bruno/iss"
  local remote_work_dir="${remote_base_dir}"
  local remote_exp_dir="${remote_base_dir}/current-deployment-data"

  local remote_bin_dir="${remote_base_dir}/bin"
  local remote_scripts_dir="${remote_base_dir}/scripts"
  local remote_status_file="${remote_exp_dir}/status/${instance_id}.status"
  local remote_ready_file="${remote_exp_dir}/status/master.ready"

  echo "---------------------------------------------------------------------"
  echo "  [REMOTO] Garantindo ambiente em ${ctrl_ip}"
  echo "           instance_id = ${instance_id}"
  echo "           tag         = ${slave_tag}"
  echo "---------------------------------------------------------------------"

  # Kill e limpeza prévia (rodado antes no deploy-remote.sh, mas não faz mal)
  ssh ${ssh_options} "Bruno@${ctrl_ip}" "
    # Mata binários antigos e limpa dados do experimento anterior
    killall -9 discoverymaster discoveryslave orderingpeer orderingclient scp rsync 2>/dev/null || true
    rm -rf ${remote_exp_dir}

    # Garante diretórios de trabalho
    mkdir -p '${remote_work_dir}' '${remote_exp_dir}'
    mkdir -p \"\$(dirname '${remote_status_file}')\"

    echo RUNNING > '${remote_status_file}'

    # Remove marca antiga de master pronto, se existir
    rm -f '${remote_ready_file}'

    # Mata sessões sshd antigas (notty), se existirem
    kill -9 \$(ps -ef | grep 'sshd: notty' | awk '{print \$2}') 2>/dev/null || true
  " || {
    echo "  [REMOTO] ERRO ao preparar ambiente em ${ctrl_ip}."
    return 1
  }

  # Copia binários e scripts necessários
  # Binários
  rsync -avz -e "ssh ${ssh_options}" \
    "${local_bin_dir}/discoverymaster" \
    "${local_bin_dir}/discoveryslave" \
    "${local_bin_dir}/orderingpeer" \
    "${local_bin_dir}/orderingclient" \
    "Bruno@${ctrl_ip}:${remote_bin_dir}/" || {
      echo "  [REMOTO] ERRO ao copiar binários para ${ctrl_ip}."
      return 1
    }

  # Scripts de deployment
  rsync -avz -e "ssh ${ssh_options}" \
    "${deployment_dir}/scripts/" \
    "Bruno@${ctrl_ip}:${remote_scripts_dir}/" || {
      echo "  [REMOTO] ERRO ao copiar scripts para ${ctrl_ip}."
      return 1
    }

  echo "    [REMOTO] OK: ambiente garantido em ${ctrl_ip}."
}

# --------------------------------------------------------------------
# Passo 1: garantir ambiente em todos os slaves
# --------------------------------------------------------------------
echo "==== [start-remote-slaves] (REMOTO) Garantindo binários e scripts ===="

# Percorre a lista rest[] de 5 em 5:
#   instance_id ctrl_ip private_ip role tag
idx=0
total="${#rest[@]}"

while (( idx + 4 < total )); do
  instance_id="${rest[$idx]}"
  ctrl_ip="${rest[$((idx+1))]}"
  private_ip="${rest[$((idx+2))]}"
  role="${rest[$((idx+3))]}"
  slave_tag="${rest[$((idx+4))]}"

  # Só mexe em quem é slave (role="slave")
  if [[ "${role}" != "slave" ]]; then
    ((idx+=5))
    continue
  fi

  ensure_remote_slave_env "${instance_id}" "${ctrl_ip}" "${private_ip}" "${role}" "${slave_tag}"

  ((idx+=5))
done

echo "==== [start-remote-slaves] Distribuição concluída. ===="
echo

# --------------------------------------------------------------------
# Passo 2: iniciar apenas os slaves da TAG pedida
# --------------------------------------------------------------------
echo "==== [start-remote-slaves] Iniciando slaves da TAG '${tag}' (n = ${n}) ===="

idx=0
started=0

while (( idx < total )); do
  key="${rest[$idx]}"

  # Suporta marca "skip <instance_id> <ctrl_ip>" (se for usada no futuro)
  if [[ "${key}" == "skip" ]]; then
    ((idx+=3))
    continue
  fi

  if (( idx + 4 >= total )); then
    break
  fi

  instance_id="${rest[$idx]}"
  public_ip="${rest[$((idx+1))]}"
  # private_ip não é usado aqui; se precisar, é rest[$((idx+2))]
  role="${rest[$((idx+3))]}"
  slave_tag="${rest[$((idx+4))]}"

  if [[ "${role}" == "slave" && "${slave_tag}" == "${tag}" ]]; then
    echo "  [DEPLOY] Iniciando slave em ${public_ip}"
    ssh -f -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
      "Bruno@${public_ip}" \
      "cd /users/Bruno/iss && ./scripts/start-remote-single-slave.sh '${exp_data_dir}' '${tag}' '${instance_id}' '${master_ip}'" \
      || echo "  [WARN] Falha ao iniciar slave em ${public_ip}"
    ((started++))
    if (( started >= n )); then
      break
    fi
  fi

  ((idx+=5))
done

echo
echo "==== [start-remote-slaves] Todos os slaves disparados. ===="
echo "==== [start-remote-slaves] FIM ==========================================="
echo "====================================================================="

