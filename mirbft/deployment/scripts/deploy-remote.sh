#!/bin/bash

set -euo pipefail

# Carrega variáveis globais do ambiente de deployment
source scripts/global-vars.sh

exp_data_dir="$1"          # ex.: deployment-data/remote-0000
instance_info_file="$2"    # ex.: scripts/instance-info
deploy_schedule="$3"       # ex.: new / reuse / etc.

instance_info_file_name=$(basename "$instance_info_file")

###############################################################################
# Descobrir IP do master
###############################################################################

master_ip=$(awk '$4 == "master" {print $2}' "$instance_info_file")
if [ -z "$master_ip" ]; then
  >&2 echo "remote-deploy.sh: could not obtain master ip from instance info file: $instance_info_file"
  exit 1
fi

cp "$instance_info_file" "$exp_data_dir/$instance_info_file_name"
echo "Using instance info file: $instance_info_file"
echo "       Master IP address: $master_ip"

###############################################################################
# Gerar arquivo de comandos do master (master-commands.cmd)
###############################################################################

export ssh_key_file="$remote_private_key_file"
export own_public_ip="$master_ip"
export master_port
export status_file="$remote_status_file"

envsubst '$ssh_key_file $own_public_ip $master_port $status_file' \
  < "$exp_data_dir/$local_master_command_template_file" \
  > "$exp_data_dir/$local_master_command_file"

echo -e "\nwrite-file $status_file DONE" >> "$exp_data_dir/$local_master_command_file"

echo "Using pre-generated master command script at $exp_data_dir/$local_master_command_file."
echo "Master command script written to $exp_data_dir/$local_master_command_file."
echo ""

###############################################################################
# Matar tudo que estiver rodando e limpar estado remoto
###############################################################################

echo "Killing everything that is alive and pruning state on the remote machines (including SSH) and removing potential bandwidth limit."

# 1) para analyze-continuously
for ip in $(awk '{print $2}' "$instance_info_file"); do
  ssh $ssh_options "$ip" \
    "kill -9 \$(ps -ef | grep 'analyze-continuously' | grep -v \$\$ | awk '{print \$2}') || true" &
  sleep 0.1
done
wait

echo -e "\nKilled continuous analysis scripts.\n"

# 2) matar discovery/ordering/cliente e limpar diretórios
for ip in $(awk '{print $2}' "$instance_info_file"); do
  ssh $ssh_options "$ip" "
    # tc qdisc del dev eth0 root tbf rate 1gbit burst 320kbit latency 400ms
    killall -9 discoverymaster discoveryslave orderingpeer orderingclient scp rsync || true
    rm -rf $remote_delete_files

    # Garantir diretórios remotos básicos
    mkdir -p $remote_work_dir $remote_exp_dir
    mkdir -p \$(dirname $remote_status_file)

    echo RUNNING > $remote_status_file
    kill -9 \$(ps -ef | grep 'sshd: notty' | awk '{print \$2}') || true
    echo -e '\n\n\nBERO\n\n\n'
  " &
  sleep 0.1
done
wait

echo -e "\n Reset machine state.\n"

###############################################################################
# Iniciar master, slaves e coleta de resultados
###############################################################################

# Start the master server (gera/copia comandos, configs, analise contínua, discoverymaster)
scripts/start-master.sh "$exp_data_dir" "$master_ip" &

# Start slaves according to schedule
scripts/deploy-slaves-remote.sh "$exp_data_dir" "$instance_info_file" "$master_ip" "$deploy_schedule" &

# Start result fetching in the background.
scripts/fetch-results.sh "$master_ip" "$exp_data_dir" > "$exp_data_dir/$local_result_fetching_log" 2>&1 &

echo "Waiting for deployment process and result fetching to finish."
echo "For progress on experiment result fetching, see $exp_data_dir/$local_result_fetching_log."
wait

###############################################################################
# Cancelar VMs de nuvem, se configurado
###############################################################################

if $cancel_instances; then
  scripts/cancel-cloud-instances.sh "$exp_data_dir/$instance_info_file_name"
else
  echo -e "Do not forget to cancel the used virtual servers using\n\n    scripts/cancel-cloud-instances.sh $exp_data_dir/$instance_info_file_name \n"
fi

