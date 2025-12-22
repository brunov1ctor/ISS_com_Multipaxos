#!/usr/bin/env bash

# deploy-remote.sh (versão corrigida)

set -e

ts() { date +"%Y-%m-%d %H:%M:%S%z"; }
log_i() { echo "[INFO  ][$(ts)] $*"; }
log_w() { echo "[WARN  ][$(ts)] $*" >&2; }
log_e() { echo "[ERRO  ][$(ts)] $*" >&2; }

exec 2> >(grep -v "tc qdisc del dev eth0 root tbf rate 1gbit burst 320kbit latency 400ms" >&2)

# Flag de cancelamento de instâncias (padrão: false)
: "${cancel_instances:=false}"

# =====================================================================
# 1) Variáveis esperadas do ambiente (de deploy.sh + global-vars.sh)
# =====================================================================
# Esperados:
#   - exp_data_dir
#   - instance_info_file
#   - deployment_data_root
#   - local_master_command_template_file
#   - local_master_command_file
#   - local_result_summary_csv
#   - local_result_fetching_log
#   - remote_user
#   - ssh_options
#   - remote_work_dir
#   - remote_bin_dir
#   - status_file
#   - master_ip
#   - master_port
#   - local_result_fetching_log
#   - remote_user
#   - ssh_options
#   - remote_work_dir
#   - remote_bin_dir

if [[ -z "${exp_data_dir:-}" || -z "${instance_info_file:-}" ]]; then
  log_e "deploy-remote.sh: exp_data_dir ou instance_info_file vazio."
  log_e "  exp_data_dir='${exp_data_dir:-}' instance_info_file='${instance_info_file:-}'"
  exit 1
fi

instance_info_file_name="$(basename "$instance_info_file")"

# Caminhos derivados
deployment_file="$exp_data_dir/deployment.dpl"
template_path="${local_master_command_template_file:-$exp_data_dir/master-commands-template.cmd}"
local_master_cmd="${local_master_command_file:-$exp_data_dir/master-commands.cmd}"

# =====================================================================
# 2) Sanidade dos arquivos e diretórios locais
# =====================================================================

if [[ ! -f "$instance_info_file" ]]; then
  log_e "Arquivo instance-info não encontrado: $instance_info_file"
  exit 1
fi

if [[ ! -d "$exp_data_dir" ]]; then
  log_e "Diretório de experimento não existe: $exp_data_dir"
  exit 1
fi

if [[ ! -f "$deployment_file" ]]; then
  log_e "Arquivo deployment.dpl não encontrado em: $deployment_file"
  exit 1
fi

mkdir -p "$exp_data_dir/logs" "$exp_data_dir/_debug"

# =====================================================================
# 3) Garantir que status_file aponte para algo sensato
# =====================================================================

# Se não vier de fora, usar o que o script original espera
remote_status_file="${remote_status_file:-${remote_work_dir}/status}"

# Mas o deploy.sh também define um status_file local, então fazemos:
export status_file="$remote_status_file"

# =====================================================================
# 4) Gerar master-commands-template.cmd (se não existir)
# =====================================================================

if [[ ! -f "$template_path" ]]; then
  log_i "master-commands-template.cmd não encontrado. Gerando via generate-master-commands.py..."
  log_i "  deployment_file (.dpl) = $deployment_file"
  log_i "  template out           = $template_path"

  # Uso correto: generate-master-commands.py <deplType> <deployment.dpl> <outFile> <local_exp_data>
  if ! python3 scripts/generate-master-commands.py remote "$deployment_file" "$template_path" "$exp_data_dir"; then
    log_e "Falha ao gerar master-commands-template.cmd via generate-master-commands.py"
    exit 1
  fi
else
  log_i "master-commands-template.cmd já existe em: $template_path"
fi

# =====================================================================
# 4) Gerar master-commands.cmd final com envsubst
# =====================================================================

