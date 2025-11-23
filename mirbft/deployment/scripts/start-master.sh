#!/bin/bash

source scripts/global-vars.sh

# Kill all children of this script when exiting
trap "$trap_exit_command" EXIT

exp_data_dir=$1
master_ip=$2

###############################################################################
# Gerar master-commands.cmd (já foi gerado pelo initialize-deployment.sh)
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
# Copiar master-commands e config para o master
###############################################################################

echo "Copying master commands and configs."

# master-commands.cmd
./scripts/stubborn-scp.sh 10 \
  "$master_command_file" \
  "$master_ip:$remote_master_command_file" || exit 4

# ATENÇÃO: o config-0000.yml fica dentro do subdiretório 'config/'
# (deployment-data/remote-XXXX/config/config-0000.yml)
master_config_local="$exp_data_dir/config/config-0000.yml"

if [ ! -f "$master_config_local" ]; then
  echo "ERRO: Arquivo de configuração do master não encontrado: $master_config_local" >&2
  exit 4
fi

./scripts/stubborn-scp.sh 10 \
  "$master_config_local" \
  "$master_ip:$remote_master_config_file" || exit 4

echo "Done."

###############################################################################
# Copiar arquivos de configuração para slaves
###############################################################################

echo "Copying configs to slaves."

# Gera um arquivo com:
#   <IP> <config-local> <config-remoto>
tmp_config_scp_cmds=$(mktemp)

# Para cada linha do deployment.dpl, montamos qual config enviar para qual IP
# Formato esperado do deployment.dpl:
#   expID slaveID slaveIP slaveRole configID ...
while read line; do
  # Ignorar linhas vazias/comentários
  if [ -z "$line" ] || [[ "$line" =~ ^# ]]; then
    continue
  fi

  exp_id=$(echo "$line"    | awk '{print $1}')
  slave_id=$(echo "$line"  | awk '{print $2}')
  slave_ip=$(echo "$line"  | awk '{print $3}')
  slave_role=$(echo "$line"| awk '{print $4}')
  config_id=$(echo "$line" | awk '{print $5}')

  # Caminho local do config gerado para esse experimento
  # IMPORTANTE: também aqui ele fica dentro de 'config/'
  local_config_file="$exp_data_dir/config/config-$(printf "%04d" "$exp_id").yml"

  # Caminho remoto do config no slave
  # remote_slave_config_dir vem de global-vars.sh (ex.: /users/Bruno/iss/config)
  remote_config_file="$remote_slave_config_dir/config-$config_id.yml"

  echo "$slave_ip $local_config_file $remote_config_file" >> "$tmp_config_scp_cmds"
done < "$exp_data_dir/$dpl_filename"

# Agora faz o SCP de todos os configs necessários
while read slave_ip local_cfg remote_cfg; do
  if [ ! -f "$local_cfg" ]; then
    echo "ERRO: Config local não encontrado: $local_cfg (slave $slave_ip)" >&2
    exit 5
  fi

  ./scripts/stubborn-scp.sh 10 \
    "$local_cfg" \
    "$slave_ip:$remote_cfg" || exit 5
done < "$tmp_config_scp_cmds"

rm -f "$tmp_config_scp_cmds"

echo "Configs copied."

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

