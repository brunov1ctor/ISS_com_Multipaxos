#!/bin/bash
# scripts/start-master.sh
#
# Executado pelo deploy-remote.sh para:
#   - preparar diretórios no master remoto
#   - copiar master-commands.cmd e scripts auxiliares
#   - copiar arquivos de config gerados para o master
#   - garantir que os binários Go (discoverymaster, discoveryslave, orderingpeer, orderingclient)
#     existam em $HOME/go/bin (e compilá-los se necessário)
#   - disparar discoverymaster no master remoto

set -euo pipefail

# ---------------------------------------------------------------------------
# 0) Diretórios básicos (lado "deployment", no node-0)
# ---------------------------------------------------------------------------

this_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
deployment_dir="$(cd "$this_dir/.." && pwd)"
repo_dir="$(cd "$deployment_dir/.." && pwd)"   # raiz do repo mirbft (onde está o go.mod)

# ---------------------------------------------------------------------------
# 1) Funções auxiliares
# ---------------------------------------------------------------------------

usage() {
  echo "Uso: $0 <exp_data_dir> <master_ip>" >&2
  exit 1
}

# Garante que os binários Go necessários existam em \$HOME/go/bin.
# Se não existirem, roda 'go install ./cmd/...' para cada um, com logs.
ensure_go_binaries() {
  local local_bin_dir="${LOCAL_BIN_DIR:-$HOME/go/bin}"
  local -a bins=(discoverymaster discoveryslave orderingpeer orderingclient)
  local -a missing=()

  echo "[start-master] Verificando binários em ${local_bin_dir}..."

  for b in "${bins[@]}"; do
    if [[ ! -x "${local_bin_dir}/${b}" ]]; then
      missing+=("$b")
    fi
  done

  if [[ ${#missing[@]} -eq 0 ]]; then
    echo "[start-master] Todos os binários Go já existem em ${local_bin_dir}."
    return 0
  fi

  echo "[start-master] Binários faltando em ${local_bin_dir}: ${missing[*]}"
  echo "[start-master] Compilando binários Go a partir de ${repo_dir} ..."

  pushd "${repo_dir}" >/dev/null

  for b in "${missing[@]}"; do
    local pkg=""
    case "$b" in
      discoverymaster)  pkg="./cmd/discoverymaster"  ;;
      discoveryslave)   pkg="./cmd/discoveryslave"   ;;
      orderingpeer)     pkg="./cmd/orderingpeer"     ;;
      orderingclient)   pkg="./cmd/orderingclient"   ;;
      *)
        echo "[start-master] (WARN) binário desconhecido '$b', ignorando." >&2
        continue
      ;;
    esac

    echo "[start-master]   go install ${pkg} -> ${local_bin_dir}/${b}"
    if ! GOBIN="${local_bin_dir}" GO111MODULE=on go install "${pkg}"; then
      echo "[start-master] ERRO ao compilar ${pkg}." >&2
      exit 1
    fi
  done

  popd >/dev/null

  echo "[start-master] Compilação de binários concluída."
  echo "[start-master] Conteúdo de ${local_bin_dir}:"
  ls -l "${local_bin_dir}" || true
}

# ---------------------------------------------------------------------------
# 2) Parâmetros
# ---------------------------------------------------------------------------

