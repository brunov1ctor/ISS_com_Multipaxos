#!/usr/bin/env bash
set -euo pipefail

# Usage: start-slave.sh <tag> <master_ip> <own_public_ip> <own_private_ip> <remote_exp_dir>
tag="${1:-}"
master_ip="${2:-}"
own_public_ip="${3:-}"
own_private_ip="${4:-}"
remote_exp_dir="${5:-}"

if [[ -z "${tag}" || -z "${master_ip}" || -z "${own_public_ip}" || -z "${own_private_ip}" || -z "${remote_exp_dir}" ]]; then
  echo "Usage: $0 <tag> <master_ip> <own_public_ip> <own_private_ip> <remote_exp_dir>"
  exit 2
fi

# shellcheck source=/dev/null
source "$(dirname "$0")/global-vars.sh"

DISCOVERY_PORT="${DISCOVERY_PORT:-${master_port:-9999}}"
export DISCOVERY_PORT

echo "[start-slave] tag=${tag} master=${master_ip}:${DISCOVERY_PORT} pub=${own_public_ip} priv=${own_private_ip}"
echo "[start-slave] remote_work_dir=${remote_work_dir} remote_exp_dir=${remote_exp_dir}"
echo "[start-slave] remote_base_dir=${remote_base_dir} (configs/TLS)"

########################################
# Mata processos antigos
########################################
echo "[start-slave] Killing old processes..."
pkill -9 -f "${remote_bin_dir}/discoveryslave" 2>/dev/null || true
pkill -9 -f "discoveryslave ${tag} " 2>/dev/null || true
pkill -9 -f "${remote_bin_dir}/orderingpeer" 2>/dev/null || true
pkill -9 -f "${remote_bin_dir}/orderingclient" 2>/dev/null || true
sleep 0.3

########################################
# Limpa estado antigo
########################################
echo "[start-slave] Wiping stale state files..."

# estado “global” antigo no work_dir
rm -f "${remote_work_dir}"/.discovery*           2>/dev/null || true
rm -f "${remote_work_dir}"/.discoveryslave*      2>/dev/null || true
rm -f "${remote_work_dir}"/discoveryslave*.pid   2>/dev/null || true
rm -f "${remote_work_dir}"/slave*.pid            2>/dev/null || true
rm -f "${remote_work_dir}"/status                2>/dev/null || true

# estado de experimento no exp_dir
rm -rf "${remote_exp_dir}/state"        2>/dev/null || true
rm -rf "${remote_exp_dir}/slave-state"  2>/dev/null || true

########################################
# Prepara diretórios
########################################
# diretórios “globais” (pesado) no work_dir
mkdir -p \
  "${remote_work_dir}/logs" \
  "${remote_work_dir}/bin"

# diretórios leves (configs/TLS) em /users/<user>/iss
mkdir -p \
  "${remote_base_dir}/config" \
  "${remote_base_dir}/tls-data"

# diretórios do experimento (onde o discovery/peers vão gravar)
mkdir -p \
  "${remote_exp_dir}/experiment-output/0000" \
  "${remote_exp_dir}/raw-results/experiment-output" \
  "${remote_exp_dir}/_debug"

echo "[start-slave] PWD=${remote_exp_dir}"
echo "[start-slave] Preparado diretórios básicos:"
echo "  - experiment-output/"
echo "  - experiment-output/0000/"
echo "  - raw-results/experiment-output/"
echo "  - _debug/"

remote_scripts_dir="${remote_work_dir}/scripts"
export PATH="${remote_scripts_dir}:${remote_bin_dir}:${PATH}"

# Exporta dicas de diretório para os binários Go (caso usem)
export MIR_REMOTE_EXP_DIR="${remote_exp_dir}"
export MIR_REMOTE_WORK_DIR="${remote_work_dir}"

export master_port="${DISCOVERY_PORT}"
export own_public_ip="${own_public_ip}"
export own_private_ip="${own_private_ip}"

########################################
# Importante: rodar discoveryslave a partir do remote_exp_dir
# para que paths relativos (experiment-output/...) caiam aqui.
########################################
cd "${remote_exp_dir}"

echo "[start-slave] Starting discoveryslave (fresh)..."

exec /usr/bin/nohup \
  "${remote_bin_dir}/discoveryslave" \
  "${tag}" \
  "${master_ip}:${DISCOVERY_PORT}" \
  "${own_public_ip}" \
  "${own_private_ip}" \
  > "${remote_work_dir}/logs/discoveryslave-${tag}.log" 2>&1 < /dev/null

