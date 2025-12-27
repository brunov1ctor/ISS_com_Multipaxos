#!/usr/bin/env bash
set -euo pipefail

###############################################################################
# Local deployment layout
###############################################################################
deployment_data_root=deployment-data

dpl_filename=deployment.dpl
csv_filename=deployment.csv
result_summary_file=result-summary.csv
exp_id_digits=4
analysis_query_params="-q queries/ethereum.sql -q queries/aggregates.sql -q queries/histograms.sql"

###############################################################################
# SSH options (Emulab / cluster com usuário Bruno)
###############################################################################
# No seu fluxo atual você já usa chaves/ssh normal do usuário Bruno.
# Se quiser forçar uma chave específica, defina private_key_file e adicione "-i".
private_key_file="${private_key_file:-}"

# Opções default (compatível com o que você mostrou nos logs: StrictHostKeyChecking=no etc.)
if [[ -n "${private_key_file}" ]]; then
  ssh_options="-i ${private_key_file} -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ServerAliveInterval=60"
else
  ssh_options="-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ServerAliveInterval=60"
fi

# Command to kill children of exiting scripts
trap_exit_command='{ jobs; if [ -n "$(jobs -p)" ]; then kill $(jobs -p); fi; sleep 0.5; } > /dev/null 2>&1'

###############################################################################
# Master
###############################################################################
master_port=9999
machine_status_poll_period=5

open_files_limit=16384

###############################################################################
# Local naming (templates/outputs)
###############################################################################
instance_info_file_name=instance-info
default_instance_info=last-instance-info

local_public_ip=127.0.0.1
local_private_ip=127.0.0.1
local_master_command_template_file=master-commands-template.cmd
local_master_command_file=master-commands.cmd
local_master_log=master-log.log
local_master_status_file=master-status
local_master_ready_file=master-ready
local_result_fetching_log=result-fetching.log

###############################################################################
# Remote layout (MUDANÇA CRÍTICA)
#
# Objetivo:
# - heavy/work/status/logs/output em /tmp/iss-<user>
# - configs/TLS em /users/<user>/iss
#
# Isso precisa bater com o master-commands.cmd que você mostrou:
#   /users/Bruno/iss/experiment-config/config-0000.yml
###############################################################################
remote_user="${remote_user:-Bruno}"

# (pesado)
remote_work_dir="${remote_work_dir:-/tmp/iss-${remote_user}}"
remote_instance_tag_file="$remote_work_dir/instance-tag"
remote_status_file="$remote_work_dir/status"
remote_ready_file="$remote_work_dir/master-ready"
remote_master_command_file="$remote_work_dir/master-commands.cmd"
remote_main_log="$remote_work_dir/main_log.log"
remote_master_log="$remote_work_dir/master-log.log"
remote_slave_log="$remote_work_dir/slave-log.log"
remote_instance_detail_file="$remote_work_dir/instance-detail.json"
remote_user_script_body="$remote_work_dir/user-script-body.sh"
remote_user_script_uploaded="$remote_work_dir/user-script-uploaded"

# (estável / leve)
remote_base_dir="${remote_base_dir:-/users/${remote_user}/iss}"

# Onde o deploy publica configs do experimento no MASTER:
remote_config_dir="${remote_config_dir:-$remote_base_dir/experiment-config}"

# Onde cada nó guarda o config ativo (pull do master):
remote_runtime_config_dir="${remote_runtime_config_dir:-$remote_base_dir/config}"

# TLS sincronizado para os nós:
remote_tls_directory="${remote_tls_directory:-$remote_base_dir/tls-data}"

# binários no cluster (onde seu log disse que já existe):
remote_bin_dir="${remote_bin_dir:-/users/${remote_user}/go/bin}"

# output pesado no cluster
remote_experiment_output_dir="${remote_experiment_output_dir:-$remote_work_dir/experiment-output}"

# Em vários scripts antigos, remote_exp_dir era "deployment-data no remoto".
# No seu fluxo atual, você está usando /tmp/iss-Bruno como base de trabalho.
remote_exp_dir="${remote_exp_dir:-$remote_work_dir}"

remote_analysis_processes=8

###############################################################################
# Go code directory (não obrigatório no Emulab, mas mantido p/ compatibilidade)
###############################################################################
# Se você não sincroniza código (só binários), isso não é usado.
remote_gopath="${remote_gopath:-/users/${remote_user}/go}"
remote_code_dir="${remote_code_dir:-$remote_gopath/src/github.com/hyperledger-labs/mirbft}"

remote_log_archives="experiment-output-*.tar.gz"
downloaded_code_dir=github.com/hyperledger-labs/mirbft/
downloaded_gopath="remote-gopath"

# remote_delete_files must be on one line
remote_delete_files="$remote_work_dir/experiment-output-*.tar.gz $remote_work_dir/experiment-output $remote_master_log $remote_slave_log $remote_status_file $remote_ready_file $remote_instance_tag_file $remote_master_command_file $remote_code_dir $remote_config_dir $remote_exp_dir"

###############################################################################
# OLDMIR (mantido como estava; geralmente não usado no seu fluxo)
###############################################################################
oldmir_git_repository=git@github.ibm.com:fabric-security-research/sbft.git
oldmir_git_branch=mir
oldmir_git_directory=sbft

server_bootstrap_script=server-bootstrap-script.sh
user_script_template_slave=user-script-slave.sh.template
user_script_template_master=user-script-master.sh.template

###############################################################################
# Local code dir list (mantido)
###############################################################################
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

