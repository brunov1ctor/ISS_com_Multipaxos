deployment_data_root=deployment-data

dpl_filename=deployment.dpl
csv_filename=deployment.csv
result_summary_file=result-summary.csv
exp_id_digits=4
analysis_query_params="-q queries/ethereum.sql -q queries/aggregates.sql -q queries/histograms.sql"

# Private key, of which the corresponding public key needs to be an authorized ssh key at each instance.
# (No Emulab, essa é a chave que você criou e funciona: id_ed25519_emulab)
private_key_file="$HOME/.ssh/id_ed25519_emulab"

# Options to use when communicating with the remote machines.
# Usamos sempre o usuário Bruno e essa chave.
ssh_options="-i $private_key_file -o User=Bruno -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ServerAliveInterval=60"

# Command to kill children of exiting scripts
trap_exit_command='{ jobs; if [ -n "$(jobs -p)" ]; then kill $(jobs -p); fi; sleep 0.5; } > /dev/null 2>&1'

# Port on which the master listens.
# Os master_machine abaixo são usados só no modo "cloud", não no "remote" do Emulab.
#master_machine=cloud-machine-templates/dedicated-machine-32-CPUs-32GB-RAM-lon02.cmt
#master_machine=cloud-machine-templates/dedicated-machine-32-CPUs-32GB-RAM-ams01.cmt
master_machine=cloud-machine-templates/dedicated-machine-32-CPUs-32GB-RAM-fra02.cmt
#master_machine=cloud-machine-templates/dedicated-machine-32-CPUs-32GB-RAM-mil01.cmt
#master_machine=cloud-machine-templates/small-machine-mil01.cmt
#master_machine=cloud-machine-templates/small-machine-fra02.cmt
master_port=9999
machine_status_poll_period=5

# The maximum number of open files to be set at remote machines.
open_files_limit=16384

# Number of seconds to wait for a deployed instance to become ready.
instance_ready_timeout=600
instance_creation_batch=64
instance_info_file_name=cloud-instance-info
default_instance_info=last-cloud-instance-info

# Valores para deploy local (não usados no remote Emulab, mas deixamos)
local_public_ip=127.0.0.1
local_private_ip=127.0.0.1
local_master_command_template_file=master-commands-template.cmd
local_master_command_file=master-commands.cmd
local_master_log=master-log.log
local_master_status_file=master-status
local_master_ready_file=master-ready
local_result_fetching_log=result-fetching.log

###############################################################################
# Caminhos REMOTOS (adaptados para Emulab / usuário Bruno)
###############################################################################

# Diretório base de trabalho no nó remoto (NFS compartilhado)
remote_work_dir=/users/Bruno/iss

# Arquivos auxiliares de instância (usados no modo cloud, inofensivos aqui)
remote_instance_tag_file=$remote_work_dir/instance-tag
remote_status_file=$remote_work_dir/status
remote_ready_file=$remote_work_dir/master-ready

# Logs principais AGORA ficam no remote_work_dir (nada em /root)
remote_main_log=$remote_work_dir/main_log.log
remote_master_log=$remote_work_dir/master-log.log
remote_slave_log=$remote_work_dir/slave-log.log

# Chave privada remota (modo cloud IBM, não é usada no Emulab, mas mantida)
remote_private_key_file=$remote_work_dir/ibmcloud-ssh-key # Key used by the instances to communicate among each other.
remote_instance_detail_file=$remote_work_dir/instance-detail.json
remote_user_script_body=$remote_work_dir/user-script-body.sh
remote_user_script_uploaded=$remote_work_dir/user-script-uploaded

# Arquivo de comandos do master no nó remoto
remote_master_command_file=$remote_work_dir/master-commands.cmd

# Diretório remoto onde ficam os dados do experimento (logs/resultados)
remote_exp_dir=$remote_work_dir/current-deployment-data

# Número de processos de análise contínua
remote_analysis_processes=8

# GOPATH remoto (onde código e binários serão instalados)
remote_gopath=/users/Bruno/go

# Diretório de código remoto (repo mirbft clonado pelo deploy)
remote_code_dir="$remote_gopath/src/github.com/hyperledger-labs/mirbft"

# Diretório de configs remotas geradas
remote_config_dir=$remote_work_dir/experiment-config

# Diretório de certificados TLS
remote_tls_directory="$remote_code_dir/tls-data"

# Arquivos de logs compactados
remote_log_archives="experiment-output-*.tar.gz"

# Caminho local depois de baixar código remoto (modo cloud)
downloaded_code_dir=github.com/hyperledger-labs/mirbft/
downloaded_gopath="remote-gopath"

# remote_delete_files deve ficar em UMA linha; deixamos vazio para não tentar remover nada em /root.
remote_delete_files=""

###############################################################################
# OLDMIR (desnecessário pro MultiPaxos, mas deixado como no original)
###############################################################################

oldmir_git_repository=git@github.ibm.com:fabric-security-research/sbft.git
oldmir_git_branch=mir
oldmir_git_directory=sbft # Relative path from the Go source IBM repository dir ($GOPATH/src/github.ibm.com)

###############################################################################
# Scripts auxiliares
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
$local_code_dir/validator
$local_code_dir/run-protoc.sh"

