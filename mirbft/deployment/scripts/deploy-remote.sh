#!/bin/bash
#
# scripts/deploy-remote.sh
#
# Chamado por deploy.sh quando o tipo de deploy é "remote".
# Supõe que scripts/global-vars.sh e scripts/initialize-deployment.sh
# já foram usados e definiram variáveis como:
#   exp_data_dir, deployment_file, ssh_options,
#   remote_work_dir, remote_exp_dir, remote_status_file,
#   remote_ready_file, remote_master_command_file,
#   remote_delete_files, remote_config_dir,
#   local_master_command_template_file, local_master_command_file,
#   local_result_fetching_log, master_port, instance_info_file (ou default).
#

set -e

source scripts/global-vars.sh

if [ $# -lt 3 ]; then
  echo "Uso: $0 <exp_data_dir> <deployment_file> <instance_info_file>"
  exit 1
fi

exp_data_dir="$1"
deployment_file="$2"
instance_info_file="$3"

echo "Using experiment data directory: $exp_data_dir"
echo "Using deployment file: $deployment_file"

# Garante diretório de dados do experimento
mkdir -p "$exp_data_dir"

# Arquivos locais gerados
local_master_command_template_file="$exp_data_dir/master-commands-template.cmd"
local_master_command_file="$exp_data_dir/master-commands.cmd"
local_result_fetching_log="$exp_data_dir/result-fetching.log"

###############################################################################
# 1) Gerar master-commands-template a partir do .dpl
###############################################################################

echo "initialize-deployment.sh: about to generate master commands:"
echo "  depl_type                     = remote"
echo "  deployment_file (.dpl)        = $deployment_file"
echo "  local_master_command_template = $local_master_command_template_file"
echo "  output template path          = $local_master_command_template_file"

python3 scripts/generate-master-commands.py \
  remote \
  "$deployment_file" \
  "$local_master_command_template_file" \
  "$exp_data_dir"

rc=$?
echo "initialize-deployment.sh: generate-master-commands.py exit code = $rc"
if [ $rc -ne 0 ]; then
  echo "ERRO: generate-master-commands.py falhou (rc=$rc)."
  exit $rc
fi

###############################################################################
# 2) Ler informações do master a partir do instance-info
###############################################################################

if [ ! -f "$instance_info_file" ]; then
  echo "ERRO: instance_info_file não encontrado: $instance_info_file"
  exit 1
fi

echo "initialize-deployment.sh: lendo master a partir de $instance_info_file"

# Espera uma linha com role=master, ex:
# host=node-0 ctrl_ip=172.20.4.4 data_ip=10.10.1.1 role=master tag=master
master_ip=""
master_data_ip=""

while read -r line; do
  # pula linhas em branco ou comentadas
  [[ -z "$line" || "$line" =~ ^# ]] && continue

  host=$(echo "$line" | sed 's/.*host=\([^ ]*\).*/\1/')
  ctrl_ip=$(echo "$line" | sed 's/.*ctrl_ip=\([^ ]*\).*/\1/')
  data_ip=$(echo "$line" | sed 's/.*data_ip=\([^ ]*\).*/\1/')
  role=$(echo "$line" | sed 's/.*role=\([^ ]*\).*/\1/')

  if [ "$role" = "master" ]; then
    master_ip="$ctrl_ip"
    master_data_ip="$data_ip"
    break
  fi
done < "$instance_info_file"

if [ -z "$master_ip" ]; then
  echo "ERRO: não foi possível encontrar a entrada role=master em $instance_info_file"
  exit 1
fi

echo "initialize-deployment.sh: master_ip   = $master_ip"
echo "initialize-deployment.sh: master_data = $master_data_ip"

###############################################################################
# 3) Resetar estado nas máquinas remotas (master + slaves)
###############################################################################

echo "Killing everything that is alive and pruning state on the remote machines (including SSH) and removing potential bandwidth limit."

# Percorre todas as linhas de instance-info e roda o reset em cada ctrl_ip
while read -r line; do
  [[ -z "$line" || "$line" =~ ^# ]] && continue

  ctrl_ip=$(echo "$line" | sed 's/.*ctrl_ip=\([^ ]*\).*/\1/')

  ssh $ssh_options "Bruno@$ctrl_ip" "kill -9 \$(ps -ef | grep 'analyze-continuously' | grep -v \$\$ | awk '{print \$2}') 2>/dev/null || true" || true

done < "$instance_info_file"

echo
echo "Killed continuous analysis scripts."
echo

# Agora, para cada máquina (incluindo master), limpa estado de experimento anterior
while read -r line; do
  [[ -z "$line" || "$line" =~ ^# ]] && continue
  ctrl_ip=$(echo "$line" | sed 's/.*ctrl_ip=\([^ ]*\).*/\1/')

  ssh $ssh_options "Bruno@$ctrl_ip" "
    # Mata binários antigos e limpa dados do experimento anterior
    killall -9 discoverymaster discoveryslave orderingpeer orderingclient scp rsync 2>/dev/null || true
    rm -rf $remote_delete_files

    # Garante diretórios de trabalho
    mkdir -p '$remote_work_dir' '$remote_exp_dir'
    mkdir -p \"\$(dirname '$remote_status_file')\"

    echo RUNNING > '$remote_status_file'

    # Remove marca antiga de master pronto, se existir
    rm -f '$remote_ready_file'

    # Mata sessões sshd antigas (notty), se existirem
    kill -9 \$(ps -ef | grep 'sshd: notty' | awk '{print \$2}') 2>/dev/null || true
  " || true

done < "$instance_info_file"

echo
echo " Reset machine state."
echo

###############################################################################
# 4) Copiar master-commands e configs para o MASTER
###############################################################################

echo "Ensuring remote directories on master ($master_ip)."
ssh $ssh_options "Bruno@$master_ip" "
  mkdir -p '$remote_work_dir'
  mkdir -p '$remote_config_dir'
  mkdir -p '$remote_exp_dir'
" || true

echo "Copying master commands and configs to master."

remote_master_command_file="$remote_work_dir/master-commands.cmd"

if [ -f "$local_master_command_file" ]; then
  echo "Using pre-generated master command script: $local_master_command_file"
else
  echo "No pre-generated master command script, copying template."
  cp "$local_master_command_template_file" "$local_master_command_file"
fi

echo "Master command script written to $local_master_command_file."

# Copia master-commands.cmd
scripts/stubborn-scp.sh \
  "$local_master_command_file" \
  "Bruno@$master_ip:$remote_master_command_file"

# Aqui também poderíamos copiar arquivos de config se necessário:
#   scp $exp_data_dir/experiment-config/*.yml Bruno@$master_ip:$remote_config_dir/

###############################################################################
# 5) Iniciar discoverymaster automaticamente no MASTER
###############################################################################
# Aqui é o ponto crucial: fazemos o master ler master-commands.cmd e
# coordenar as execuções nos slaves.

echo "Starting discoverymaster on master ($master_ip)."

ssh $ssh_options "Bruno@$master_ip" "
  cd '$remote_work_dir' || exit 1

  # Garante que o discoverymaster exista no GOPATH remoto
  if [ ! -x '$remote_gopath/bin/discoverymaster' ]; then
    echo 'ERROR: discoverymaster não encontrado em $remote_gopath/bin/discoverymaster' >&2
    exit 1
  fi

  # (Re)cria a flag de READY do master para os slaves
  rm -f '$remote_ready_file'
  echo 'READY' > '$remote_ready_file'
  ls -l '$remote_ready_file' 2>/dev/null || echo 'WARNING: READY file not found after write' >&2

  # Sobe discoverymaster em background
  nohup '$remote_gopath/bin/discoverymaster' \
    master \
    0.0.0.0:$master_port \
    'master-commands.cmd' \
    > '$remote_master_log' 2>&1 &

" || true

# Observação:
# Se você quiser que o master só seja marcado como pronto quando o
# discoverymaster terminar, uma alternativa (mais bloqueante) seria:
#
# ssh ... "(
#   cd '$remote_work_dir' || exit 1
#   '$remote_gopath/bin/discoverymaster' master 0.0.0.0:$master_port 'master-commands.cmd' \
#     > '$remote_master_log' 2>&1
#   echo READY > '$remote_ready_file'
# )" &
#
# Mas, no teu pipeline, os slaves apenas precisam saber que o master
# já subiu o discoverymaster, então marcamos READY imediatamente.

###############################################################################
# 6) Disparar slaves remotos (1client + peers) usando start-remote-slaves.sh
###############################################################################

# Extrai informações para cada tag a partir do arquivo de deployment,
# por exemplo:
#   -1 1 1client cloud-machine-templates/small-machine-fra05.cmt
#   -1 4 peers   cloud-machine-templates/small-machine-fra05.cmt

deploy_schedule=$(grep -v '^\s*#' "$deployment_file" | grep -v '^\s*$')
echo "initialize-deployment.sh: deploy_schedule (raw) = '$deploy_schedule'"

num_clients=0
num_peers=0
client_tag=1client
peer_tag=peers

while read -r line; do
  [[ -z "$line" || "$line" =~ ^# ]] && continue

  # coluna 2 = número de instâncias
  n=$(echo "$line" | awk '{print $2}')
  t=$(echo "$line" | awk '{print $3}')

  if [ "$t" = "$client_tag" ]; then
    num_clients="$n"
  elif [ "$t" = "$peer_tag" ]; then
    num_peers="$n"
  fi
done <<< "$deploy_schedule"

echo "  num_clients = $num_clients (tag=$client_tag)"
echo "  num_peers   = $num_peers (tag=$peer_tag)"

# start-remote-slaves.sh lê instance-info para descobrir ctrl_ip/data_ip
instance_args="$instance_info_file"

echo "Deploying slaves: $num_clients $client_tag"
scripts/start-remote-slaves.sh \
  "$exp_data_dir" \
  "$client_tag" \
  "$num_clients" \
  "$master_ip" \
  $instance_args

echo "Deploying slaves: $num_peers $peer_tag"
scripts/start-remote-slaves.sh \
  "$exp_data_dir" \
  "$peer_tag" \
  "$num_peers" \
  "$master_ip" \
  $instance_args

echo "Remote slave deployment finished."

