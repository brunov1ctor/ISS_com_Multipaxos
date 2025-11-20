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
export master_port
export status_file=$remote_status_file
export ready_file=$remote_ready_file

envsubst '$ssh_key_file $own_public_ip $master_port $status_file $ready_file' \
  < "$exp_data_dir/$local_master_command_template_file" \
  > "$exp_data_dir/$local_master_command_file"

echo -e "\nwrite-file $status_file DONE" >> "$exp_data_dir/$local_master_command_file"

###############################################################################
# Criar diretórios remotos no MASTER
###############################################################################

ssh $ssh_options "$master_ip" "
  mkdir -p \"$remote_code_dir\" &&
  mkdir -p \"$remote_config_dir\" &&
  mkdir -p \"$remote_exp_dir/raw-results\"
" || exit 1

###############################################################################
# Enviar código para o MASTER
###############################################################################

rsync --progress -rptz -e "ssh $ssh_options" \
  $local_code_files \
  "$master_ip:$remote_code_dir" || exit 2

###############################################################################
# Enviar configs
###############################################################################

rsync --progress -rptz -e "ssh $ssh_options" \
  "$exp_data_dir/config/"* \
  "$master_ip:$remote_config_dir" || exit 3

###############################################################################
# Enviar scripts de análise e queries
###############################################################################

rsync --progress -rptz -e "ssh $ssh_options" \
  queries scripts \
  "$master_ip:$remote_work_dir" || exit 4

###############################################################################
# Enviar master-commands.cmd
###############################################################################

scp $ssh_options \
  "$exp_data_dir/$local_master_command_file" \
  "$master_ip:$remote_master_command_file" || exit 5

###############################################################################
# Gerar TLS e Compilar ISS no MASTER
# (sem run-protoc.sh, usando os .pb.go já no repo)
###############################################################################

ssh $ssh_options "$master_ip" "
  set -e

  cd \"$remote_tls_directory\" &&
  ./generate.sh \"$master_ip\" &&

  cd \"$remote_work_dir\" &&
  cp -r \"$remote_tls_directory\" . &&

  echo 'Compiling ISS (sem protoc).' &&
  export PATH=\"\$PATH:$remote_gopath/bin:$remote_work_dir/bin\" &&
  export GOPATH=\"$remote_gopath\" &&
  export GO111MODULE=auto &&
  export GOCACHE=\"$remote_work_dir/.cache/go-build\" &&

  cd \"$remote_code_dir\" &&
  go install ./cmd/...
" || exit 6

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

  discoverymaster $master_port file \"$remote_master_command_file\" \
    > \"$remote_master_log\" 2>&1 < /dev/null
"

