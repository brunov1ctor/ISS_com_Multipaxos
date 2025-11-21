#!/bin/bash

source scripts/global-vars.sh

# Kill all children of this script when exiting
trap "$trap_exit_command" EXIT

tag=$1
master_ip=$2
public_ip=$3
private_ip=$4

# Comando de inicialização do slave (copiar TLS, bins e script auxiliar)
init_command="
  export PATH=\$PATH:$remote_gopath/bin:$remote_work_dir/bin &&

  cd $remote_work_dir &&

  # Copia tls-data do master (sem rodar generate.sh nos slaves)
  rsync --progress -rptz -e \"ssh $ssh_options\" $master_ip:$remote_tls_directory . &&

  # Copia os binários compilados do master para o GOPATH remoto
  rsync --progress -rptz -e \"ssh $ssh_options\" $master_ip:$remote_gopath/bin/* $remote_gopath/bin/ &&

  # Garante diretório de config e script oldmir-start
  mkdir -p config &&
  rsync --progress -rptz -e \"ssh $ssh_options\" $master_ip:$remote_code_dir/oldmir/oldmir-start.sh $remote_work_dir/bin &&
  chmod u+x $remote_work_dir/bin/oldmir-start.sh
"

# Comando que de fato inicia o discoveryslave
slave_command="
  ulimit -Sn $open_files_limit &&
  export PATH=\$PATH:$remote_gopath/bin:$remote_work_dir/bin &&
  discoveryslave $tag $master_ip:$master_port $public_ip $private_ip
"

echo "Setting up slave: $public_ip ($private_ip)"

# Periodicamente checa status do slave até ficar RUNNING
slave_status=$(scripts/remote-machine-status.sh $public_ip)
echo "Slave status ($public_ip): $slave_status"
while ! [[ "$slave_status" = "RUNNING" ]]; do
  sleep $machine_status_poll_period
  slave_status=$(scripts/remote-machine-status.sh $public_ip)
  echo "Slave status ($public_ip): $slave_status"
done

# Espera o master ficar pronto (criar master-ready etc)
echo "Waiting for master server."
while ! ssh $ssh_options -q -o "ConnectTimeout=10" "$master_ip" "cat $remote_ready_file > /dev/null"; do
  sleep $machine_status_poll_period
  echo "Master not ready. Retrying in $machine_status_poll_period seconds."
done

# Inicializa o slave (cópia de TLS, bins e script)
# Retry porque às vezes o ssh falha por "connection reset by peer" etc
while ! ssh $ssh_options $public_ip "$init_command"; do
  sleep 1
  echo "Retrying to initialize slave."
done

echo "Master ready. Starting slave process."
ssh $ssh_options $public_ip "$slave_command"

