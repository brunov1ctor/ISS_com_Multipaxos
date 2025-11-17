#!/bin/bash

# start-master.sh exp_data_dir master_ip
#
# Gera o arquivo de comandos do master, envia código/configs para o nó master
# e inicia o discoverymaster + análise contínua.

source scripts/global-vars.sh

# Kill all children of this script when exiting
trap "$trap_exit_command" EXIT

exp_data_dir=$1
master_ip=$2

#############################
# 1. Gerar master-commands  #
#############################

# Estas variáveis são usadas pelo envsubst no template de comandos
export ssh_key_file=$remote_private_key_file
export own_public_ip=$master_ip
export master_port
export status_file=$remote_status_file
export ready_file=$remote_ready_file

# Gera o arquivo de comandos final do master a partir do template
envsubst '$ssh_key_file $own_public_ip $master_port $status_file $ready_file' \
  < "$exp_data_dir/$local_master_command_template_file" \
  > "$exp_data_dir/$local_master_command_file"

# Ao final da execução, escreve DONE no status_file
echo -e "\nwrite-file $status_file DONE" >> "$exp_data_dir/$local_master_command_file"

#############################################
# 2. Criar diretórios remotos no master     #
#############################################

ssh $ssh_options "$master_ip" "
  mkdir -p \"$remote_code_dir\" &&
  mkdir -p \"$remote_config_dir\" &&
  mkdir -p \"$remote_exp_dir/raw-results\" &&
  mkdir -p \"$remote_gopath/bin\" &&
  mkdir -p \"$remote_work_dir/.cache/go-build\"
" || exit 1

#############################################
# 3. Enviar código, configs e scripts       #
#############################################

# Upload do código da ISS/mirbft
rsync --progress -rptz -e "ssh $ssh_options" \
  $local_code_files "$master_ip:$remote_code_dir" || exit 2

# Upload das configs geradas para o master
rsync --progress -rptz -e "ssh $ssh_options" \
  "$exp_data_dir/config/"* "$master_ip:$remote_config_dir" || exit 3

# Upload de consultas e scripts de análise para o diretório de trabalho remoto
rsync --progress -rptz -e "ssh $ssh_options" \
  queries scripts "$master_ip:$remote_work_dir" || exit 4

# Upload do arquivo de comandos do master
scp $ssh_options \
  "$exp_data_dir/$local_master_command_file" \
  "$master_ip:$remote_master_command_file" || exit 5

#############################################
# 4. Gerar TLS e compilar no master         #
#############################################

ssh $ssh_options "$master_ip" "
  set -e

  # Gerar certificados TLS
  cd \"$remote_tls_directory\" &&
  ./generate.sh \"$master_ip\" &&

  # Copiar TLS para o diretório de trabalho
  cd \"$remote_work_dir\" &&
  cp -r \"$remote_tls_directory\" . &&

  echo 'Compiling ISS.' &&

  # Preparar ambiente Go
  export GOPATH=\"$remote_gopath\" &&
  export PATH=\"\$PATH:$remote_gopath/bin:$remote_work_dir/bin\" &&

  # Desabilitar módulos go para compatibilidade com versões mais novas
  export GO111MODULE=auto &&
  export GOCACHE=\"$remote_work_dir/.cache/go-build\" &&

  # Compilar protobufs e binários
  cd \"$remote_code_dir\" &&
  ./run-protoc.sh &&
  go install ./cmd/...
" || exit 6

#############################################
# 5. Iniciar master e análise contínua      #
#############################################

echo "Starting result processor and master server on $master_ip."

ssh $ssh_options "$master_ip" "
  ulimit -Sn $open_files_limit &&

  cd \"$remote_work_dir\" &&

  # Inicia análise contínua em background (se script existir)
  if [ -x \"$remote_work_dir/scripts/analyze/analyze-continuously.sh\" ]; then
    \"$remote_work_dir/scripts/analyze/analyze-continuously.sh\" \
      \"$remote_exp_dir\" \
      \"$remote_status_file\" \
      \"$remote_work_dir/scripts\" \
      \"$remote_work_dir/queries\" \
      \"$remote_gopath/bin/orderingpeer\" \
      \"$remote_gopath/bin/orderingclient\" \
      $remote_analysis_processes \
      > \"$remote_exp_dir/continuous-analysis.log\" 2>&1 &
  fi

  # Inicia discoverymaster
  export GOPATH=\"$remote_gopath\" &&
  export PATH=\"\$PATH:$remote_gopath/bin:$remote_work_dir/bin\" &&

  discoverymaster $master_port file \"$remote_master_command_file\" \
    > \"$remote_master_log\" 2>&1 < /dev/null
"

echo "Master and result processor started on $master_ip."

