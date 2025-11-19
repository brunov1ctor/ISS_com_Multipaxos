#!/bin/bash

source scripts/global-vars.sh

# Kill all children of this script when exiting
trap "$trap_exit_command" EXIT

exp_data_dir=$1
master_ip=$2

###############################################################################
# 1) Gerar arquivo de comandos do master
###############################################################################

# Essas variáveis são usadas no master-commands-template.cmd
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
# 2) Criar diretórios remotos no master
###############################################################################

ssh $ssh_options "$master_ip" "
  mkdir -p $remote_code_dir &&
  mkdir -p $remote_config_dir &&
  mkdir -p $remote_exp_dir/raw-results &&
  mkdir -p $remote_gopath/bin &&
  mkdir -p $remote_work_dir/scripts &&
  mkdir -p $remote_work_dir/queries
" || exit 1

###############################################################################
# 3) Compilar ISS LOCALMENTE (no node-0) usando Go em \$HOME/go-root
###############################################################################

echo 'Compiling ISS localmente (Go modules ativados)...'

# Ajusta ambiente de Go LOCAL
export GOROOT=\"\$HOME/go-root\"
export GOPATH=\"\$HOME/go\"
export GOBIN=\"\$GOPATH/bin\"
export PATH=\"\$GOROOT/bin:\$PATH\"

(
  # Ir para a raiz do repositório (.. a partir de deployment/)
  cd \"$local_code_dir\" || exit 1

  # Garante uso de módulos
  export GO111MODULE=on

  # Baixa/atualiza dependências
  go mod tidy

  # Instala todos os binários (discoverymaster, discoveryslave, orderingpeer, etc.)
  go install ./cmd/...
) || { echo 'Erro ao compilar ISS localmente'; exit 2; }

###############################################################################
# 4) Enviar código-fonte + binários já compilados para o master
###############################################################################

# Código-fonte para $remote_code_dir
rsync --progress -rptz -e "ssh $ssh_options" \
  $local_code_files "$master_ip:$remote_code_dir" || exit 3

# Binários compilados (ficam em $GOBIN) para $remote_gopath/bin no master
rsync --progress -rptz -e "ssh $ssh_options" \
  "$GOBIN/" "$master_ip:$remote_gopath/bin" || exit 4

# Configuração experimental (configs geradas pelo generate-*-config.sh)
rsync --progress -rptz -e "ssh $ssh_options" \
  "$exp_data_dir/config/" "$master_ip:$remote_config_dir" || exit 5

# Scripts de análise e queries SQL
rsync --progress -rptz -e "ssh $ssh_options" \
  queries scripts "$master_ip:$remote_work_dir" || exit 6

# Arquivo de comandos do master
scp $ssh_options \
  "$exp_data_dir/$local_master_command_file" \
  "$master_ip:$remote_master_command_file" || exit 7

###############################################################################
# 5) Gerar certificados TLS NO MASTER (só usa openssl, não usa go)
###############################################################################

ssh $ssh_options "$master_ip" "
  cd $remote_tls_directory &&
  ./generate.sh $master_ip &&
  cd $remote_work_dir &&
  cp -r $remote_tls_directory .
" || exit 8

###############################################################################
# 6) Iniciar análise contínua + discoverymaster NO MASTER (usando binários)
###############################################################################

echo 'Starting result processor and master server.'
ssh $ssh_options "$master_ip" "
  ulimit -Sn $open_files_limit &&

  # análise contínua (usa orderingpeer e orderingclient em $remote_gopath/bin)
  $remote_work_dir/scripts/analyze/analyze-continuously.sh \
    $remote_exp_dir \
    $remote_status_file \
    $remote_work_dir/scripts \
    $remote_work_dir/queries \
    $remote_gopath/bin/orderingpeer \
    $remote_gopath/bin/orderingclient \
    $remote_analysis_processes \
    > $remote_exp_dir/continuous-analysis.log 2>&1 &

  # iniciar discoverymaster
  export PATH=\$PATH:$remote_gopath/bin:$remote_work_dir/bin &&
  discoverymaster $master_port file $remote_master_command_file \
    > $remote_master_log 2>&1 < /dev/null
" || exit 9

