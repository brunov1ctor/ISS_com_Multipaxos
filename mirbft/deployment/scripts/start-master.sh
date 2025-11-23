#!/bin/bash

set -euo pipefail

source scripts/global-vars.sh

# Kill all children of this script when exiting
trap "$trap_exit_command" EXIT

exp_data_dir="$1"   # ex.: deployment-data/remote-0000
master_ip="$2"      # ex.: 172.20.3.2

###############################################################################
# Arquivo de comandos do master (já gerado pelo deploy-remote)
###############################################################################

export ssh_key_file="$remote_private_key_file"
export own_public_ip="$master_ip"
export request_payload_dir="$remote_request_payload_dir"

# Diretório local com os dados do experimento
export exp_dir="$exp_data_dir"

master_command_file="$exp_data_dir/$local_master_command_file"

echo ""
echo "Using pre-generated master command script at $master_command_file."
echo ""
echo "Master command script written to $master_command_file."
echo ""

###############################################################################
# Garantir diretórios remotos no master
###############################################################################

echo "Ensuring remote directories on master ($master_ip)."

ssh $ssh_options "$master_ip" "
  cd \"\$HOME\" &&
  mkdir -p iss \
           iss/experiment-config \
           iss/current-deployment-data \
           iss/current-deployment-data/raw-results
"

###############################################################################
# Copiar master-commands para o master
###############################################################################

echo "Copying master commands and configs to master."

./scripts/stubborn-scp.sh 10 \
  "$master_command_file" \
  "$master_ip:$remote_master_command_file"

###############################################################################
# Copiar TODOS os configs gerados para o master (experiment-config)
###############################################################################

# Os configs gerados estão em:  $exp_data_dir/config/config-000X.yml
# No master, vamos colocar em:  ~/iss/experiment-config/config-000X.yml
# que é exatamente o caminho usado no master-commands.cmd:
#   172.20.3.2:iss/experiment-config/config-0000.yml ...

if [ -d "$exp_data_dir/config" ]; then
  for cfg in "$exp_data_dir"/config/config-*.yml; do
    [ -f "$cfg" ] || continue

    cfg_base=$(basename "$cfg")   # ex.: config-0000.yml
    echo "Copying $cfg to master as iss/experiment-config/$cfg_base"

    ./scripts/stubborn-scp.sh 10 \
      "$cfg" \
      "$master_ip:iss/experiment-config/$cfg_base"
  done
else
  echo "WARNING: Directory $exp_data_dir/config not found; no configs copied to master." >&2
fi

echo "Done."

###############################################################################
# Iniciar análise contínua + discoverymaster no master
###############################################################################

echo "Starting result processor and master server."

ssh $ssh_options "$master_ip" "
  cd \"\$HOME\" &&
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

echo "Master startup sequence finished."

