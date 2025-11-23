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
master_command_file="master-commands.cmd"

python3 scripts/generate-master-commands.py > "$master_command_file" || exit 3

echo ""
echo "Master command script written to $master_command_file."
echo ""

###############################################################################
# Copiar master-commands e config para o master
###############################################################################

echo "Copying master commands and configs."

./scripts/stubborn-scp.sh 10 \
  "$master_command_file" \
  "$master_ip:$remote_master_command_file" || exit 4

./scripts/stubborn-scp.sh 10 \
  "$exp_data_dir/config-0000.yml" \
  "$master_ip:$remote_master_config_file" || exit 4

echo "Done."

###############################################################################
# Copiar arquivos de configuração para slaves
###############################################################################

echo "Copying configs to slaves."

# Gera um arquivo com:
#   <IP> <config-local> <config-remoto>
tmp_config_scp_cmds=$(mktemp)

for ((i=0; i<exp_num_peers; i++)); do
  ip_var_name="peer${i}_public_ip"
  ip="${!ip_var_name}"

  # config-000X.yml (e -faulty, se existir)
  local_cfg="$exp_data_dir/config-$(printf '%04d' "$i").yml"
  remote_cfg="$remote_config_dir/config-$(printf '%04d' "$i").yml"

  echo "$ip $local_cfg $remote_cfg" >> "$tmp_config_scp_cmds"

  local_cfg_faulty="$exp_data_dir/config-$(printf '%04d' "$i")-faulty.yml"
  if [ -f "$local_cfg_faulty" ]; then
    remote_cfg_faulty="$remote_config_dir/config-$(printf '%04d' "$i")-faulty.yml"
    echo "$ip $local_cfg_faulty $remote_cfg_faulty" >> "$tmp_config_scp_cmds"
  fi
done

# Executa cópias em paralelo
parallel -a "$tmp_config_scp_cmds" --colsep ' ' -j "$max_parallel_scp" \
  ./scripts/stubborn-scp.sh 10 {2} {1}:{3} || exit 5

rm -f "$tmp_config_scp_cmds"

echo "Configs copied."

###############################################################################
# Preparar TLS e compilar ISS no master
# (sem run-protoc.sh, usando os .pb.go já no repo)
###############################################################################

ssh $ssh_options "$master_ip" "
  set -e

  cd \"$remote_tls_directory\" &&
  ./generate.sh \"$master_ip\" &&

  cd \"$remote_work_dir\" &&
  cp -r \"$remote_tls_directory\" . &&

  echo 'Compiling ISS (sem protoc).' &&
  # Garante que o binário go seja encontrado mesmo em sessão ssh não interativa
  export PATH=\"/usr/local/go/bin:\$PATH:$remote_gopath/bin:$remote_work_dir/bin\" &&
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

  \"$remote_gopath/bin/discoverymaster\" $master_port file \"$remote_master_command_file\" \
    > \"$remote_master_log\" 2>&1 < /dev/null
"