if [[ $# -ne 2 ]]; then
  usage
fi

exp_data_dir="$1"   # ex.: deployment-data/remote-0000
master_ip="$2"

# Normaliza exp_data_dir para caminho absoluto, se vier relativo
if [[ "$exp_data_dir" != /* ]]; then
  exp_data_dir="${deployment_dir}/${exp_data_dir}"
fi

if [[ ! -d "$exp_data_dir" ]]; then
  echo "[start-master] ERRO: diretório de experimento não existe: $exp_data_dir" >&2
  exit 1
fi

local_master_cmd="${exp_data_dir}/master-commands.cmd"

if [[ ! -f "$local_master_cmd" ]]; then
  echo "[start-master] ERRO: master-commands.cmd não encontrado em ${local_master_cmd}" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# 3) Garantir binários Go antes de mexer com o master
# ---------------------------------------------------------------------------

ensure_go_binaries

# Diretório onde os binários foram instalados (o mesmo usado em ensure_go_binaries)
local_bin_dir="${LOCAL_BIN_DIR:-$HOME/go/bin}"

# ---------------------------------------------------------------------------
# 4) Variáveis globais compartilhadas com o lado remoto
# ---------------------------------------------------------------------------

# Vamos reutilizar as mesmas variáveis que os outros scripts de deployment,
# mas sem depender de global-vars.sh no lado deployment.
remote_user="${DEPL_REMOTE_USER:-$USER}"

remote_work_dir="${REMOTE_WORK_DIR:-/users/${remote_user}/iss}"
remote_exp_dir="${REMOTE_EXP_DIR:-${remote_work_dir}/current-deployment-data}"
remote_bin_dir="${REMOTE_BIN_DIR:-/users/${remote_user}/go/bin}"

remote_ready_file="${REMOTE_READY_FILE:-${remote_work_dir}/master-ready}"
remote_status_file="${REMOTE_STATUS_FILE:-${remote_work_dir}/status}"
master_port="${MASTER_PORT:-9999}"

ssh_options="-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null"

remote_master_cmd="${remote_work_dir}/master-commands.cmd"

echo "Using experiment data directory: ${exp_data_dir}"
echo "Using master IP: ${master_ip}"
echo "Local master command script: ${local_master_cmd}"
echo "Remote work dir: ${remote_work_dir}"
echo "Remote master command path: ${remote_master_cmd}"
echo

# ---------------------------------------------------------------------------
# 5) Garante estrutura de diretórios no master remoto
# ---------------------------------------------------------------------------

echo "Ensuring remote directories on master (${master_ip})."

ssh ${ssh_options} "${remote_user}@${master_ip}" " \
  mkdir -p \
    '${remote_work_dir}' \
    '${remote_work_dir}/config' \
    '${remote_work_dir}/logs' \
    '${remote_work_dir}/scripts' \
    '${remote_work_dir}/tls-data' \
    '${remote_exp_dir}' \
    '${remote_exp_dir}/raw-results' \
    '${remote_work_dir}/experiment-config' \
"

echo "Remote directories ensured."
echo

# ---------------------------------------------------------------------------
# 6) Copiar master-commands e scripts auxiliares para o master
# ---------------------------------------------------------------------------

echo "Copying master commands and helper scripts to master."

scp ${ssh_options} \
  "${local_master_cmd}" \
  "${remote_user}@${master_ip}:${remote_master_cmd}"

# start-slave.sh (que será usado nos slaves)
scp ${ssh_options} \
  "${this_dir}/start-slave.sh" \
  "${remote_user}@${master_ip}:${remote_work_dir}/scripts/start-slave.sh"

# stubborn-scp.sh (apoiando cópias robustas)
scp ${ssh_options} \
  "${this_dir}/stubborn-scp.sh" \
  "${remote_user}@${master_ip}:${remote_work_dir}/scripts/stubborn-scp.sh"

# global-vars.sh (para o ambiente remoto interpretar paths/variáveis)
scp ${ssh_options} \
  "${this_dir}/global-vars.sh" \
  "${remote_user}@${master_ip}:${remote_work_dir}/scripts/global-vars.sh"

echo "Copying experiment config files to master."

local_config_src_dir="${exp_data_dir}/config"

if ls "${local_config_src_dir}"/config-*.yml >/dev/null 2>&1; then
  echo "  - Enviando configs de ${local_config_src_dir} para ${master_ip}:${remote_work_dir}/experiment-config/ ..."
  scp ${ssh_options} \
    "${local_config_src_dir}"/config-*.yml \
    "${remote_user}@${master_ip}:${remote_work_dir}/experiment-config/"
else
  echo "WARNING: nenhum arquivo config-XXXX.yml encontrado em ${local_config_src_dir}; configs não foram copiadas."
fi

echo "Done."
echo

# ---------------------------------------------------------------------------
# 7) Disparar discoverymaster no master remoto
# ---------------------------------------------------------------------------

echo "Starting discoverymaster on remote master (${master_ip})."
echo "  - remote_bin_dir     = ${remote_bin_dir}"
echo "  - remote_ready_file  = ${remote_ready_file}"
echo "  - remote_status_file = ${remote_status_file}"
echo "  - master_port        = ${master_port}"
echo

ssh ${ssh_options} "${remote_user}@${master_ip}" " \
  cd '${remote_work_dir}' && \
  rm -f '${remote_ready_file}' && \
  echo 'RUNNING' > '${remote_status_file}' && \
  nohup '${remote_bin_dir}/discoverymaster' \
    -masterAddr='${master_ip}:${master_port}' \
    -cmdFile='${remote_master_cmd}' \
    -resultDir='${remote_exp_dir}/raw-results' \
    -readyFile='${remote_ready_file}' \
    -statusFile='${remote_status_file}' \
    > main_log.log 2>&1 & \
  echo 'discoverymaster started on ${master_ip}' \
"

echo "Master discovery + orderingclient disparados via discoverymaster."
echo "start-master.sh finished."

