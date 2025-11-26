#!/usr/bin/env bash
#
# start-remote-slaves.sh — versão genérica (sem usuário hard-coded)
#
# - Compila automaticamente os binários no nó local (node-0)
# - Verifica os binários gerados
# - Distribui binários + scripts/ para todos os slaves
# - Dispara os slaves de uma TAG específica (peers, 1client, etc.)
#
# Assumindo:
#   * Você está rodando a partir do diretório 'deployment'
#   * 'scripts/global-vars.sh' define caminhos remotos (remote_work_dir, remote_gopath, ...)
#
set -uo pipefail

exp_data_dir="$1"
tag="$2"
n="$3"
master_ip="$4"
shift 4

echo "====================================================================="
echo "=== [start-remote-slaves] INÍCIO ===================================="
echo "  exp_data_dir = ${exp_data_dir}"
echo "  tag          = ${tag}"
echo "  n            = ${n}"
echo "  master_ip    = ${master_ip}"
echo "  args rest    = $*"
echo "====================================================================="
echo

# ----------------------------------------------------------------------
# Diretórios base (local) e variáveis globais
# ----------------------------------------------------------------------
# BASE_DIR = diretório 'deployment'
BASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${BASE_DIR}"

# Carrega variáveis compartilhadas (ssh_options, remote_work_dir, remote_gopath, etc.)
source scripts/global-vars.sh

# Raiz do repositório (um nível acima de deployment)
local_repo="$(cd "${BASE_DIR}/.." && pwd)"

# Caminhos locais de Go:
#   - se GOBIN já estiver definido, usamos
#   - senão, se GOPATH existir, usamos GOPATH/bin
#   - senão, caímos no remote_gopath definido em global-vars.sh
if [[ -n "${GOBIN:-}" ]]; then
  local_bin_dir="${GOBIN%/}"
elif [[ -n "${GOPATH:-}" ]]; then
  local_bin_dir="${GOPATH%/}/bin"
else
  # Fallback: usa remote_gopath também como GOPATH local
  GOPATH="${remote_gopath%/}"
  export GOPATH
  local_bin_dir="${GOPATH}/bin"
fi

export GOPATH="${GOPATH:-${remote_gopath%/}}"
export GOBIN="${local_bin_dir}"
export PATH="${GOBIN}:/usr/local/go/bin:${PATH}"

local_discoverymaster="${local_bin_dir}/discoverymaster"
local_discoveryslave="${local_bin_dir}/discoveryslave"
local_orderingpeer="${local_bin_dir}/orderingpeer"
local_orderingclient="${local_bin_dir}/orderingclient"

# Estes são relativos ao diretório de deployment (BASE_DIR)
local_start_slave_script="${BASE_DIR}/scripts/start-slave.sh"
local_scripts_dir="${BASE_DIR}/scripts"

# Caminhos remotos (derivados de global-vars.sh)
remote_bin_dir="${remote_gopath%/}/bin"
remote_logs_dir="${remote_work_dir%/}/logs"

# ----------------------------------------------------------------------
# Auto-Compilação dos binários
# ----------------------------------------------------------------------
echo
echo "==== [start-remote-slaves] (LOCAL) Compilando binários ====="
echo "  Repositório: ${local_repo}"

cd "${local_repo}" || {
  echo "  [LOCAL] ERRO: diretório ${local_repo} não encontrado!"
  exit 1
}

echo "  Executando: go install ./cmd/..."
if ! go install ./cmd/... ; then
  echo "  [LOCAL] ERRO: falha ao compilar binários!"
  exit 1
fi

echo "  [LOCAL] Compilação concluída."
echo

# Volta para o diretório 'deployment'
cd "${BASE_DIR}" || {
  echo "  [LOCAL] ERRO: não foi possível voltar para ${BASE_DIR}"
  exit 1
}

# ----------------------------------------------------------------------
# Verificação dos binários compilados
# ----------------------------------------------------------------------
echo "==== [start-remote-slaves] (LOCAL) Verificando binários em ${local_bin_dir} ===="
echo "  remote_gopath = ${remote_gopath}"
echo "  local_bin_dir = ${local_bin_dir}"

for bin in "${local_discoverymaster}" "${local_discoveryslave}" "${local_orderingpeer}" "${local_orderingclient}"; do
  if [[ ! -x "${bin}" ]]; then
    echo "  [LOCAL] ERRO: binário não encontrado após compilação: ${bin}"
    exit 1
  else
    echo "  [LOCAL] OK: ${bin}"
  fi
done

# Verificar start-slave.sh
if [[ ! -f "${local_start_slave_script}" ]]; then
  echo "  [LOCAL] ERRO: script ${local_start_slave_script} não encontrado!"
  exit 1
fi

