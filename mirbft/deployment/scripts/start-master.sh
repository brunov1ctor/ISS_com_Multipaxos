#!/bin/bash
# scripts/start-master.sh
#
# Executado pelo deploy-remote.sh para:
#   - preparar diretórios no master remoto
#   - copiar master-commands.cmd
#   - disparar discoverymaster + orderingclient no master

set -euo pipefail

this_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
deployment_dir="$(cd "$this_dir/.." && pwd)"

# Argumentos:
#   $1 = exp_data_dir (local, no node-0)
#   $2 = master_ip
exp_data_dir="$1"
master_ip="$2"

# global-vars.sh já foi sourced pelo deploy.sh,
# então temos:
#   remote_work_dir, remote_exp_dir, remote_config_dir,
#   remote_status_file, ssh_options, etc.

local_master_cmd="$exp_data_dir/master-commands.cmd"
remote_master_cmd="$remote_work_dir/master-commands.cmd"

echo "Using experiment data directory: $exp_data_dir"
echo "Using master IP: $master_ip"
echo "Local master command script: $local_master_cmd"
echo "Remote work dir: $remote_work_dir"
echo "Remote master command path: $remote_master_cmd"
echo

echo "Ensuring remote directories on master ($master_ip)."

ssh $ssh_options "$master_ip" " \
  mkdir -p \
    '$remote_work_dir' \
    '$remote_work_dir/config' \
    '$remote_work_dir/logs' \
    '$remote_work_dir/scripts' \
    '$remote_work_dir/tls-data' \
    '$remote_exp_dir' \
    '$remote_exp_dir/raw-results' \
    '$remote_work_dir/experiment-config'
"

echo "Remote directories ensured."
echo

echo "Copying master commands and configs to master."

# Copia o master-commands.cmd para o master.
\"$deployment_dir/scripts/stubborn-scp.sh\" 10 -i \
  \"$local_master_cmd\" \
  \"$master_ip:iss/master-commands.cmd\"

# Copia scripts auxiliares que o master precisa (se ainda não estiverem lá).
\"$deployment_dir/scripts/stubborn-scp.sh\" 10 -i \
  \"$deployment_dir/scripts/start-slave.sh\" \
  \"$master_ip:iss/scripts/start-slave.sh\"

\"$deployment_dir/scripts/stubborn-scp.sh\" 10 -i \
  \"$deployment_dir/scripts/stubborn-scp.sh\" \
  \"$master_ip:iss/scripts/stubborn-scp.sh\"

echo "Copying experiment config files to master."

# Garante que o diretório de configs exista no master.
ssh $ssh_options \"$master_ip\" \"mkdir -p '$remote_work_dir/experiment-config'\"

# Copia todos os config-000X.yml gerados pelo generate-config.sh
\"$deployment_dir/scripts/stubborn-scp.sh\" 10 -i \
  \"\$HOME/iss/experiment-config/config-\"*.yml \
  \"$master_ip:iss/experiment-config/\"

echo "Done."
echo

# Inicia discovery + orderingclient no master via start-master-remote.sh
echo "Starting result processor and master server."
ssh $ssh_options \"$master_ip\" \" \
  cd '$remote_work_dir' && \
  nohup ./start-master-remote.sh > main_log.log 2>&1 & \
\"
echo "Master discovery + orderingclient disparados."
echo "start-master.sh finished."

