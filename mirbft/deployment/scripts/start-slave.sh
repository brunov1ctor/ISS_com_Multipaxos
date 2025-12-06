#!/bin/bash

# scripts/start-slave.sh
#
# Inicializa um slave remoto (Emulab) e dispara o discoveryslave.
# É chamado localmente pelo start-remote-slaves.sh.

set -euo pipefail

source scripts/global-vars.sh

# Mata filhos ao sair
trap "$trap_exit_command" EXIT

tag=$1        # tag do slave (peers, 1client, etc.)
master_ip=$2  # IP de controle do master
public_ip=$3  # IP de controle deste slave
private_ip=$4 # IP de dados deste slave (10.10.1.X)

###############################################################################
# Comando de inicialização no slave
###############################################################################

init_command="
  set -e

  echo \"[slave-init] Iniciando init_command em $public_ip (tag=$tag)\"

  # GOPATH/GOBIN/PATH corretos para Go e scripts do ISS
  export GOPATH=\"$remote_gopath\" &&
  export GOBIN=\"\$GOPATH/bin\" &&
  export PATH=\"\$GOBIN:/usr/local/go/bin:$remote_work_dir/bin:$remote_work_dir/scripts:$remote_work_dir/deployment/scripts:\$PATH\" &&

  echo \"[slave-init] GOPATH=\$GOPATH\"
  echo \"[slave-init] GOBIN=\$GOBIN\"
  echo \"[slave-init] PATH=\$PATH\"

  # Garante árvore básica de diretórios
  mkdir -p \
    \"$remote_work_dir\" \
    \"$remote_work_dir/bin\" \
    \"$remote_work_dir/scripts\" \
    \"$remote_work_dir/config\" \
    \"$remote_work_dir/experiment-config\" \
    \"$remote_work_dir/experiment-output\" &&

  cd \"$remote_work_dir\" &&

  # Si

