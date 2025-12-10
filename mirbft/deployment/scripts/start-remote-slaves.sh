#!/bin/bash

# scripts/start-remote-slaves.sh
#
# Dispara os slaves (peers/clients) em ambiente remoto (Emulab/cluster).
#
# Uso:
#   scripts/start-remote-slaves.sh <exp_data_dir> <instance_info_file> <tag>
#
# Onde:
#   <tag> é "peers", "1client", etc., conforme definido no instance-info.
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

# --------------------------------------------------------------------
# 1) Distribui scripts e binários para os slaves da tag desejada
# --------------------------------------------------------------------

echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] ==== [start-remote-slaves] Distribuindo scripts/binários aos slaves ===="

while read -r tag ctrl_ip private_ip public_ip instance_id; do
  # pula linhas vazias ou comentários
  [[ -z "$tag" ]] && continue
  [[ "$tag" == \#* ]] && continue

  # só nos interessam as linhas da tag desejada
  if [[ "$tag" != "$wanted_tag" ]]; then
    continue
  fi

  echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] ---------------------------------------------------------------------"
  echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   [REMOTO] Garantindo ambiente em $ctrl_ip"
  echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]            instance_id = $instance_id"
  echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]            tag         = $tag"

  ssh ${ssh_options} "${remote_user}@${ctrl_ip}" "
    mkdir -p \
      '$remote_work_dir' \
      '$remote_exp_dir' \
      '$remote_config_dir' \
      '$remote_work_dir/scripts' \
      '$remote_work_dir/logs' \
      '$remote_work_dir/tls-data' \
      '$remote_work_dir/bin'
  " >/dev/null 2>&1 || true

  # Copia scripts necessários (incluindo stubborn-scp.sh)
  mkdir -p "$exp_data_dir/scripts-temp"
  cp "$this_dir/start-slave.sh" "$exp_data_dir/scripts-temp/"
  cp "$this_dir/stubborn-scp.sh" "$exp_data_dir/scripts-temp/"
  cp "$this_dir/new-experiment-state.sh" "$exp_data_dir/scripts-temp/"

  bash "${this_dir}/stubborn-scp.sh" \
    "$scp_retries" \
    "$exp_data_dir/scripts-temp/" \
    "${remote_user}@${ctrl_ip}:$remote_work_dir/scripts/"

  # Copia binários (orderingpeer, orderingclient, discovery*)
  bash "${this_dir}/stubborn-scp.sh" \
    "$scp_retries" \
    "$remote_bin_dir/" \
    "${remote_user}@${ctrl_ip}:$remote_bin_dir/"

  echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]     [REMOTO] OK: ambiente garantido em $ctrl_ip."
done < "$instance_info_file"

echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] ==== [start-remote-slaves] Distribuição concluída. ===="
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] "

# --------------------------------------------------------------------
# 2) Dispara os slaves da tag desejada
# --------------------------------------------------------------------

echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] ==== [start-remote-slaves] Disparando slaves da tag '$wanted_tag' ===="

while read -r tag ctrl_ip private_ip public_ip instance_id; do
  [[ -z "$tag" ]] && continue
  [[ "$tag" == \#* ]] && continue

  if [[ "$tag" != "$wanted_tag" ]]; then
    continue
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
      > '$remote_work_dir/logs/slave-\${instance_id}.log' 2>&1 &
  " >/dev/null 2>&1 || true
done < "$instance_info_file"

echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] "
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] ==== [start-remote-slaves] Todos os slaves disparados. ===="
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] ==== [start-remote-slaves] FIM ==========================================="

