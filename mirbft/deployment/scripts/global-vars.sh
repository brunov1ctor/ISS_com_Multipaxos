#!/bin/bash

#############################
# Diretórios / arquivos base
#############################

# Diretório raiz onde ficam os dados de deployment (já está assim no seu setup).
deployment_data_root=deployment-data

# Nomes dos arquivos gerados pelo generate-config.sh
dpl_filename=deployment.dpl
csv_filename=deployment.csv
result_summary_file=result-summary.csv
exp_id_digits=4

# Queries usadas no analyze (pode deixar igual ao original ou ajustar se quiser)
analysis_query_params="-q queries/ethereum.sql -q queries/aggregates.sql -q queries/histograms.sql"

#########################################
# Comando de trap para matar processos
#########################################

# Usado pelo deploy.sh para matar filhos ao sair.
trap_exit_command='{ jobs; if [ -n "$(jobs -p)" ]; then kill $(jobs -p); fi; sleep 0.5; } > /dev/null 2>&1'

##########################
# Parametrização de rede
##########################

# Porta em que o master (discoverymaster / orderingclient) escuta.
master_port=9999

# Período de polling de status de máquina (se algum script usar).
machine_status_poll_period=5

# Limite máximo de arquivos abertos nas máquinas remotas (caso algum script ajuste).
open_files_limit=16384

############################################
# Arquivos de instance-info / cloud legacy
############################################

# Nome padrão de arquivo de instance-info para scripts de CLOUD.
# Para o modo REMOTE (Emulab), você já passa "scripts/instance-info" direto para o deploy.sh.
instance_info_file_name="cloud-instance-info"
default_instance_info=last-cloud-instance-info

##########################
# Usuário e caminhos remotos (Emulab)
##########################

# Usuário remoto nos nós do Emulab.
remote_user="${remote_user:-Bruno}"

# Diretório de trabalho remoto (onde criamos /users/Bruno/iss).
remote_work_dir="${remote_work_dir:-/users/Bruno/iss}"

# Arquivos de status/log remotos.
remote_status_file="$remote_work_dir/status"
remote_ready_file="$remote_work_dir/master-ready"
remote_main_log="$remote_work_dir/main_log.log"
remote_master_log="$remote_work_dir/master-log.log"
remote_slave_log="$remote_work_dir/slave-log.log"

# Diretório onde ficam os dados do experimento no nó remoto (copiado de node-0).
remote_exp_dir="$remote_work_dir/current-deployment-data"

# Diretório de configs remotas (onde deploy.sh copia config-000X.yml).
remote_config_dir="$remote_work_dir/experiment-config"

#########################################
# GOPATH e binários remotos (Emulab)
#########################################

# GOPATH remoto (onde você já instalou os binários do mirbft).
remote_gopath="${remote_gopath:-/users/Bruno/go}"
remote_bin_dir="${remote_bin_dir:-$remote_gopath/bin}"

# Código remoto (se você quiser clonar o repositório também nos slaves).
# Pode deixar assim ou ajustar para o caminho real do clone remoto.
remote_code_dir="$remote_work_dir/mirbft"
remote_tls_directory="$remote_code_dir/tls-data"

# Arquivos de log compactados gerados pelos slaves.
remote_log_archives="experiment-output-*.tar.gz"

# Arquivos que os scripts de limpeza remota podem apagar antes de novo experimento.
# (tudo em uma linha para caber bem no comando remoto)
remote_delete_files="$remote_work_dir/experiment-output-*.tar.gz \
$remote_work_dir/experiment-output \
$remote_master_log \
$remote_slave_log \
$remote_status_file \
$remote_ready_file \
$remote_work_dir/instance-tag \
$remote_work_dir/master-commands.cmd \
$remote_code_dir \
$remote_config_dir \
$remote_exp_dir"

#########################################
# Arquivos locais (no node-0 / deployment)
#########################################

local_public_ip=127.0.0.1
local_private_ip=127.0.0.1

# Arquivos de comandos/log/status do master, gerados localmente.
local_master_command_template_file=master-commands-template.cmd
local_master_command_file=master-commands.cmd
local_master_log=master-log.log
local_master_status_file=master-status
local_master_ready_file=master-ready
local_result_fetching_log=result-fetching.log

#########################################
# SSH / chaves
#########################################

# Chave PRIVADA opcional para acesso remoto (se quiser usar uma específica).
# Se você não configurar nada aqui, o ssh usa chave padrão / agent.
remote_private_key_file="${remote_private_key_file:-}"

# Opções de SSH usadas em todos os scripts.
ssh_options="-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ServerAliveInterval=60"

# Se uma chave explícita estiver definida e existir, adiciona -i.
if [ -n "$remote_private_key_file" ] && [ -f "$remote_private_key_file" ]; then
  ssh_options="-i $remote_private_key_file $ssh_options"
fi

#########################################
# Variáveis de CLOUD antigas (mantidas
# só para compatibilidade com scripts
# que você não está usando no Emulab)
#########################################

private_key_file=ibmcloud-ssh-key

# Máquinas cloud legacy (não usadas no modo REMOTE / Emulab)
master_machine=cloud-machine-templates/dedicated-machine-32-CPUs-32GB-RAM-fra02.cmt
instance_ready_timeout=600
instance_creation_batch=64

downloaded_code_dir=github.com/hyperledger-labs/mirbft/
downloaded_gopath="remote-gopath"

# OLDMIR (código antigo) – pode deixar quieto.
oldmir_git_repository=git@github.ibm.com:fabric-security-research/sbft.git
oldmir_git_branch=mir
oldmir_git_directory=sbft # relativo a $GOPATH/src/github.ibm.com

#########################################
# Scripts auxiliares
#########################################

server_bootstrap_script=server-bootstrap-script.sh
user_script_template_slave=user-script-slave.sh.template
user_script_template_master=user-script-master.sh.template

#########################################
# Arquivos locais de código (lista
# usada por scripts de CLOUD para copiar
# código – mantida para compatibilidade)
#########################################

local_code_dir=".."
local_code_files="
$local_code_dir/announcer
$local_code_dir/checkpoint
$local_code_dir/cmd
$local_code_dir/config
$local_code_dir/crypto
$local_code_dir/discovery
$local_code_dir/log
$local_code_dir/manager
$local_code_dir/membership
$local_code_dir/messenger
$local_code_dir/oldmir
$local_code_dir/orderer
$local_code_dir/profiling
$local_code_dir/request
$local_code_dir/protobufs
$local_code_dir/statetransfer
$local_code_dir/tls-data
$local_code_dir/tracing
$local_code_dir/util
$local_code_dir/validator
$local_code_dir/run-protoc.sh"

