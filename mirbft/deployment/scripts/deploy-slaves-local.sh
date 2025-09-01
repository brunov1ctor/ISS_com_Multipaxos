#!/bin/bash

export GOPATH=/root/go
export GOROOT=/usr/local/go
export PATH="$GOPATH/bin:$GOROOT/bin:$PATH"
echo "SLAVE PATH: $PATH"

source scripts/global-vars.sh

# Kill all children of this script when exiting
trap "$trap_exit_command" EXIT

# Get root directory of the deployment data
exp_data_dir=$1
shift

# helper: lê status do master de forma segura
read_master_status() {
  local f="$exp_data_dir/$local_master_status_file"
  if [[ -f "$f" ]]; then
    cat "$f"
  else
    echo ""
  fi
}

# For each tuple given on the command line
while [ -n "$1" ]; do

  # Read arguments
  trigger=$1
  n=$2
  tag=$3
  machine_template=$4
  shift 4

  echo "Deploy params: $trigger, $n, $tag, $machine_template"

  # Sanitiza n (número de instâncias)
  if [[ ! "$n" =~ ^[0-9]+$ ]]; then
    echo "WARN: valor de n inválido ('$n'), assumindo 0"
    n=0
  fi

  # Espera por trigger apenas se for número >= 0
  if [[ "$trigger" =~ ^-?[0-9]+$ ]]; then
    if (( trigger >= 0 )); then
      master_status="$(read_master_status)"
      # enquanto master_status não for número, ou master_status < trigger, espera
      while { [[ ! "$master_status" =~ ^[0-9]+$ ]] || (( 10#$master_status < trigger )); }; do
        sleep "$machine_status_poll_period"
        master_status="$(read_master_status)"
      done
    else
      # trigger negativo => iniciar imediatamente (sem espera)
      :
    fi
  else
    echo "WARN: trigger inválido ('$trigger'), iniciando sem espera"
  fi

  # Deploy slave nodes.
  echo "Changing directory to $exp_data_dir"
  initial_directory=$(pwd)
  cd "$exp_data_dir" || exit 1

  echo "Starting local slaves: $n $tag"
  for i in $(seq 1 "$n"); do
    echo discoveryslave "$tag" "$local_public_ip:$master_port" "$local_public_ip" "$local_private_ip"
    discoveryslave "$tag" "$local_public_ip:$master_port" "$local_public_ip" "$local_private_ip" > "slave-$i.log" 2>&1 &
  done

  echo "Changing directory back to $initial_directory"
  cd "$initial_directory" || exit 1

done
wait

echo "Local slave deployment finished."

