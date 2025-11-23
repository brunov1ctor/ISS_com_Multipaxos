#!/bin/bash

set -e

exp_data_dir="$1"
master_ip="$2"

source scripts/global-vars.sh

# Ensure all required directories exist
mkdir -p "$remote_work_dir"
mkdir -p "$remote_exp_dir"
mkdir -p "$remote_request_payload_dir"

echo "Starting master at $master_ip"

ssh $ssh_options "$master_ip" "
    mkdir -p $remote_work_dir;
    mkdir -p $remote_exp_dir;
    mkdir -p $remote_request_payload_dir;
" || true

# Start master command processor
ssh $ssh_options "$master_ip" "
    cd $remote_work_dir;
    nohup ./scripts/order_master.sh master-commands.cmd > master-log.log 2>&1 &
"

