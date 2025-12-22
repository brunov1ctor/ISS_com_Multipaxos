#!/usr/bin/env bash

# deploy-remote.sh (versão enxuta de logs)

set -e

ts() { date +"%Y-%m-%d %H:%M:%S%z"; }
log_i() { echo "[INFO  ][$(ts)] $*"; }
log_w() { echo "[WARN  ][$(ts)] $*" >&2; }
log_e() { echo "[ERRO  ][$(ts)] $*" >&2; }

# Flag de cancelamento de instâncias (padrão: false)
: "${cancel_instances:=false}"

# =====================================================================
# 1) Variáveis esperadas do ambiente (de deploy.sh + global-vars.sh)
# =====================================================================

if [[ -z "${exp_data_dir:-}" || -z "${instance_info_file:-}" ]]; then
  log_e "exp_data_dir ou instance_info_file vazio."
  log_e "exp_data_dir='${exp_data_dir:-}' instance_info_file='${instance_info_file:-}'"
  exit 1
fi

instance_info_file_name="$(basename "$instance_info_file")"

# defaults “seguros” se não vierem setados de fora
local_master_command_template_file="${local_master_command_template_file:-master-commands-template.cmd}"
local_master_command_file="${local_master_command_file:-master-commands.cmd}"
local_result_fetching_log="${local_result_fetching_log:-result-fetching.log}"

remote_user="${remote_user:-${USER}}"
remote_work_dir="${remote_work_dir:-/users/${remote_user}/iss}"
remote_bin_dir="${remote_bin_dir:-/users/${remote_user}/go/bin}"
ssh_options="${ssh_options:--o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -T -o BatchMode=yes -o ConnectTimeout=8 -o ConnectionAttempts=1 -o ServerAliveInterval=5 -o ServerAliveCountMax=2 -o LogLevel=ERROR -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ControlMaster=no -o ControlPath=none -o ControlPersist=no}"
remote_status_file="${remote_status_file:-${remote_work_dir}/status}"

# =====================================================================
# 2) Descobrir IP do master e copiar instance-info
# =====================================================================

master_ip=$(awk '$4 == "master" {print $2}' "$instance_info_file" | head -n1)

if [ -z "$master_ip" ]; then
  log_e "Não foi possível obter IP do master em $instance_info_file."
  exit 1
fi

cp "$instance_info_file" "$exp_data_dir/$instance_info_file_name"

log_i "Master: $master_ip (instance-info: $instance_info_file)"

# =====================================================================
# 3) Garantir master-commands-template.cmd
# =====================================================================

template_path="$exp_data_dir/$local_master_command_template_file"
deployment_file="$exp_data_dir/deployment.dpl"

if [ ! -f "$template_path" ]; then
  log_i "Gerando master-commands-template.cmd..."

  if [ ! -f "$deployment_file" ]; then
    log_e "Deployment file não encontrado: $deployment_file"
    exit 1
  fi

  # Uso correto: generate-master-commands.py <deplType> <deployment.dpl> <outFile> <local_exp_data>
  if ! python3 scripts/generate-master-commands.py remote "$deployment_file" "$template_path" "$exp_data_dir"; then
    log_e "Falha ao gerar master-commands-template.cmd."
    exit 1
  fi
else
  log_i "master-commands-template.cmd já existe."
fi

# =====================================================================
# 4) Gerar master-commands.cmd final com envsubst
# =====================================================================

export ssh_key_file="$remote_private_key_file"
export own_public_ip="$master_ip"
export master_port
export status_file="$remote_status_file"

log_i "Gerando master-commands.cmd..."
envsubst '$ssh_key_file $own_public_ip $master_port $status_file' \
  < "$template_path" \
  > "$exp_data_dir/$local_master_command_file"

echo -e "\nwrite-file $status_file DONE" >> "$exp_data_dir/$local_master_command_file"

log_i "master-commands.cmd: $exp_data_dir/$local_master_command_file"

# =====================================================================
# 5) Reset remoto: matar processos antigos + limpar estado
# =====================================================================

log_i "Resetando estado nas máquinas remotas..."

# Mata analyze-continuously (se estiver rodando)
for ip in $(awk '{print $2}' "$instance_info_file"); do
  ssh $ssh_options "${remote_user}@${ip}" \
    "kill -9 \$(ps -ef | grep 'analyze-continuously' | grep -v \$\$ | awk '{print \$2}')" \
    >/dev/null 2>&1 || log_w "$ip: não foi possível encerrar 'analyze-continuously'."
  sleep 0.1
done

# Limpa estado, mata binários velhos, reseta status
for ip in $(awk '{print $2}' "$instance_info_file"); do
  remote_delete_files="$remote_work_dir"
  remote_status_file="$remote_work_dir/status"

  ssh $ssh_options "${remote_user}@${ip}" "
    tc qdisc del dev eth0 root tbf rate 1gbit burst 320kbit latency 400ms 2>/dev/null || true
    killall -9 discoverymaster discoveryslave orderingpeer orderingclient scp rsync 2>/dev/null || true
    rm -rf $remote_delete_files
    echo RUNNING > $remote_status_file
    kill -9 \$(ps -ef | grep 'sshd: ${remote_user}@notty' | awk '{print \$2}') 2>/dev/null || true
  " >/dev/null 2>&1 || log_w "$ip: falha no reset remoto."
  sleep 0.1
done
wait

log_i "Reset remoto concluído."

# =====================================================================
# 5b) Garantir diretório de resultados no master
# =====================================================================

log_i "Garantindo diretório de resultados no master..."
ssh $ssh_options "${remote_user}@${master_ip}" "
  mkdir -p /users/${remote_user}/iss/current-deployment-data/raw-results
" >/dev/null 2>&1 || log_w "Não foi possível criar raw-results no master."

# =====================================================================
# 6) Start master
# =====================================================================

log_i "Iniciando master em $master_ip..."

scripts/start-master.sh \
  "$remote_user" \
  "$master_ip" \
  "$remote_work_dir" \
  "$remote_bin_dir" \
  "$exp_data_dir" \
  "$exp_data_dir/$local_master_command_file" &

log_i "start-master.sh em background."

# =====================================================================
# 7) Start slaves (peers + 1client)
# =====================================================================

log_i "Iniciando slaves (peers)..."
scripts/start-remote-slaves.sh "$exp_data_dir" 0 peers "$instance_info_file"

log_i "Iniciando slaves (1client)..."
scripts/start-remote-slaves.sh "$exp_data_dir" 0 1client "$instance_info_file"

log_i "Slaves iniciados."

# =====================================================================
# 8) Fetch de resultados em background
# =====================================================================

log_i "Iniciando coleta de resultados..."
scripts/fetch-results.sh "$master_ip" "$exp_data_dir" \
  > "$exp_data_dir/$local_result_fetching_log" 2>&1 &

fetch_pid=$!

log_i "Log de coleta: $exp_data_dir/$local_result_fetching_log"

wait "$fetch_pid" || log_w "fetch-results.sh terminou com código $?."

# =====================================================================
# 9) Cancelar instâncias (se configurado)
# =====================================================================

if $cancel_instances; then
  log_i "Cancelando máquinas na nuvem..."
  scripts/cancel-cloud-instances.sh "$exp_data_dir/$instance_info_file_name"
else
  echo -e "Lembre-se de cancelar as VMs com:\n  cancel-cloud-instances.sh $exp_data_dir/$instance_info_file_name\n"
fi

log_i "deploy-remote.sh concluído."

