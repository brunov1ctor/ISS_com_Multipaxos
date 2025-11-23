#!/bin/bash
set -euo pipefail

###############################################################################
# Parâmetros
#   $1 = exp_data_dir          (ex: deployment-data/remote-0000)
#   $2 = instance_info_file    (ex: scripts/instance-info)
#   $3 = deploy_schedule       (ex: new, reuse, etc.)
###############################################################################
if [ "$#" -lt 3 ]; then
  >&2 echo "Usage: $0 <exp_data_dir> <instance_info_file> <deploy_schedule>"
  exit 1
fi

exp_data_dir="$1"
instance_info_file="$2"
deploy_schedule="$3"

# Garantir que estamos no diretório de deployment (onde estão scripts/global-vars.sh)
# Se você já garante isso em deploy.sh, isso aqui é só redundância inofensiva.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$DEPLOY_DIR"

# Carregar variáveis globais (remote_work_dir, remote_exp_dir, remote_status_file, etc.)
source "$SCRIPT_DIR/global-vars.sh"

###############################################################################
# Obter IP do master
###############################################################################

master_ip=$(awk '$4 == "master" {print $2}' "$instance_info_file" || true)
if [ -z "${master_ip:-}" ]; then
  >&2 echo "deploy-remote.sh: could not obtain master ip from instance info file: $instance_info_file"
  exit 1
fi

cp "$instance_info_file" "$exp_data_dir/$instance_info_file_name"
echo "Using instance info file: $instance_info_file"
echo "       Master IP address: $master_ip"

###############################################################################
# Gerar arquivo final de comandos do master
###############################################################################

export ssh_key_file="$remote_private_key_file"
export own_public_ip="$master_ip"
export master_port
export status_file="$remote_status_file"

envsubst '$ssh_key_file $own_public_ip $master_port $status_file' \
  < "$exp_data_dir/$local_master_command_template_file" \
  > "$exp_data_dir/$local_master_command_file"

# No fim do comando do master, escrever DONE no status
echo -e "\nwrite-file $status_file DONE" >> "$exp_data_dir/$local_master_command_file"

###############################################################################
# Matar tudo que estiver rodando nas máquinas remotas e limpar estado antigo
###############################################################################

echo "Killing everything that is alive and pruning state on the remote machines (including SSH) and removing potential bandwidth limit."

# 1) matar analyze-continuously
for ip in $(awk '{print $2}' "$instance_info_file"); do
  ssh $ssh_options "$ip" \
    "kill -9 \$(ps -ef | grep 'analyze-continuously' | grep -v \$\$ | awk '{print \$2}')" \
    || true &
  # Abrir muitas conexões ao mesmo tempo faz algumas falharem; pequeno sleep ajuda.
  sleep 0.1
done
wait

echo -e "\nKilled continuous analysis scripts.\n"

# 2) matar processos de experimento, limpar diretórios, garantir dirs do novo experimento
for ip in $(awk '{print $2}' "$instance_info_file"); do
  ssh $ssh_options "$ip" "
    # tc qdisc del dev eth0 root tbf rate 1gbit burst 320kbit latency 400ms

    # matar processos antigos (se não existirem, ignorar erro)
    killall -9 discoverymaster discoveryslave orderingpeer orderingclient scp rsync 2>/dev/null || true

    # remover arquivos/diretórios antigos
    rm -rf $remote_delete_files

    # garantir diretórios remotos necessários
    mkdir -p $remote_work_dir $remote_exp_dir
    mkdir -p \$(dirname \"$remote_status_file\")

    # (re)criar arquivo de status
    echo RUNNING > \"$remote_status_file\"

    # matar sessões ssh 'notty' antigas (se existirem)
    kill -9 \$(ps -ef | grep 'sshd: notty' | awk '{print \$2}') 2>/dev/null || true

    echo -e '\n\n\nBERO\n\n\n'
  " &
  sleep 0.1
done
wait

echo -e "\n Reset machine state.\n"

###############################################################################
# Iniciar master, slaves e coleta de resultados
###############################################################################

# 1) Master: copia comandos + config e inicia discoverymaster + analyze-continuously
scripts/start-master.sh "$exp_data_dir" "$master_ip" &

# 2) Slaves: start orderingpeer / orderingclient conforme o schedule
scripts/deploy-slaves-remote.sh "$exp_data_dir" "$instance_info_file" "$master_ip" "$deploy_schedule" &

# 3) Coleta de resultados em background
scripts/fetch-results.sh "$master_ip" "$exp_data_dir" > "$exp_data_dir/$local_result_fetching_log" 2>&1 &

echo "Waiting for deployment process and result fetching to finish."
echo "For progress on experiment result fetching, see $exp_data_dir/$local_result_fetching_log."
wait

###############################################################################
# Cancelar VMs na nuvem, se configurado
###############################################################################
if $cancel_instances; then
  scripts/cancel-cloud-instances.sh "$exp_data_dir/$instance_info_file_name"
else
  echo -e "Do not forget to cancel the used virtual servers using\n\n    scripts/cancel-cloud-instances.sh $exp_data_dir/$instance_info_file_name \n"
fi

