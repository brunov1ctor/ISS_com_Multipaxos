#!/bin/bash

# scripts/start-remote-slaves.sh
#
# Dispara os slaves (peers/clients) em ambiente remoto (Emulab/cluster).
#
# Uso:
#   scripts/start-remote-slaves.sh <exp_data_dir> <instance_info_file> <tag>
#
# Onde:
#   <tag> é "peers", "1client", etc., conforme definido no instance-info:
#     peers   172.20.x.x  10.10.1.x  172.20.x.x  node-1
#     1client 172.20.y.y  10.10.1.y  172.20.y.y  node-6
#
# Este script:
#   1) Garante diretórios remotos (work_dir, exp_dir, config_dir, scripts, logs)
#   2) Copia scripts (start-slave.sh, stubborn-scp.sh, new-experiment-state.sh)
#   3) Copia binários (orderingpeer, orderingclient, discovery*)
#   4) Dispara start-slave.sh em cada máquina da tag desejada
#

set -euo pipefail

source scripts/global-vars.sh

if [[ $# -lt 3 ]]; then
  echo "Uso: $0 <exp_data_dir> <instance_info_file> <tag>"
  exit 1
fi

exp_data_dir="$1"
instance_info_file="$2"
wanted_tag="$3"

this_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
deployment_dir="$(cd "$this_dir/.." && pwd)"
repo_dir="$(cd "$deployment_dir/.." && pwd)"

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

# Função para tratar cada linha do instance-info
process_instance_line() {
  local line="$1"

  # Ignora vazias e comentários
  [[ -z "$line" ]] && return 0
  [[ "$line" =~ ^[[:space:]]*# ]] && return 0

  # Espera formato:
  #   <tag> <ctrl_ip> <private_ip> <public_ip> <instance_id>
  set -- $line
  local tag="$1"
  local ctrl_ip="$2"
  local private_ip="$3"
  local public_ip="$4"
  local instance_id="$5"

  # Só nos interessam as linhas da tag desejada
  if [[ "$tag" != "$wanted_tag" ]]; then
    return 0
  fi

  echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] ---------------------------------------------------------------------"
  echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   [REMOTO] Garantindo ambiente em $ctrl_ip"
  echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]            instance_id = $instance_id"
  echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]            tag         = $tag"

  # Garante diretórios remotos
  ssh ${ssh_options} "${remote_user}@${ctrl_ip}" "
    mkdir -p \
      '$remote_work_dir' \
      '$remote_exp_dir' \
      '$remote_config_dir' \
      '$remote_work_dir/scripts' \
      '$remote_work_dir/logs' \
      '$remote_work_dir/tls-data' \
      '$remote_bin_dir'
  " >/dev/null 2>&1 || true

  # Copia scripts (incluindo stubborn-scp) para o remote_work_dir/scripts
  bash "$this_dir/stubborn-scp.sh" \
    "$scp_retries" \
    "$tmp_scripts_dir/" \
    "${remote_user}@${ctrl_ip}:$remote_work_dir/scripts/"

  # Copia binários (orderingpeer, orderingclient, discovery*)
  bash "$this_dir/stubborn-scp.sh" \
    "$scp_retries" \
    "$remote_bin_dir/" \
    "${remote_user}@${ctrl_ip}:$remote_bin_dir/"

  echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]     [REMOTO] OK: ambiente garantido em $ctrl_ip."
}

# Percorre o instance-info
while IFS= read -r line; do
  process_instance_line "$line"
done < "$instance_info_file"

echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] ==== [start-remote-slaves] Distribuição concluída. ===="
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] "

# ===================================================================
# 2) Dispara os slaves da tag desejada
# ===================================================================

echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] ==== [start-remote-slaves] Disparando slaves da tag '$wanted_tag' ===="

start_instance_line() {
  local line="$1"

  [[ -z "$line" ]] && return 0
  [[ "$line" =~ ^[[:space:]]*# ]] && return 0

  set -- $line
  local tag="$1"
  local ctrl_ip="$2"
  local private_ip="$3"
  local public_ip="$4"
  local instance_id="$5"

  if [[ "$tag" != "$wanted_tag" ]]; then
    return 0
  fi

  echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   [DEPLOY] Iniciando slave em $ctrl_ip (instance_id=$instance_id, tag=$tag)"

  ssh ${ssh_options} "${remote_user}@${ctrl_ip}" "
    nohup '$remote_work_dir/scripts/start-slave.sh' \
      '$exp_data_dir' \
      '$ctrl_ip' \
      '$private_ip' \
      '$public_ip' \
      '$instance_id' \
      '$tag' \
      > '$remote_work_dir/logs/slave-${instance_id}.log' 2>&1 &
  " >/dev/null 2>&1 || true
}

while IFS= read -r line; do
  start_instance_line "$line"
done < "$instance_info_file"

echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] "
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] ==== [start-remote-slaves] Todos os slaves disparados. ===="
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] ==== [start-remote-slaves] FIM ==========================================="

