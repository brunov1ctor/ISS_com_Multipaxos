#!/bin/bash

source scripts/global-vars.sh

# Kill all children of this script when exiting
trap "$trap_exit_command" EXIT

exp_data_dir=$1
tag=$2          # grupo-alvo (ex.: "peers", "1client")
n=$3            # quantos nós desse grupo iniciar
master_ip=$4
shift 4

# Count how many slaves need to be skipped in the input
skip=0
while [ -n "$1" ] && [ "$1" = "skip" ] && [ $n -gt 0 ]; do
  # Formato do trecho "skip":  skip <QTD> <TAG>
  # exemplo: "skip 4 peers"
  if [ "$3" = "$tag" ]; then
    s=$2
    skip=$((skip + s))
  fi
  shift 3
done

# Para cada linha de scripts/instance-info:
# node-X  <public_ip>  <private_ip>  <role>  <tag>
while [ -n "$1" ] && [ $n -gt 0 ]; do

  # Ler argumentos da linha
  instance_id=$1
  public_slave_ip=$2
  private_slave_ip=$3
  slave_role=$4      # master / slave
  slave_tag=$5       # peers / 1client / master
  shift 5

  if [ "$slave_tag" = "$tag" ] && [ $skip -gt 0 ]; then
    # Pular esse nó porque já foi contado em algum "skip <qtd> <tag>"
    skip=$((skip - 1))

  elif [ "$slave_tag" = "$tag" ]; then
    echo "Deploying slave at public IP $public_slave_ip ($instance_id) tagged $slave_tag"

    scripts/start-slave.sh "$slave_tag" "$master_ip" "$public_slave_ip" "$private_slave_ip" \
      > "$exp_data_dir/ssh-$slave_tag-$public_slave_ip.log" 2>&1 &

    # Evitar abrir conexões SSH demais ao mesmo tempo
    sleep 0.1

    # Decrementa contador de quantos desse grupo ainda faltam
    n=$((n - 1))
  fi
done

# Esperar todos os SSHs/background terminarem
wait

