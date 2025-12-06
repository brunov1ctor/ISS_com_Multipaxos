#!/bin/bash

# scripts/start-remote-slaves.sh
#
# Starts remote slaves (peers / clients) given:
#   - experiment directory
#   - tag ("peers" or "1client")
#   - expected count
#   - master IP
#   - instance-info file

set -e

if [ $# -ne 5 ]; then
  echo "Usage: $0 <exp_data_dir> <tag> <num_slaves> <master_ip> <instance_info_file>"
  exit 1
fi

exp_data_dir="$1"
tag="$2"
num_slaves="$3"
master_ip="$4"
instance_info_file="$5"

# Load global vars (remote_user, remote_gopath, remote_work_dir, remote_bin_dir, ssh_options, etc)
source scripts/global-vars.sh

this_dir=$(cd "$(dirname "$0")" && pwd)
deployment_dir=$(cd "$this_dir/.." && pwd)
repo_dir=$(cd "$deployment_dir/.." && pwd)

echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] ==== [start-remote-slaves] Diretórios detectados ====="
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   this_dir       = $this_dir"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   deployment_dir = $deployment_dir"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   repo_dir       = $repo_dir"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   remote_user    = $remote_user"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   remote_gopath  = $remote_gopath"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   remote_bin_dir = $remote_bin_dir"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   remote_work_dir= $remote_work_dir"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   remote_exp_dir = $remote_exp_dir"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] "

###############################################################################
# 1) Read instances of the given tag from instance-info
###############################################################################

mapfile -t instances < <(awk -v tgt="$tag" '$4 == "slave" && $5 == tgt {print $0}' "$instance_info_file")

if [ "${#instances[@]}" -ne "$num_slaves" ]; then
  echo "[WARN  ][$(date +"%Y-%m-%d %H:%M:%S")] Esperados $num_slaves slaves com tag '$tag', mas instance-info retornou ${#instances[@]}."
fi

###############################################################################
# 2) Distribute scripts/binaries
###############################################################################

echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] ==== [start-remote-slaves] Distribuindo scripts/binários aos slaves ===="

for line in "${instances[@]}"; do
  instance_id=$(echo "$line" | awk '{print $1}')
  ctrl_ip=$(echo "$line" | awk '{print $2}')
  data_ip=$(echo "$line" | awk '{print $3}')
  role=$(echo "$line" | awk '{print $4}')
  itag=$(echo "$line" | awk '{print $5}')

  echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] ---------------------------------------------------------------------"
  echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   [REMOTO] Garantindo ambiente em $ctrl_ip"
  echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]            instance_id = $instance_id"
  echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]            tag         = $itag"
  echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] ---------------------------------------------------------------------"

  # Scripts auxiliares
  scp $ssh_options \
    "$deployment_dir/scripts/global-vars.sh" \
    "$deployment_dir/scripts/remote-machine-status.sh" \
    "$deployment_dir/scripts/stubborn-scp.sh" \
    "$remote_user@$ctrl_ip:$remote_work_dir/scripts/" || true

  # Binários
  scp $ssh_options \
    "$remote_bin_dir/discoverymaster" \
    "$remote_bin_dir/discoveryslave" \
    "$remote_bin_dir/orderingpeer" \
    "$remote_bin_dir/orderingclient" \
    "$remote_user@$ctrl_ip:$remote_bin_dir/" || true

  echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]     [REMOTO] OK: ambiente garantido em $ctrl_ip."
done

echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] ==== [start-remote-slaves] Distribuição concluída. ===="
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] "

###############################################################################
# 3) Start slaves
###############################################################################

echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] ==== [start-remote-slaves] Disparando slaves da tag '$tag' ===="

for line in "${instances[@]}"; do
  instance_id=$(echo "$line" | awk '{print $1}')
  ctrl_ip=$(echo "$line" | awk '{print $2}')
  data_ip=$(echo "$line" | awk '{print $3}')
  role=$(echo "$line" | awk '{print $4}')
  itag=$(echo "$line" | awk '{print $5}')

  echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   [DEPLOY] Iniciando slave em $ctrl_ip (instance_id=$instance_id, tag=$itag)"

  scp $ssh_options \
    "$deployment_dir/scripts/start-slave.sh" \
    "$remote_user@$ctrl_ip:$remote_work_dir/start-slave.sh" >/dev/null 2>&1 || true

  ssh $ssh_options "$remote_user@$ctrl_ip" " \
    cd $remote_work_dir && \
    ./start-slave.sh $tag $master_ip $data_ip $instance_id \
  " >/dev/null 2>&1 || true &
done

wait

echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] "
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] ==== [start-remote-slaves] Todos os slaves disparados. ===="
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] ==== [start-remote-slaves] FIM ==========================================="

