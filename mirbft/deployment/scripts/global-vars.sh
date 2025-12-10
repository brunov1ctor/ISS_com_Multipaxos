#!/bin/bash

#############################
# Diretórios / arquivos base
#############################

# Diretório raiz onde ficam os dados de deployment.
deployment_data_root=deployment-data

# Nomes dos arquivos gerados pelo generate-config.sh
dpl_filename=deployment.dpl
csv_filename=deployment.csv
result_summary_file=result-summary.csv
exp_id_digits=4

# Queries usadas no analyze
analysis_query_params="-q queries/ethereum.sql -q queries/aggregates.sql -q queries/histograms.sql"

#########################################
# Comando de trap para matar processos
#########################################

# Usado pelo deploy.sh para matar filhos ao sair.
trap_exit_command='{ jobs; if [ -n "$(jobs -p)" ]; then kill $(jobs -p); fi; }'

############################################
# Arquivos de instance-info / cloud legacy
############################################

# Nome padrão de arquivo de instance-info para scripts de CLOUD.
instance_info_file_name="cloud-instance-info"
default_instance_info=last-cloud-instance-info

##########################
# Usuário e caminhos remotos (Emulab)
##########################

# Usuário remoto nos nós do Emulab (padrão = usuário atual do shell).
remote_user="${remote_user:-$USER}"

# Diretório de trabalho remoto (por padrão /users/<remote_user>/iss no Emulab).
remote_work_dir="${remote_work_dir:-/users/${remote_user}/iss}"

# Arquivos de status/log remotos.
remote_status_file="$remote_work_dir/status"
remote_ready_file="$remote_work_dir/master-ready"
remote_main_log="$remote_work_dir/main_log.log"
remote_master_log="$remote_work_dir/master-log.log"
remote_slave_log="$remote_work_dir/slave-log.log"

# Diretório dos dados de experimento nos nós remotos.
remote_exp_dir="$remote_work_dir/current-deployment-data"

# Diretório de configs remotas (onde deploy.sh copia config-000X.yml).
remote_config_dir="$remote_work_dir/experiment-config"

#########################################
# GOPATH e binários remotos (Emulab)
#########################################

# GOPATH remoto (onde você já instalou os binários do mirbft).
remote_gopath="${remote_gopath:-/users/${remote_user}/go}"
remote_bin_dir="${remote_bin_dir:-$remote_gopath/bin}"

# Código remoto (se você quiser clonar o repositório também nos slaves).
remote_code_dir="$remote_work_dir/mirbft"
remote_tls_directory="$remote_code_dir/tls-data"

# Arquivos de log compactados gerados pelos slaves.
remote_log_archives="experiment-output-*.tar.gz"

#########################################
# Portas / discovery / master
#########################################

# Porta de discovery/master.
master_port=9999

#########################################
# Arquivos locais do experimento
#########################################

# Diretório local onde fica o código (este repositório).
local_code_dir=$(pwd)

# Arquivos locais de log/status do deploy.
local_main_log=main_log.log
local_slave_log=slave-log.log

#########################################
# Deploy: remotos a serem apagados
#########################################

# Arquivos/diretórios que serão removidos nos nós remotos ao iniciar um novo experimento.
remote_delete_files="$remote_work_dir/experiment-output \
$remote_work_dir/current-deployment-data \
$remote_work_dir/master-ready \
$remote_work_dir/master-log.log \
$remote_work_dir/main_log.log \
$remote_work_dir/slave-log.log"

#########################################
# Caminhos usados pelo deploy (local)
#########################################

# Arquivos de configuração (gerados por generate-config.sh)
local_config_dir="$deployment_data_root/local-config"

# Arquivos de experimentos locais (úteis para debug/local-run).
local_experiment_output_root="$deployment_data_root/local-experiments"

#########################################
# Configuração de slaves em modo local
#########################################

# Endereço e porta do master em execução local.
local_master_host=127.0.0.1
local_master_port=9999

# IP privado local para experiments.
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

# Caminho padrão de chave para Emulab (se existir).
default_ssh_key="/users/${remote_user}/.ssh/id_rsa"

# Se remote_private_key_file não foi definido externamente,
# tenta usar a default, se o arquivo existir.
if [[ -z "${remote_private_key_file:-}" ]]; then
  if [[ -f "$default_ssh_key" ]]; then
    remote_private_key_file="$default_ssh_key"
  else
    remote_private_key_file=""
  fi
fi

# Opções padrão de SSH:
# - usa -i somente se tivermos uma chave definida e existente;
# - sempre desabilita StrictHostKeyChecking e não grava em known_hosts.
if [[ -z "${ssh_options:-}" ]]; then
  if [[ -n "$remote_private_key_file" && -f "$remote_private_key_file" ]]; then
    ssh_options="-i $remote_private_key_file -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null"
  else
    ssh_options="-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null"
  fi
fi

# Opções de SCP (por padrão iguais às de SSH).
scp_options="${scp_options:-$ssh_options}"

#########################################
# Configuração de análise contínua
#########################################

# Script de análise contínua (rodado no master).
continuous_analysis_script="scripts/analyze/analyze-continuously.sh"

#########################################
# Arquivos de código que o deploy local pode querer copiar
#########################################

code_files_to_copy="$local_code_dir/Makefile
$local_code_dir/deployment
$local_code_dir/orderer
$local_code_dir/protobufs
$local_code_dir/tracing
$local_code_dir/util
$local_code_dir/run-protoc.sh"