# Variáveis que o template usa
export ssh_key_file="$remote_priv_key"
export remote_user="$remote_user"
export remote_work_dir="$remote_work_dir"
export remote_bin_dir="$remote_bin_dir"
export instance_info_file_name="$instance_info_file_name"
export exp_data_dir="$exp_data_dir"

# Para o template antigo que espera algumas variáveis extras:
export deployment_data_root="${deployment_data_root:-$(dirname "$exp_data_dir")}"

# Gera master-commands.cmd substituindo variáveis
log_i "Generating final master command file a partir do template..."
if ! envsubst < "$template_path" > "$local_master_cmd"; then
  log_e "Falha ao gerar master-commands.cmd via envsubst."
  exit 1
fi

log_i "Master command file pronto: $local_master_cmd"

# =====================================================================
# 5) Limpar estado prévio nas máquinas remotas
# =====================================================================

log_i "Killing everything that is alive and pruning state on the remote machines (including SSH) and removing potential bandwidth limit."

# 5a) Matar scripts de análise contínua, se houver
for ip in $(awk '{print $2}' "$instance_info_file"); do
  ssh $ssh_options "${remote_user}@${ip}" \
    "kill -9 \$(ps -ef | grep 'analyze-continuously' | grep -v \$\$ | awk '{print \$2}')" \
    >/dev/null 2>&1 || log_w "$ip: could not kill analyze-continuously (continuando)."
  sleep 0.1
done

log_i "Killed continuous analysis scripts."

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
  " >/dev/null 2>&1 || log_w "$ip: reset failed (continuando)."
  sleep 0.1
done
wait

echo
log_i "Reset machine state."
echo

# =====================================================================
# 5b) Garantir diretório de resultados no master
# =====================================================================

log_i "Ensuring raw-results directory exists on master at ${remote_work_dir}/current-deployment-data/raw-results ..."

ssh $ssh_options "${remote_user}@${master_ip}" "mkdir -p ${remote_work_dir}/current-deployment-data/raw-results" \
  >/dev/null 2>&1 || {
    log_e "Could not ensure raw-results directory on master."
    exit 1
  }

# =====================================================================
# 6) Iniciar master
# =====================================================================

log_i "Starting master on ${master_ip}..."
nohup ssh $ssh_options "${remote_user}@${master_ip}" \
  "cd ${remote_work_dir} && ./scripts/start-master.sh '${exp_data_dir}' '${instance_info_file}' '${remote_work_dir}' '${remote_bin_dir}' '${local_master_cmd}' '${master_port}'" \
  >/dev/null 2>&1 &

log_i "start-master.sh disparado em background (remote_user=${remote_user}, master_ip=${master_ip})."

# =====================================================================
# 7) Iniciar slaves (peers + clients)
# =====================================================================

log_i "Starting peer slaves (tag=peers)..."
./scripts/start-remote-slaves.sh "$exp_data_dir" "$instance_info_file" "peers" "$remote_user" "$remote_work_dir" "$remote_bin_dir" "$master_ip" "$master_port" "$ssh_options"

log_i "Starting client slaves (tag=1client)..."
./scripts/start-remote-slaves.sh "$exp_data_dir" "$instance_info_file" "1client" "$remote_user" "$remote_work_dir" "$remote_bin_dir" "$master_ip" "$master_port" "$ssh_options"

log_i "All slaves started."

# =====================================================================
# 8) Iniciar coleta de resultados
# =====================================================================

log_i "Starting result fetching in the background..."
nohup ./scripts/analyze/extract-successful.sh "$exp_data_dir" "$local_result_summary_csv" "$local_result_fetching_log" \
  >/dev/null 2>&1 &

log_i "Waiting for deployment process and result fetching to finish."
wait

log_i "deploy-remote.sh finished."

# =====================================================================
# 9) Mensagem final
# =====================================================================

cat <<EOF

Do not forget to cancel the used virtual servers using cancel-cloud-instances.sh $exp_data_dir/instance-info 

EOF

