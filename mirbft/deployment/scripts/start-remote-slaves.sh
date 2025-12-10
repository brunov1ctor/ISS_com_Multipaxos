#!/bin/bash

# scripts/start-remote-slaves.sh
#
# Dispara os slaves (peers/clients) em ambiente remoto (Emulab/cluster).
#
# Uso (conforme deploy.sh):
#   scripts/start-remote-slaves.sh <exp_data_dir> <tag> <instance_info_file>
#
# Exemplos (a partir do deploy.sh):
#   scripts/start-remote-slaves.sh deployment-data/remote-0000 peers   scripts/instance-info
#   scripts/start-remote-slaves.sh deployment-data/remote-0000 1client scripts/instance-info
#
# Onde o instance-info tem linhas do tipo:
#   peers   172.20.x.x  10.10.1.x  172.20.x.x  node-1
#   1client 172.20.y.y  10.10.1.y  172.20.y.y  node-6
#

set -euo pipefail

source scripts/global-vars.sh

if [[ $# -lt 3 ]]; then
  echo "Uso: $0 <exp_data_dir> <tag> <instance_info_file>"
  exit 1
fi

# Ordem correta vinda do deploy.sh:
#   $1 = exp_data_dir
#   $2 = tag (peers, 1client, etc.)
#   $3 = instance_info_file (scripts/instance-info)
exp_data_dir_arg="$1"
wanted_tag="$2"
instance_info_file="$3"

# Diretórios base
this_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
deployment_dir="$(cd "$this_dir/.." && pwd)"
repo_dir="$(cd "$deployment_dir/.." && pwd)"

# Normaliza exp_data_dir para caminho absoluto baseado em deployment_dir
if [[ "$exp_data_dir_arg" = /* ]]; then
  exp_data_dir="$exp_data_dir_arg"
else
  exp_data_dir="$deployment_dir/$exp_data_dir_arg"
fi

# Número de tentativas do stubborn-scp (pode sobrescrever via env scp_retries)
scp_retries="${scp_retries:-10}"

echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] ==== [start-remote-slaves] Diretórios detectados ====="
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   this_dir       = $this_dir"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   deployment_dir = $deployment_dir"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   repo_dir       = $repo_dir"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   remote_user    = $remote_user"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   remote_gopath  = $remote_gopath"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   remote_bin_dir = $remote_bin_dir"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   remote_work_dir= $remote_work_dir"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   remote_exp_dir = $remote_exp_dir"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] "

if [[ ! -f "$instance_info_file" ]]; then
  echo "[ERRO  ][$(date +"%Y-%m-%d %H:%M:%S")] Arquivo instance-info não encontrado: $instance_info_file"
  exit 1
fi

# ===================================================================
# 1) Distribui scripts e binários para os slaves da tag desejada
# ===================================================================

echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] ==== [start-remote-slaves] Distribuindo scripts/binários aos slaves ===="

# Prepara diretório temporário de scripts a serem enviados
tmp_scripts_dir="${exp_data_dir}/scripts-temp"
rm -rf "$tmp_scripts_dir"
mkdir -p "$tmp_scripts_dir"
cp "$this_dir/start-slave.sh" "$tmp_scripts_dir/"
cp "$this_dir/stubborn-scp.sh" "$tmp_scripts_dir/"
cp "$this_dir/new-experiment-state.sh" "$tmp_scripts_dir/"

process_instance_line() {
  local line="$1"

  # Ignora vazias e comentários
  [[ -z "$line" ]] && return

