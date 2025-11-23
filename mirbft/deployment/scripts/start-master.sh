#!/bin/bash

source scripts/global-vars.sh

# Kill all children of this script when exiting
trap "$trap_exit_command" EXIT

exp_data_dir=$1
master_ip=$2

###############################################################################
# Gerar master-commands.cmd
###############################################################################

export ssh_key_file=$remote_private_key_file
export own_public_ip=$master_ip
export request_payload_dir=$remote_request_payload_dir

# Diretório local onde o experimento será rodado (vai virar $remote_exp_dir)
export exp_dir=$exp_data_dir

# Arquivo de saída dos commands (lido pelo discoverymaster)
# IMPORTANTE: aqui usamos o arquivo JÁ GERADO pelo initialize-deployment.sh
master_command_file="$exp_data_dir/$local_master_command_file"

echo ""
echo "Using pre-generated master command script at $master_command_file."
echo ""
echo "Master command script written to $master_command_file."
echo ""

###############################################################################
# Copiar master-commands e config do master
###############################################################################

echo "Copying master commands and configs to master."

# Copia o master-commands para o master
./scripts/stubborn-scp.sh 10 \
  "$master_command_file" \
  "$master_ip:$remote_master_command_file" || exit 4

# Copia o config-0000.yml (da pasta config/) para o master, se existir
master_local_cfg="$exp_data_dir/config/config-0000.yml"

if [ -f "$master_local_cfg" ]; then
  ./scripts/stubborn-scp.sh 10 \
    "$master_local_cfg" \
    "$master_ip:$remote_master_config_file" || exit 4
else
  echo "WARNING: master config file not found at $master_local_cfg. Skipping copy to master."
fi

echo "Done."

###############################################################################
# NÃO vamos copiar configs para os slaves aqui.
# A distribuição/configuração dos slaves é tratada pelos outros scripts
# (start-remote-slaves, deploy-remote, etc.).
###############################################################################

###############################################################################
# Iniciar processador contínuo + master
###############################################################################

echo "Starting result processor and master server."
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

