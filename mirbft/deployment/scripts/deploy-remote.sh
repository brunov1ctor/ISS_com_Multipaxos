#!/bin/bash
set -euo pipefail

###############################################################################
# Este script é carregado via "source" por deploy.sh para executar um
# experimento remoto. As seguintes variáveis devem estar definidas por
# scripts/initialize-deployment.sh:
#   - exp_data_dir
#   - instance_info_file
#   - deploy_schedule
#   - cancel_instances
###############################################################################

: "${exp_data_dir:?exp_data_dir not set}"
: "${instance_info_file:?instance_info_file not set}"
: "${deploy_schedule:?deploy_schedule not set}"

# Garantir que estamos no diretório de deployment (onde estão scripts/global-vars.sh)
# Se você já garante isso em deploy.sh, isso aqui é só redundância inofensiva.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$DEPLOY_DIR"

# Carregar variáveis globais (remote_work_dir, remote_exp_dir, remote_status_file, etc.)
source "$SCRIPT_DIR/global-vars.sh"

###############################################################################
# Obter IP do master
###############################################################################

master_ip=$(awk '$4 == "master" {print $2}' "$instance_info_file" || true)
if [ -z "${master_ip:-}" ]; then
  >&2 echo "deploy-remote.sh: could not obtain master ip from instance info file: $instance_info_file"
  exit 1
fi

cp "$instance_info_file" "$exp_data_dir/$instance_info_file_name"
echo "Using instance info file: $instance_info_file"
echo "       Master IP address: $master_ip"

###############################################################################
# Gerar arquivo final de comandos do master
###############################################################################

export ssh_key_file="$remote_private_key_file"
export own_public_ip="$master_ip"
export master_port
export status_file="$remote_status_file"

envsubst '$ssh_key_file $own_public_ip $master_port $status_file' \
  < "$local_master_command_template_file" \
  > "$exp_data_dir/$local_master_command_file"

echo "Using master command file: $exp_data_dir/$local_master_command_file"

###############################################################################
# Preparar diretórios remotos e copiar arquivos necessários
###############################################################################

echo "Creating remote experiment directory: $remote_exp_dir"
ssh $ssh_options "$ssh_user@$master_ip" "mkdir -p '$remote_exp_dir'" || {
  >&2 echo "deploy-remote.sh: failed to create remote experiment directory on master: $master_ip"
  exit 1
}

echo "Copying experiment data to master."
scp $scp_options -r \
  "$exp_data_dir"/* \
  "$ssh_user@$master_ip:$remote_exp_dir/" || {
  >&2 echo "deploy-remote.sh: failed to copy experiment data to master: $master_ip"
  exit 1
}

###############################################################################
# Iniciar master remoto
###############################################################################

echo "Starting remote master on $master_ip"

ssh $ssh_options "$ssh_user@$master_ip" "
  set -euo pipefail

  cd '$remote_exp_dir'

  echo \"Compiling on master node.\"
  initial_directory=\$(pwd)
  cd '$remote_work_dir/..'
  ./run-protoc.sh
  cd \"\$initial_directory\"
  go install -race ./cmd/...

  echo \"Copy TLS keys and certificates to experiment directory on master.\"
  cp -r tls-data '$remote_exp_dir'

  echo \"Copying binaries into gopath/bin inside experiment directory on master.\"
  mkdir -p '$remote_exp_dir/gopath/bin'
  cp \"\$GOPATH/bin/orderingpeer\" \
     \"\$GOPATH/bin/orderingclient\" \
     \"\$GOPATH/bin/discoverymaster\" \
     \"\$GOPATH/bin/discoveryslave\" \
     '$remote_exp_dir/gopath/bin/'

  echo \"Starting discoverymaster on master node.\"
  discoverymaster $master_port file '$local_master_command_file' '$remote_status_file' \
    > '$remote_exp_dir/master.log' 2>&1 &
" || {
  >&2 echo "deploy-remote.sh: failed to start discoverymaster on master: $master_ip"
  exit 1
}

###############################################################################
# Iniciar slaves remotos conforme o schedule
###############################################################################

echo "Starting remote slaves according to schedule: $deploy_schedule"

# Ler informações de instâncias (peers, clientes, etc.)
if ! [ -r "$exp_data_dir/$instance_info_file_name" ]; then
  >&2 echo "deploy-remote.sh: cannot read instance info file: $exp_data_dir/$instance_info_file_name"
  exit 1
fi

# O script start-remote-slaves.sh lê o arquivo de instance-info, filtra pelo tag
# e inicia os grupos conforme o deploy_schedule.
scripts/start-remote-slaves.sh "$exp_data_dir" "$deploy_schedule" \
  "$master_ip" "$exp_data_dir/$instance_info_file_name" &

###############################################################################
# Acompanhar status das máquinas remotas (opcional)
###############################################################################

if [ "${remote_status_check:-true}" = true ]; then
  echo "Checking remote machine status periodically."
  scripts/remote-machine-status.sh "$master_ip" &
fi

###############################################################################
# Buscar resultados do experimento
###############################################################################

local_result_fetching_log="result-fetching.log"

echo "Starting result fetching in the background."
(
  cd "$DEPLOY_DIR"
  scripts/analyze/untar.sh "$exp_data_dir" > "$exp_data_dir/$local_result_fetching_log" 2>&1
) &

echo "Waiting for deployment process and result fetching to finish."
echo "For progress on experiment result fetching, see $exp_data_dir/$local_result_fetching_log."
wait

###############################################################################
# Cancelar VMs na nuvem, se configurado
###############################################################################

if ${cancel_instances:-false}; then
  scripts/cancel-cloud-instances.sh "$exp_data_dir/$instance_info_file_name"
else
  echo -e "Do not forget to cancel the used virtual servers using:\n  scripts/cancel-cloud-instances.sh $exp_data_dir/$instance_info_file_name\n"
fi

