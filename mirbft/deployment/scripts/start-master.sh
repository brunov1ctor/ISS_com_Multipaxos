#!/bin/bash
# scripts/start-master.sh
#
# Executado pelo deploy-remote.sh para:
#   - preparar diretórios no master remoto
#   - copiar master-commands.cmd e scripts auxiliares
#   - copiar arquivos de config gerados para o master
#   - disparar discoverymaster + orderingclient no master

set -euo pipefail

this_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
deployment_dir="$(cd "$this_dir/.." && pwd)"

# Carrega as variáveis globais (remote_work_dir, remote_exp_dir, ssh_options, etc.)
# shellcheck source=/dev/null
. "$deployment_dir/scripts/global-vars.sh"

# Argumentos:
#   $1 = exp_data_dir (LOCAL, no node-0)
#   $2 = master_ip
exp_data_dir="$1"
master_ip="$2"

local_master_cmd="$exp_data_dir/master-commands.cmd"
remote_master_cmd="$remote_work_dir/master-commands.cmd"

echo "Using experiment data directory: $exp_data_dir"
echo "Using master IP: $master_ip"
echo "Local master command script: $local_master_cmd"
echo "Remote work dir: $remote_work_dir"
echo "Remote master command path: $remote_master_cmd"
echo

echo "Ensuring remote directories on master ($master_ip)."

# remote_work_dir  -> /users/<user>/iss
# remote_exp_dir   -> /users/<user>/iss/current-deployment-data
ssh $ssh_options "$master_ip" " \
  mkdir -p \
    '$remote_work_dir' \
    '$remote_work_dir/config' \
    '$remote_work_dir/logs' \
    '$remote_work_dir/scripts' \
    '$remote_work_dir/tls-data' \
    '$remote_exp_dir' \
    '$remote_exp_dir/raw-results' \
    '$remote_work_dir/experiment-config' \
"

echo "Remote directories ensured."
echo

echo "Copying master commands and helper scripts to master."

# MASTER COMMANDS (LOCAL -> REMOTO)
scp $ssh_options \
  "$local_master_cmd" \
  "$master_ip:iss/master-commands.cmd"

# start-slave.sh (LOCAL -> REMOTO)
scp $ssh_options \
  "$deployment_dir/scripts/start-slave.sh" \
  "$master_ip:iss/scripts/start-slave.sh"

# stubborn-scp.sh (LOCAL -> REMOTO)
scp $ssh_options \
  "$deployment_dir/scripts/stubborn-scp.sh" \
  "$master_ip:iss/scripts/stubborn-scp.sh"

echo "Copying experiment config files to master."

# Configs locais geradas pelo generate-config.sh do deploy,
# que coloca os arquivos em: $exp_data_dir/config/config-000X.yml
local_config_src_dir="$exp_data_dir/config"

if ls "$local_config_src_dir"/config-*.yml >/dev/null 2>&1; then
  echo "  - Enviando configs de $local_config_src_dir para $master_ip:iss/experiment-config/ ..."
  scp $ssh_options \
    "$local_config_src_dir"/config-*.yml \
    "$master_ip:iss/experiment-config/"
else
  echo "WARNING: nenhum arquivo config-XXXX.yml encontrado em $local_config_src_dir; configs não foram copiadas."
fi

echo "Done."
echo

echo "Starting result processor and master server."
ssh $ssh_options "$master_ip" " \
  cd '$remote_work_dir' && \
  nohup ./start-master-remote.sh > main_log.log 2>&1 & \
"
echo "Master discovery + orderingclient disparados."
echo "start-master.sh finished."

