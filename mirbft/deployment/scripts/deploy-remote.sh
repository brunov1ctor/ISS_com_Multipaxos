# Obtain IP of the master node
master_ip=$(awk '$4 == "master" {print $2}' "$instance_info_file")
if [ -z "$master_ip" ]; then
  >&2 echo "deploy-remote.sh: could not obtain master ip from instance info file: $instance_info_file"
  exit 1
fi

# Registra o instance-info dentro do diretório do experimento
cp "$instance_info_file" "$exp_data_dir/$instance_info_file_name"
echo "Using instance info file: $instance_info_file"
echo "       Master IP address: $master_ip"
echo

# Carrega variáveis globais (remote_work_dir, remote_status_file, remote_delete_files, etc.).
# initialize-deployment.sh já foi 'source'd pelo deploy.sh antes de chegar aqui.
source scripts/global-vars.sh

###############################################################################
# 1) Limpeza dos nós remotos (sem usuário hard-coded)
###############################################################################

echo "Killing everything that is alive and pruning state on the remote machines (including SSH) and removing potential bandwidth limit."

# 1a) Mata scripts de análise contínua antigos, se existirem
for ip in $(awk '{print $2}' "$instance_info_file"); do
  ssh $ssh_options "$ip" " \
    pids=\$(ps -ef | grep 'analyze-continuously' | grep -v grep | awk '{print \$2}'); \
    if [ -n \"\$pids\" ]; then kill -9 \$pids || true; fi \
  " >/dev/null 2>&1 || true &
  sleep 0.1
done
wait

echo
echo "Killed continuous analysis scripts."
echo

# 1b) Limpa estado antigo (processos e arquivos) em todos os nós
for ip in $(awk '{print $2}' "$instance_info_file"); do
  ssh $ssh_options "$ip" " \
    # Remove traffic shaping antigo (ignora erros).
    tc qdisc del dev eth0 root tbf rate 1gbit burst 320kbit latency 400ms 2>/dev/null || true; \
    # Mata processos de experimentos anteriores (ignora erros se não existirem).
    killall -9 discoverymaster discoveryslave orderingpeer orderingclient scp rsync 2>/dev/null || true; \
    # Remove arquivos antigos relacionados ao experimento.
    rm -rf $remote_delete_files 2>/dev/null || true; \
    # Garante diretório do status e reseta para RUNNING.
    mkdir -p \"$(dirname "$remote_status_file")\" 2>/dev/null || true; \
    echo RUNNING > \"$remote_status_file\" 2>/dev/null || true \
  " >/dev/null 2>&1 || true &
  sleep 0.1
done
wait

echo
echo " Reset machine state."
echo

###############################################################################
# 2) Export de variáveis usadas por generate-master-commands / templates
###############################################################################

# Essas variáveis são consumidas indiretamente pelos comandos gerados
# (master-commands.cmd) e/ou por scripts auxiliares.
export ssh_key_file="$remote_private_key_file"
export own_public_ip="$master_ip"
export master_port
export status_file="$remote_status_file"

###############################################################################
# 3) Dispara master (discoverymaster + orderingclient) no nó master
###############################################################################

echo "Starting master on $master_ip."
scripts/start-master.sh "$exp_data_dir" "$master_ip"

###############################################################################
# 4) Dispara slaves remotos (peers e clients) de acordo com o instance-info
###############################################################################

# Conta quantos slaves com tag 'peers' e '1client' existem no instance-info.
num_peers=$(awk '$4 == "slave" && $5 == "peers" {c++} END {print c+0}' "$instance_info_file")
num_clients=$(awk '$4 == "slave" && $5 == "1client" {c++} END {print c+0}' "$instance_info_file")

if [ "$num_peers" -gt 0 ]; then
  echo "Starting $num_peers peer slaves."
  scripts/start-remote-slaves.sh "$exp_data_dir" "peers" "$num_peers" "$master_ip" "$instance_info_file"
fi

if [ "$num_clients" -gt 0 ]; then
  echo "Starting $num_clients client slaves (tag=1client)."
  scripts/start-remote-slaves.sh "$exp_data_dir" "1client" "$num_clients" "$master_ip" "$instance_info_file"
fi

echo "All slaves started. waiting for them to finish."
echo "Remote slave deployment finished."

###############################################################################
# 5) Inicia a coleta de resultados em background
###############################################################################

# Start result fetching in the background.
scripts/fetch-results.sh "$master_ip" "$exp_data_dir" > "$exp_data_dir/$local_result_fetching_log" 2>&1 &

echo "Waiting for deployment process and result fetching to finish."
echo "For progress on experiment result fetching, see $exp_data_dir/$local_result_fetching_log."
wait

# Cancel cloud machines if configured to do so.
if $cancel_instances; then
  scripts/cancel-cloud-instances.sh "$exp_data_dir/$instance_info_file_name"
else
  echo -e "Do not forget to cancel the used virtual servers using\n\n    scripts/cancel-cloud-instances.sh $exp_data_dir/$instance_info_file_name \n"
fi

