#!/bin/bash

source scripts/global-vars.sh

# Kill all children of this script when exiting
trap "$trap_exit_command" EXIT

exp_data_dir=$1
master_ip=$2

# Generate final master command file
export ssh_key_file=$remote_private_key_file
export own_public_ip=$master_ip
export master_port
export status_file=$remote_status_file
export ready_file=$remote_ready_file

envsubst '$ssh_key_file $own_public_ip $master_port $status_file $ready_file' \
  < "$exp_data_dir/$local_master_command_template_file" \
  > "$exp_data_dir/$local_master_command_file"

echo -e "\nwrite-file $status_file DONE" >> "$exp_data_dir/$local_master_command_file"

# ----------------------------------------------------------------------
# 1) Criar diretórios no master remoto
# ----------------------------------------------------------------------
ssh $ssh_options $master_ip "
  mkdir -p $remote_code_dir &&
  mkdir -p $remote_config_dir &&
  mkdir -p $remote_exp_dir/raw-results &&
  mkdir -p $remote_gopath/bin
" || exit 1

# ----------------------------------------------------------------------
# 2) Enviar árvore de código, configs, scripts, queries
# ----------------------------------------------------------------------
# Código (fonte)
rsync --progress -rptz -e "ssh $ssh_options" $local_code_files "$master_ip:$remote_code_dir" || exit 2

# Configs geradas
rsync --progress -rptz -e "ssh $ssh_options" "$exp_data_dir/config/" "$master_ip:$remote_config_dir" || exit 3

# Scripts de análise e queries
rsync --progress -rptz -e "ssh $ssh_options" queries scripts "$master_ip:$remote_work_dir" || exit 4

# arquivo de comandos do master
scp $ssh_options "$exp_data_dir/$local_master_command_file" "$master_ip:$remote_master_command_file" || exit 5

# ----------------------------------------------------------------------
# 3) Gerar certificados TLS no master (usa generate.sh remoto)
# ----------------------------------------------------------------------
ssh $ssh_options $master_ip "
  set -e
  cd $remote_tls_directory
  ./generate.sh $master_ip
" || exit 6

# ----------------------------------------------------------------------
# 4) Compilar tudo LOCALMENTE (no node-0) e enviar binários para o master
# ----------------------------------------------------------------------
echo 'Compiling ISS localmente (Go modules ativados)...'

(
  cd .. || exit 1

  # Ajusta ambiente local para compilar o mirbft
  export GOPATH=\$HOME/go
  export GO111MODULE=on

  # Garante que dependências estão ok e baixa libs externas (zerolog, grpc, kyber, etc.)
  go mod tidy

  # Compila todos os binários em cmd/* (discoverymaster, discoveryslave, orderingpeer, orderingclient, etc.)
  go install ./cmd/...
) || exit 7

echo 'Enviando binários compilados para o master remoto...'

# Copia o conteúdo de $HOME/go/bin para o GOPATH remoto
rsync --progress -rptz -e "ssh $ssh_options" "$HOME/go/bin/" "$master_ip:$remote_gopath/bin/" || exit 8

# ----------------------------------------------------------------------
# 5) Iniciar master no nó remoto
# ----------------------------------------------------------------------
echo "Starting result processor and master server."
ssh $ssh_options $master_ip "
  ulimit -Sn $open_files_limit &&

  # Inicia análise contínua em background
  $remote_work_dir/scripts/analyze/analyze-continuously.sh \
    $remote_exp_dir \
    $remote_status_file \
    $remote_work_dir/scripts \
    $remote_work_dir/queries \
    $remote_gopath/bin/orderingpeer \
    $remote_gopath/bin/orderingclient \
    $remote_analysis_processes \
    > $remote_exp_dir/continuous-analysis.log 2>&1 &

  # Coloca binários no PATH
  export PATH=\$PATH:$remote_gopath/bin:$remote_work_dir/bin

  # Sobe o discoverymaster (coordenador do experimento)
  discoverymaster $master_port file $remote_master_command_file \
    > $remote_master_log 2>&1 < /dev/null
"

