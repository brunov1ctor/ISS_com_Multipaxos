#!/bin/bash

# Root directory where all deployment data (configs, logs, results) is stored.
deployment_data_root=deployment-data

# Filenames used within each deployment directory.
dpl_filename=deployment.dpl
csv_filename=deployment.csv
result_summary_file=result-summary.csv
exp_id_digits=4

# SQL queries used by analysis scripts (if any).
analysis_query_params="-q queries/ethereum.sql -q queries/aggregates.sql -q queries/histograms.sql"

###############################################################################
# SSH CONFIGURATION
###############################################################################

# Private key file. The corresponding public key must be in ~/.ssh/authorized_keys
# de todos os nós (node-0 .. node-6).
private_key_file="$HOME/.ssh/id_ed25519_emulab"

# Options to use when communicating with the remote machines.
ssh_options="-i $private_key_file -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ServerAliveInterval=60"

# Command to kill children of exiting scripts
trap_exit_command='{ jobs; if [ -n "$(jobs -p)" ]; then kill $(jobs -p); fi; sleep 0.5; } > /dev/null 2>&1'

###############################################################################
# MASTER / POLLING CONFIG
###############################################################################

# Machine template used in original cloud setup (não usado no Emulab, mas deixamos aqui).
#master_machine=cloud-machine-templates/dedicated-machine-32-CPUs-32GB-RAM-lon02.cmt
#master_machine=cloud-machine-templates/dedicated-machine-32-CPUs-32GB-RAM-ams01.cmt
master_machine=cloud-machine-templates/dedicated-machine-32-CPUs-32GB-RAM-fra02.cmt
#master_machine=cloud-machine-templates/dedicated-machine-32-CPUs-32GB-RAM-mil01.cmt
#master_machine=cloud-machine-templates/small-machine-mil01.cmt
#master_machine=cloud-machine-templates/small-machine-fra02.cmt

# Port on which the master listens.
master_port=9999

# How often to poll the master status (in seconds).
machine_status_poll_period=5

# The maximum number of open files to be set at remote machines.
open_files_limit=16384

# Timeouts and cloud-related parameters (mantidos pra compatibilidade, mesmo sem usar cloud).
instance_ready_timeout=600
instance_creation_batch=64
instance_info_file_name=cloud-instance-info
default_instance_info=last-cloud-instance-info

###############################################################################
# LOCAL (node-0) CONFIGURATION
###############################################################################

local_public_ip=127.0.0.1
local_private_ip=127.0.0.1
local_master_command_template_file=master-commands-template.cmd
local_master_command_file=master-commands.cmd
local_master_log=master-log.log
local_master_status_file=master-status
local_master_ready_file=master-ready
local_result_fetching_log=result-fetching.log

###############################################################################
# REMOTE (NODES Emulab) CONFIGURATION
###############################################################################

# Diretório de trabalho remoto em TODOS os nós (node-0..6).
# Os scripts vão criar arquivos de status, logs, configs e código aqui.
remote_work_dir="/users/Bruno/iss"

remote_instance_tag_file="$remote_work_dir/instance-tag"
remote_status_file="$remote_work_dir/status"
remote_ready_file="$remote_work_dir/master-ready"

remote_main_log="$remote_work_dir/main_log.log"
remote_master_log="$remote_work_dir/master-log.log"
remote_slave_log="$remote_work_dir/slave-log.log"

# Chave que as instâncias usariam para se comunicar entre si (não é crítica no Emulab).
remote_private_key_file="$remote_work_dir/ibmcloud-ssh-key"

remote_instance_detail_file="$remote_work_dir/instance-detail.json"
remote_user_script_body="$remote_work_dir/user-script-body.sh"
remote_user_script_uploaded="$remote_work_dir/user-script-uploaded"
remote_master_command_file="$remote_work_dir/master-commands.cmd"
remote_exp_dir="$remote_work_dir/current-deployment-data"

# Número de processos de análise em paralelo no nó remoto (se usado).
remote_analysis_processes=8

###############################################################################
# GOPATH / CÓDIGO REMOTO
###############################################################################

# GOPATH remoto para o usuário Bruno.
remote_gopath="/users/Bruno/go"

# Diretório onde o código mirbft será colocado nos nós remotos.
remote_code_dir="$remote_gopath/src/github.com/hyperledger-labs/mirbft"

# Diretório de configuração e TLS.
remote_config_dir="$remote_work_dir/experiment-config"
remote_tls_directory="$remote_code_dir/tls-data"

remote_log_archives="experiment-output-*.tar.gz"
downloaded_code_dir="github.com/hyperledger-labs/mirbft/"
downloaded_gopath="remote-gopath"

# Quais arquivos/pastas apagar na limpeza remota.
# IMPORTANTE: agora tudo está em /users/Bruno/iss, não em /root.
# remote_delete_files deve ficar em UMA linha só.
remote_delete_files="$remote_work_dir/experiment-output-*.tar.gz $remote_work_dir/experiment-output $remote_master_log $remote_slave_log $remote_status_file $remote_ready_file $remote_instance_tag_file $remote_master_command_file $remote_code_dir $remote_config_dir $remote_exp_dir"

###############################################################################
# OLDMIR (não usado nos seus testes, mantido p/ compatibilidade)
###############################################################################

oldmir_git_repository=git@github.ibm.com:fabric-security-research/sbft.git
oldmir_git_branch=mir
oldmir_git_directory=sbft # Relative path from the Go source IBM repository dir ($GOPATH/src/github.ibm.com)

###############################################################################
# LOCAL CODE (node-0) — o que será enviado pros nós
###############################################################################

server_bootstrap_script=server-bootstrap-script.sh
user_script_template_slave=user-script-slave.sh.template
user_script_template_master=user-script-master.sh.template

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
$local_code_dir/validator"

