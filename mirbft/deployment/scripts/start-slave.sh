#!/bin/bash

source scripts/global-vars.sh

# Kill all children of this script when exiting
trap "$trap_exit_command" EXIT

tag=$1
master_ip=$2
public_ip=$3
private_ip=$4

init_command="
  export PATH=\$PATH:$remote_gopath/bin:$remote_work_dir/bin &&
  cd $remote_work_dir &&

  # Copia diretório de TLS do master e gera certificados locais
  rsync --progress -rptz -e \"ssh $ssh_options\" $master_ip:$remote_tls_directory . &&
  cd tls-data &&
  ./generate.sh $public_ip $private_ip &&

  # Volta para o diretório de trabalho
  cd $remote_work_dir &&

  # Copia os binários compilados do master para o GOPATH remoto
  rsync --progress -rptz -e \"ssh $ssh_options\" $master_ip:$remote_gopath/bin/ $remote_gopath/bin/ &&

  # Garante diretório de config
  mkdir -p config &&

  # Copia o script oldmir-start.sh do master para o bin local e torna executável
  rsync --progress -rptz -e \"ssh $ssh_options\" $master_ip:$remote_code_dir/oldmir/oldmir-start.sh $remote_work_dir/bin/ &&
  chmod u+x $remote_work_dir/bin/oldmir-start.sh
"

slave_command="
  ulimit -Sn $open_files_limit &&
  export PATH=\$PATH:$remote_gopath/bin:$remote_work_dir/bin &&
  discoveryslave $tag $master_ip:$master_port $public_ip $private_ip
"

echo "Setting up slave: $public_ip ($private_ip)"

# (Apenas log, não bloqueia mais aqui)
slave_status=$(scripts/remote-machine-status.sh $public_ip 2>/dev/null || echo "UNKNOWN")
echo "Slave status ($public_ip): $slave_status"

# Espera o master ficar pronto (criar master-ready)
echo "Waiting for master server."
while ! ssh $ssh_options -q -o "ConnectTimeout=10" "$master_ip" "cat $remote_ready_file > /dev/null"; do
  sleep $machine_status_poll_period
  echo "Master not ready. Retrying in $machine_status_poll_period seconds."
done

# Inicializa o slave (TLS, binários, script oldmir)
echo "Initializing slave: $public_ip"
while ! ssh $ssh_options $public_ip "$init_command"; do
  sleep 1
  echo "Retrying to initialize slave."
done

echo "Master ready. Starting slave process on $public_ip."
ssh $ssh_options $public_ip "$slave_command"

