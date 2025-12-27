#!/bin/bash

#############################
# Diretórios / arquivos base
#############################

# Diretório raiz onde ficam os dados de deployment.
#
# IMPORTANTE:
#   Muitos scripts são executados/sourced a partir de diretórios diferentes.
#   Se este caminho ficar relativo ao CWD, ele pode virar vazio/errado e gerar
#   exp_data_dir como "/remote-0000" (raiz do filesystem), causando Permission denied.
#
# Portanto, sempre resolvemos o root do deployment baseado na localização deste arquivo.

_gv_this_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
_gv_deployment_dir="$(cd "${_gv_this_dir}/.." && pwd)"

# Mantém compatibilidade: se o usuário/export já definiu deployment_data_root, respeita.
deployment_data_root="${deployment_data_root:-${_gv_deployment_dir}/deployment-data}"

# Normaliza: se alguém setou vazio, volta para o default seguro.
if [[ -z "${deployment_data_root}" ]]; then
  deployment_data_root="${_gv_deployment_dir}/deployment-data"
fi

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

# Diretório de trabalho remoto.
#
# IMPORTANTE (quota em /users):
#   NÃO usamos mais /users/<user>/iss como default, porque isso acaba gerando
#   artefatos temporários/grandes (ex.: experiment-output/, scp-output-*.log,
#   discoverymaster.pid) em um filesystem com quota apertada.
#
# Default novo: /tmp/iss-<user>
remote_work_dir="${remote_work_dir:-/tmp/iss-${remote_user}}"

# Diretório "leve" (configs/TLS/scripts) mantido em /users/<user>/iss.
# Isso evita que templates/commands que referenciam /users/<user>/iss quebrem
# quando remote_work_dir aponta para /tmp.
remote_base_dir="${remote_base_dir:-/users/${remote_user}/iss}"

# Arquivos de status/log remotos.
remote_status_file="$remote_work_dir/status"
remote_ready_file="$remote_work_dir/master-ready"
remote_main_log="$remote_work_dir/main_log.log"
remote_master_log="$remote_work_dir/master-log.log"
remote_slave_log="$remote_work_dir/slave-log.log"

# Diretório dos dados de experimento nos nós remotos.
# Layout canônico: tudo em um único root (${remote_work_dir}).
# (Evita duplicação de tls-data/ e raw-results/ e diretórios vazios.)
remote_exp_dir="$remote_work_dir"

# Diretórios leves em /users (configs/TLS)
remote_config_dir="${remote_base_dir}/experiment-config"
remote_cfg_dir="${remote_base_dir}/config"
remote_tls_dir="${remote_base_dir}/tls-data"

#########################################
# GOPATH e binários remotos (Emulab)
#########################################

# GOPATH remoto (onde você já instalou os binários do mirbft).
remote_gopath="${remote_gopath:-/users/${remote_user}/go}"
remote_bin_dir="${remote_bin_dir:-$remote_gopath/bin}"

# Código remoto (se você quiser clonar o repositório também nos slaves).
remote_code_dir="$remote_work_dir/mirbft"

# Mantém compatibilidade com scripts antigos que esperavam um "remote_tls_directory".
# Agora o TLS canônico fica em ${remote_tls_dir}.
remote_tls_directory="$remote_tls_dir"

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
# Usa a raiz do repositório (deployment/..).
local_code_dir="$(cd "${_gv_deployment_dir}/.." && pwd)"

# Arquivos locais de log/status do deploy.
local_main_log=main_log.log
local_slave_log=slave-log.log

#########################################
# Deploy: remotos a serem apagados
#########################################

# Arquivos/diretórios que serão removidos nos nós remotos ao iniciar um novo experimento.
remote_delete_files="$remote_work_dir/experiment-output \
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
# - se existir chave, usa -i; senão, deixa o ssh usar o que o ambiente tiver.
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

