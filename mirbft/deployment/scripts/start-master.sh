#!/bin/bash

# Inicia o discoverymaster e o analisador contínuo no nó master.
# Uso: ./scripts/start-master.sh <exp_data_dir> <master_ip>

set -e

source scripts/global-vars.sh

# Mata todos os filhos deste script ao sair
trap "$trap_exit_command" EXIT

if [ $# -ne 2 ]; then
  echo "Uso: $0 <exp_data_dir> <master_ip>" >&2
  exit 1
fi

exp_data_dir="$1"
master_ip="$2"

###############################################################################
# Arquivo de comandos do master
###############################################################################

master_command_file="$exp_data_dir/$local_master_command_file"

echo ""
echo "Using pre-generated master command script at $master_command_file."
echo ""
echo "Master command script written to $master_command_file."
echo ""

###############################################################################
# Copiar master-commands para o master
###############################################################################

echo "Copying master commands to master node."

./scripts/stubborn-scp.sh 10 \
  "$master_command_file" \
  "$master_ip:$remote_master_command_file"

echo "Master command file copied."

###############################################################################
# Iniciar processador contínuo + master
###############################################################################

echo "Starting result processor and master server on $master_ip."
ssh $ssh_options "$master_ip" "
  ulimit -Sn $open_files_limit &&
  export PATH=\"\$PATH:$remote_gopath/bin:$remote_work_dir/bin\" &&

  \"$remote_work_dir/scripts/analyze/analyze-continuously.sh\" \
    \"$remote_exp_dir\" \
    \"$remote_status_file\" \
    \"$remote_work_dir/scripts\" \
    \"$remote_work_dir/queries\" \
    \"$remote_gopath/bin/orderingpeer\" \
    \"$remote_gopath/bin/orderingclient\" \
    $remote_analysis_processes \
    > \"$remote_exp_dir/continuous-analysis.log\" 2>&1 &

  \"$remote_gopath/bin/discoverymaster\" $master_port file \"$remote_master_command_file\" \
    > \"$remote_master_log\" 2>&1 < /dev/null
"

echo "Master node started."

