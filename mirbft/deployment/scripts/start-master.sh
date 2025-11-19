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

# Garante que no fim o master grava o status como DONE
echo -e "\nwrite-file $status_file DONE" >> "$exp_data_dir/$local_master_command_file"

# ---------------------------------------------------------------------
# 1) Criar diretórios remotos de código, config e resultados
# ---------------------------------------------------------------------
ssh $ssh_options "$master_ip" "
  mkdir -p \"$remote_code_dir\" &&
  mkdir -p \"$remote_config_dir\" &&
  mkdir -p \"$remote_exp_dir/raw-results\" &&
  mkdir -p \"$remote_work_dir/.cache/go-build\"
" || exit 1

# ---------------------------------------------------------------------
# 2) Enviar código para o master
# ---------------------------------------------------------------------
rsync --progress -rptz -e "ssh $ssh_options" \
  $local_code_files \
  "$master_ip:$remote_code_dir" || exit 2

# ---------------------------------------------------------------------
# 3) Enviar configs geradas para o master
# ---------------------------------------------------------------------
rsync --progress -rptz -e "ssh $ssh_options" \
  "$exp_data_dir/config/"* \
  "$master_ip:$remote_config_dir" || exit 3

# ---------------------------------------------------------------------
# 4) Enviar scripts de análise e queries para o master
# ---------------------------------------------------------------------
rsync --progress -rptz -e "ssh $ssh_options" \
  queries scripts \
  "$master_ip:$remote_work_dir" || exit 4

# ---------------------------------------------------------------------
# 5) Enviar arquivo de comandos do master
# ---------------------------------------------------------------------
scp $ssh_options \
  "$exp_data_dir/$local_master_command_file" \
  "$master_ip:$remote_master_command_file" || exit 5

# ---------------------------------------------------------------------
# 6) Gerar certificados TLS e COMPILAR ISS no master
#    (sem run-protoc.sh, usando *.pb.go já presentes)
# ---------------------------------------------------------------------
ssh $ssh_options "$master_ip" "
  set -e

  # TLS
  cd \"$remote_tls_directory\" &&
  ./generate.sh \"$master_ip\" &&
  cd \"$remote_work_dir\" &&
  cp -r \"$remote_tls_directory\" . &&

  echo 'Compiling ISS (sem run-protoc.sh, usando *.pb.go existentes).'

  # Caminho fixo do binário do Go no master
  GO_BIN=\"/users/Bruno/go-root/bin/go\"

  if [ ! -x \"\$GO_BIN\" ]; then
    echo 'ERRO: Go não encontrado em /users/Bruno/go-root/bin/go.' >&2
    echo 'Instale o Go nesse caminho no nó master antes de rodar ./deploy.sh.' >&2
    exit 6
  fi

  # Ambiente de compilação (tudo em /users/Bruno/iss)
  export PATH=\"\$PATH:$remote_work_dir/bin\"
  export GOPATH=\"$remote_work_dir/go\"
  export GO111MODULE=auto
  export GOCACHE=\"$remote_work_dir/.cache/go-build\"

  mkdir -p \"\$GOPATH\" \"\$GOCACHE\"

  cd \"$remote_code_dir\" &&
  \"\$GO_BIN\" install ./cmd/...
" || exit 6

# ---------------------------------------------------------------------
# 7) Iniciar o master (discoverymaster) e o analisador contínuo
# ---------------------------------------------------------------------
echo "Starting result processor and master server."
ssh $ssh_options "$master_ip" "
  set -e

  # Limite de arquivos abertos
  ulimit -Sn $open_files_limit

  # Caminho para os binários compilados (orderingpeer, orderingclient, etc.)
  export PATH=\"\$PATH:$remote_work_dir/go/bin:$remote_work_dir/bin\"

  # Análise contínua em background
  \"$remote_work_dir/scripts/analyze/analyze-continuously.sh\" \
      \"$remote_exp_dir\" \
      \"$remote_status_file\" \
      \"$remote_work_dir/scripts\" \
      \"$remote_work_dir/queries\" \
      \"$remote_work_dir/go/bin/orderingpeer\" \
      \"$remote_work_dir/go/bin/orderingclient\" \
      $remote_analysis_processes \
      > \"$remote_exp_dir/continuous-analysis.log\" 2>&1 &

  # Servidor master de descoberta/controle
  discoverymaster $master_port file \"$remote_master_command_file\" \
      > \"$remote_master_log\" 2>&1 < /dev/null
"

