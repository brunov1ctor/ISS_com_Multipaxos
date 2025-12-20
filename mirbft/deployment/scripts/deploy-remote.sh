#!/usr/bin/env bash

# Versão próxima do original, só com:
# - logs extras
# - uso de $remote_user em vez de root
# - geração automática do master-commands-template.cmd se estiver faltando

set -e

ts() { date +"%Y-%m-%d %H:%M:%S%z"; }
log_i() { echo "[INFO  ][$(ts)] $*"; }
log_w() { echo "[WARN  ][$(ts)] $*" >&2; }
log_e() { echo "[ERRO  ][$(ts)] $*" >&2; }

# Flag de cancelamento de instâncias (padrão: false)
: "${cancel_instances:=false}"

# =====================================================================
# 1) Descobrir IP do master
# =====================================================================

# Variáveis esperadas do ambiente (de deploy.sh + global-vars.sh):
#   - exp_data_dir
#   - instance_info_file
#   - deployment_data_root
#   - remote_user, ssh_options, remote_work_dir, remote_bin_dir
#   - local_master_command_template_file, local_master_command_file
#   - remote_status_file
#   - master_port
#   - local_result_fetching_log
#   - instance_info_file_name

if [[ -z "${exp_data_dir:-}" || -z "${instance_info_file:-}" ]]; then
  log_e "deploy-remote.sh: exp_data_dir ou instance_info_file vazio."
  log_e "  exp_data_dir='${exp_data_dir:-}' instance_info_file='${instance_info_file:-}'"
  exit 1
fi

instance_info_file_name="$(basename "$instance_info_file")"

master_ip=$(awk '$4 == "master" {print $2}' "$instance_info_file" | head -n1)

if [ -z "$master_ip" ]; then
  log_e "deploy-remote.sh: could not obtain master ip from instance info file: $instance_info_file"
  exit 1
fi

cp "$instance_info_file" "$exp_data_dir/$instance_info_file_name"

log_i "Using instance info file: $instance_info_file"
log_i "Master IP address      : $master_ip"

# =====================================================================
# 2) Garantir master-commands-template.cmd
# =====================================================================

template_path="$exp_data_dir/$local_master_command_template_file"
deployment_file="$exp_data_dir/deployment.dpl"

if [ ! -f "$template_path" ]; then
  log_i "master-commands-template.cmd não encontrado. Gerando via generate-master-commands.py..."
  log_i "  deployment_file (.dpl) = $deployment_file"
  log_i "  template out           = $template_path"

  if [ ! -f "$deployment_file" ]; then
    log_e "Deployment file não encontrado: $deployment_file"
    exit 1
  fi

  # Python que gera o template a partir do .dpl
  # Uso correto: generate-master-commands.py <deplType> <deployment.dpl> <outFile> <local_exp_data>
  if ! python3 scripts/generate-master-commands.py remote "$deployment_file" "$template_path" "$exp_data_dir"; then
    log_e "Falha ao gerar master-commands-template.cmd via generate-master-commands.py"
    exit 1
  fi
else
  log_i "master-commands-template.cmd já existe em: $template_path"
fi

# =====================================================================
# 3) Gerar master-commands.cmd final com envsubst
# =====================================================================

export ssh_key_file="$remote_private_key_file"
export own_public_ip="$master_ip"
export master_port
export status_file="$remote_status_file"

log_i "Generating final master command file a partir do template..."
envsubst '$ssh_key_file $own_public_ip $master_port $status_file' \
  < "$template_path" \
  > "$exp_data_dir/$local_master_command_file"

echo -e "\nwrite-file $status_file DONE" >> "$exp_data_dir/$local_master_command_file"

log_i "Master command file pronto: $exp_data_dir/$local_master_command_file"

# =====================================================================
# 4) Reset remoto: matar processos antigos + limpar estado
# =====================================================================

log_i "Killing everything that is alive and pruning state on the remote machines (including SSH) and removing potential bandwidth limit."

for ip in $(awk '{print $2}' "$instance_info_file"); do
  # o grep -v $$ impede matar o próprio script
  ssh $ssh_options "${remote_user}@${ip}" \
    "kill -9 \$(ps -ef | grep 'analyze-continuously' | grep -v \$\$ | awk '{print \$2}')" \
    >/dev/null 2>&1 || log_w "$ip: could not kill analyze-continuously (continuando)."
  sleep 0.1 # abrir muitas conexões ssh de uma vez dá erro em algumas
done

log_i "Killed continuous analysis scripts."

for ip in $(awk '{print $2}' "$instance_info_file"); do
  remote_delete_files="$remote_work_dir"
  remote_status_file="$remote_work_dir/status"

  ssh $ssh_options "${remote_user}@${ip}" "
    tc qdisc del dev eth0 root tbf rate 1gbit burst 320kbit latency 400ms 2>/dev/null || true
    killall -9 discoverymaster discoveryslave orderingpeer orderingclient scp rsync 2>/dev/null || true
    rm -rf $remote_delete_files
    echo RUNNING > $remote_status_file
    kill -9 \$(ps -ef | grep 'sshd: ${remote_user}@notty' | awk '{print \$2}') 2>/dev/null || true
  " >/dev/null 2>&1 || log_w "$ip: reset failed (continuando)."
  sleep 0.1
done
wait

echo
log_i "Reset machine state."
echo

# =====================================================================
# 5) Start master
# =====================================================================

log_i "Starting master on $master_ip..."
# start-master.sh usa remote_user, master_ip, remote_work_dir e remote_bin_dir do ambiente.
# Não passamos exp_data_dir como argumento para não sobrescrever remote_user.
scripts/start-master.sh &
log_i "start-master.sh disparado em background."

# =====================================================================
# 6) Start slaves (peers + 1client)
# =====================================================================

log_i "Starting peer slaves (tag=peers)..."
# desired_count=0 => inicia todos os slaves com a tag solicitada.
scripts/start-remote-slaves.sh "$exp_data_dir" 0 peers "$instance_info_file"

log_i "Starting client slaves (tag=1client)..."
# desired_count=0 => inicia todos os slaves com a tag solicitada.
scripts/start-remote-slaves.sh "$exp_data_dir" 0 1client "$instance_info_file"

log_i "All slaves started."

# =====================================================================
# 7) Fetch de resultados em background
# =====================================================================

log_i "Starting result fetching in the background..."
scripts/fetch-results.sh "$master_ip" "$exp_data_dir" \
  > "$exp_data_dir/$local_result_fetching_log" 2>&1 &

fetch_pid=$!

log_i "Waiting for deployment process and result fetching to finish."
log_i "For progress on experiment result fetching, see:"
log_i "  $exp_data_dir/$local_result_fetching_log"
echo

wait "$fetch_pid" || log_w "fetch-results.sh terminou com código $? (veja o log acima)."

# =====================================================================
# 8) Cancelar instâncias (se configurado)
# =====================================================================

if $cancel_instances; then
  log_i "Canceling cloud machines as requested..."
  scripts/cancel-cloud-instances.sh "$exp_data_dir/$instance_info_file_name"
else
  echo -e "Do not forget to cancel the used virtual servers using cancel-cloud-instances.sh $exp_data_dir/$instance_info_file_name \n"
fi

log_i "deploy-remote.sh finished."

