#!/bin/bash
#
# usage: deploy-slaves-remote.sh exp_data_dir instance_info_file master_ip \
#          [trigger0 n0 label0 [trigger1 n1 label1 [trigger2 n2 label2 [...]]]]
#
# Deploys slave nodes based on master status.
# Uses slave machine addresses provided in instance_info_file instead of starting new ones.
# For each tuple (trigger, n, label) given as arguments on the command line, deploys n slave nodes with the given label
# when the master status is trigger or (numerically) higher.
# The input tuples are processed in the given order, so a later tuple is only applied when all previous tuples have
# received their triggers.
# The master must be up and running when this script starts.

set -euo pipefail

source scripts/global-vars.sh

# Kill all children of this script when exiting
trap "$trap_exit_command" EXIT

exp_data_dir=$1
instance_info_file=$2
master_ip=$3
shift 3

# After deploying slaves, their numbers and tags will be appended here.
# Each new invocation of deploy-slaves-remote.sh will receive this list as the last arguments and skip correspondingly
# many entries in the input.
skip="skip 0 master" # needs to be initialized with a dummy value for the parameter count to be right

# For each tuple given on the command line
while [ -n "${1-}" ]; do

  # Read arguments
  trigger=${1-}
  n=${2-}
  tag=${3-}
  # shifting by one more, because the machine template file (also present in the deploy schedule) is ignored.
  shift 4 || true

  # --- PATCH: garantir que trigger é numérico ---
  # Se trigger não for um número (ex: vazio, com '#', etc.), força 0
  if ! [[ "$trigger" =~ ^[0-9]+$ ]]; then
    echo "WARNING: non-numeric trigger '$trigger', forcing 0" >&2
    trigger=0
  fi
  # --- FIM DO PATCH ---

  echo "Waiting for master to reach status >= $trigger (tag=$tag, n=$n)..."

  # Wait for trigger
  master_status=$(scripts/remote-machine-status.sh "$master_ip")

  while [[ $((10#$trigger)) -ge 0 ]] && \
        { [[ ! "$master_status" =~ ^[0-9]+$ ]] || \
          [[ $((10#$master_status)) -lt $((10#$trigger)) ]]; }; do
    # Note the $((10#$trigger)) operand. This tells bash to interpret $trigger as a decimal number.
    # Otherwise, if $trigger starts with '0' (which it sometimes does), $trigger is treated as an octal number.
    sleep "$machine_status_poll_period"
    master_status=$(scripts/remote-machine-status.sh "$master_ip")
  done

  # Deploy slave nodes.
  echo "Deploying slaves: $n $tag"
  scripts/start-remote-slaves.sh \
    "$exp_data_dir" "$tag" "$n" "$master_ip" \
    $skip $(cat "$instance_info_file") &

  skip="$skip skip $n $tag"
done

echo "All slaves started. waiting for them to finish."
wait
echo "Remote slave deployment finished."

