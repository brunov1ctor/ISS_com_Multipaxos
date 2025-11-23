#!/bin/bash

# Este script é *sourceado* por deploy.sh (não é chamado com argumentos).
# Ele assume que as variáveis abaixo já foram definidas:
#   exp_data_dir
#   instance_info_file
#   instance_info_file_name
#   local_master_command_template_file
#   local_master_command_file
#   local_result_fetching_log
#   remote_private_key_file
#   remote_status_file
#   remote_delete_files
#   remote_work_dir
#   remote_exp_dir
#   ssh_options
#   master_port
#   deploy_schedule
#   cancel_instances

############################
# 1) Descobrir IP do master
############################

master_ip=$(
  awk '$4 == "master" {print $2}' "$instance_info_file"
)

if [ -z "$master_ip" ]; then
  >&2 echo "deploy-remote.sh: could not obtain master ip from instance info file: $instance_info_file"
  exit 1
fi

cp "$instance_info_file" "$exp_data_dir/$instance_info_file_name"
echo "Using instance info file: $instance_info_file"
echo "       Master IP address: $master_ip"
echo

###########################################
# 2) Gerar o arquivo de comandos do master
###########################################

export ssh_key_file="$remote_private_key_file"
export own_public_ip="$master_ip"
export master_port
export status_file="$remote_status_file"

envsubst '$ssh_key_file $own_public_ip $master_port $status_file' \
  < "$exp_data_dir/$local_master_command_template_file" \
  > "$exp_data_dir/$local_master_command_file"

echo
echo "Using pre-generated master command script at $exp_data_dir/$local_master_command_file."
echo
echo "Master command script written to $exp_data_dir/$local_master_command_file."
echo

# No fim de tudo, o master vai escrever DONE nesse status_file
echo "write-file $remote_status_file DONE" >> "$exp_data_dir/$local_master_command_file"

###########################################################
# 3) Matar scripts de análise contínua nos nós remotos
###########################################################

echo "Killing everything that is alive and pruning state on the remote machines (including SSH) and removing potential bandwidth limit."

# Mata apenas os analyze-continuously
while read -r _ ip _; do
  ssh $ssh_options "$ip" \
    "kill -9 \$(ps -ef | grep 'analyze-continuously' | grep -v \$\$ | awk '{print \$2}')" \
    || true &
  # Evita abrir conexões demais de uma vez
  sleep 0.1
done < "$instance_info_file"
wait

echo -e "\nKilled continuous analysis scripts.\n"

###########################################################
# 4) Reset de estado + criação de diretórios remotos
###########################################################

while read -r _ ip _; do
  ssh $ssh_options "$ip" "
    # Remover possível limitação de banda (comentado no template original)
    # tc qdisc del dev eth0 root tbf rate 1gbit burst 320kbit latency 400ms

    # Mata processos antigos dos binários (ignora erro se não existir)
    killall -9 discoverymaster discoveryslave orderingpeer orderingclient scp rsync || true

    # Apaga lixo antigo
    rm -rf $remote_delete_files

    # Garante diretórios que o master/slaves esperam
    mkdir -p $remote_work_dir
    mkdir -p $remote_exp_dir
    mkdir -p $remote_exp_dir/raw-results
    mkdir -p $remote_work_dir/experiment-config

    # Garante diretório do status e zera o arquivo
    mkdir -p \$(dirname $remote_status_file)
    echo RUNNING > $remote_status_file

    # Fecha sessões sshd 'notty' antigas (se houver)
    kill -9 \$(ps -ef | grep 'sshd: notty' | awk '{print \$2}') || true

    echo -e '\n\n\nBERO\n\n\n'
  " &
  sleep 0.1
done < "$instance_info_file"
wait

echo -e "\n Reset machine state.\n"

###########################################
# 5) Iniciar master, slaves e coleta
###########################################

# Inicia o master no nó remoto indicado por master_ip
scripts/start-master.sh "$exp_data_dir" "$master_ip" &

# Inicia slaves conforme o arquivo deployment.dpl
scripts/deploy-slaves-remote.sh "$exp_data_dir" "$instance_info_file" "$master_ip" "$deploy_schedule" &

# Inicia o script que fica puxando os resultados
scripts/fetch-results.sh "$master_ip" "$exp_data_dir" \
  > "$exp_data_dir/$local_result_fetching_log" 2>&1 &

echo "Waiting for deployment process and result fetching to finish."
echo "For progress on experiment result fetching, see $exp_data_dir/$local_result_fetching_log."
wait

###########################################
# 6) Cancelar VMs, se configurado
###########################################

if $cancel_instances; then
  scripts/cancel-cloud-instances.sh "$exp_data_dir/$instance_info_file_name"
else
  echo -e "Do not forget to cancel the used virtual servers using\n\n    scripts/cancel-cloud-instances.sh $exp_data_dir/$instance_info_file_name \n"
fi