# Verificar diretório scripts/ (contém stubborn-scp.sh, analyze/, etc.)
if [[ ! -f "${local_scripts_dir}/stubborn-scp.sh" ]]; then
  echo "  [LOCAL] ERRO: ${local_scripts_dir}/stubborn-scp.sh não encontrado!"
  exit 1
fi

echo "==== [start-remote-slaves] Binários + scripts verificados. ===="
echo

# ----------------------------------------------------------------------
# Função para preparar cada slave
# ----------------------------------------------------------------------
ensure_remote_slave() {
  local instance_id="$1"
  local public_ip="$2"
  local private_ip="$3"
  local role="$4"
  local slave_tag="$5"

  if [[ "${role}" != "slave" ]]; then
    echo "    [REMOTO] role='${role}' (não é slave); ignorando."
    return
  fi

  echo "---------------------------------------------------------------------"
  echo "  [REMOTO] Garantindo ambiente em ${public_ip}"
  echo "           instance_id = ${instance_id}"
  echo "           tag         = ${slave_tag}"
  echo "---------------------------------------------------------------------"

  # Criar diretórios remotos
  ssh ${ssh_options} "${public_ip}" "
    mkdir -p '${remote_bin_dir}' '${remote_work_dir}' '${remote_logs_dir}'
  " >/dev/null 2>&1

  # Copiar binários
  scp ${ssh_options} \
    "${local_discoverymaster}" \
    "${local_discoveryslave}" \
    "${local_orderingpeer}" \
    "${local_orderingclient}" \
    "${public_ip}:${remote_bin_dir}/" >/dev/null 2>&1

  # Copiar start-slave.sh
  scp ${ssh_options} \
    "${local_start_slave_script}" \
    "${public_ip}:${remote_work_dir}/start-slave.sh" >/dev/null 2>&1

  # Copiar diretório scripts/
  scp -r ${ssh_options} \
    "${local_scripts_dir}" \
    "${public_ip}:${remote_work_dir}/" >/dev/null 2>&1

  # Permissões
  ssh ${ssh_options} "${public_ip}" "
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
# Distribuição (garantir ambiente em todos os slaves)
# ----------------------------------------------------------------------
echo "==== [start-remote-slaves] (REMOTO) Garantindo binários e scripts ===="

rest=("$@")
idx=0
total=${#rest[@]}

while (( idx < total )); do
  key="${rest[$idx]}"

  if [[ "${key}" == "skip" ]]; then
    # Formato: skip <algo> <algo> → pula 3 entradas
    ((idx+=3))
    continue
  fi

  if (( idx + 4 >= total )); then
    echo "  [REMOTO-AVISO] Argumentos insuficientes."
    break
  fi

  ensure_remote_slave \
    "${rest[$idx]}"       \
    "${rest[$((idx+1))]}" \
    "${rest[$((idx+2))]}" \
    "${rest[$((idx+3))]}" \
    "${rest[$((idx+4))]}"

  ((idx+=5))
done

echo "==== [start-remote-slaves] Distribuição concluída. ===="
echo

# ----------------------------------------------------------------------
# Disparar slaves da tag desejada
# ----------------------------------------------------------------------
echo "==== [start-remote-slaves] Iniciando slaves da TAG '${tag}' (n = ${n}) ===="

started=0
idx=0

while (( idx < total )); do
  key="${rest[$idx]}"

  if [[ "${key}" == "skip" ]]; then
    ((idx+=3))
    continue
  fi

  if (( idx + 4 >= total )); then
    break
  fi

  instance_id="${rest[$idx]}"
  public_ip="${rest[$((idx+1))]}"
  private_ip="${rest[$((idx+2))]}"
  role="${rest[$((idx+3))]}"
  slave_tag="${rest[$((idx+4))]}"

  if [[ "${role}" != "slave" || "${slave_tag}" != "${tag}" ]]; then
    ((idx+=5))
    continue
  fi

  if (( started >= n )); then
    ((idx+=5))
    continue
  fi

  log_file="${exp_data_dir}/ssh-${tag}-${public_ip}.log"
  echo "  [DEPLOY] Iniciando slave em ${public_ip}"

  (
    ssh ${ssh_options} "${public_ip}" "
      cd '${remote_work_dir}' || exit 1
      nohup ./start-slave.sh \
        '${tag}' '${master_ip}' '${public_ip}' '${private_ip}' \
        > '${remote_logs_dir}/start-slave-${tag}.log' 2>&1 &
    "
  ) > "${log_file}" 2>&1 &

  started=$((started + 1))
  sleep 0.2

  ((idx+=5))
done

echo
echo "==== [start-remote-slaves] Todos os slaves disparados. ===="
wait || true
echo "==== [start-remote-slaves] FIM ==========================================="
echo "====================================================================="

