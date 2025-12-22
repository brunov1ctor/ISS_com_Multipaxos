#!/usr/bin/env bash

# deploy-remote.sh (versão com logs enxutos)

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
# Esperados:
#   - exp_data_dir
#   - instance_info_file
#   - deployment_data_root
#   - local_master_command_template_file
#   - local_master_command_file
#   - remote_status_file
#   - remote_private_key_file
#   - master_port
#   - local_result_fetching_log
#   - remote_user
#   - ssh_options
#   - remote_work_dir
#   - remote_bin_dir

if [[ -z "${exp_data_dir:-}" || -z "${instance_info_file:-}" ]]; then
  log_e "exp_data_dir ou instance_info_file não definidos."
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
# 2) Descobrir IP do master e copiar instance-info para o diretório do experimento
# =====================================================================

master_ip=$(awk '$4 == "master" {print $2}' "$instance_info_file" | head -n1)

if [ -z "$master_ip" ]; then
  log_e "Não foi possível obter o IP do master a partir de: $instance_info_file"
  exit 1
fi

cp "$instance_info_file" "$exp_data_dir/$instance_info_file_name"

log_i "instance-info: $instance_info_file"
log_i "Master IP: $master_ip"

# =====================================================================
# 3) Garantir master-commands-template.cmd
# =====================================================================

template_path="$exp_data_dir/$local_master_command_template_file"
deployment_file="$exp_data_dir/deployment.dpl"

if [ ! -f "$template_path" ]; then
  log_i "Gerando master-commands-template.cmd..."
  log_i "deployment.dpl: $deployment_file"
  log_i "template:      $template_path"

  if [ ! -f "$deployment_file" ]; then
    log_e "Deployment file não encontrado: $deployment_file"
    exit 1
  fi

  # Uso correto: generate-master-commands.py <deplType> <deployment.dpl> <outFile> <local_exp_data>
  if ! python3 scripts/generate-master-commands.py remote "$deployment_file" "$template_path" "$exp_data_dir"; then
    log_e "Falha ao gerar master-commands-template.cmd via generate-master-commands.py"
    exit 1
  fi
else
  log_i "Usando master-commands-template existente: $template_path"
fi

# =====================================================================
# 4) Gerar master-commands.cmd final com envsubst
# =====================================================================

# Variáveis que o template usa
export ssh_key_file="$remote_private_key_file"
export own_public_ip="$master_ip"
export master_port
export status_file="$remote_status_file"

log_i "Gerando master-commands.cmd a partir do template..."
envsubst '$ssh_key_file $own_public_ip $master_port $status_file' \
  < "$template_path" \
  > "$exp_data_dir/$local_master_command_file"

echo -e "\nwrite-file $status_file DONE" >> "$exp_data_dir/$local_master_command_file"

log_i "master-commands.cmd pronto: $exp_data_dir/$local_master_command_file"

# =====================================================================
# 5) Reset remoto: matar processos antigos + limpar estado
# =====================================================================

log_i "Limpando processos antigos e estado remoto (incluindo SSH/bandwidth)."

# 5a) Mata analyze-continuously (se estiver rodando)
for ip in $(awk '{print $2}' "$instance_info_file"); do
  if ! ssh $ssh_options "${remote_user}@${ip}" "bash -s" >/dev/null 2>&1 <<'EOF_KILL_ANALYZE'
pids=$(ps -ef | grep 'analyze-continuously' | grep -v $$ | awk '{print $2}')
if [ -n "$pids" ]; then
  kill -9 $pids 2>/dev/null || true
fi
EOF_KILL_ANALYZE
  then
    log_w "$ip: não foi possível encerrar analyze-continuously (ok, seguindo)."
  fi
  sleep 0.1
done

log_i "Verificação de analyze-continuously concluída."

# 5b) Limpa estado, mata binários velhos, reseta status — com log mais descritivo
for ip in $(awk '{print $2}' "$instance_info_file"); do
  remote_delete_files="$remote_work_dir"
  remote_status_file="$remote_work_dir/status"

  log_i "Limpando processos antigos no nó $ip..."

  reset_out=$(ssh $ssh_options "${remote_user}@${ip}" "bash -s" 2>/dev/null <<EOF_RESET
# Processos discovery*/ordering* antigos
killed=\$(ps -ef | egrep 'discoverymaster|discoveryslave|orderingpeer|orderingclient' | grep -v grep | awk '{print \$2}')
if [ -n "\$killed" ]; then
  kill -9 \$killed 2>/dev/null || true
  echo "processos finalizados: \$killed"
else
  echo "nenhum processo discovery/ordering antigo encontrado"
fi

# Diretório de trabalho
if [ -d "$remote_delete_files" ]; then
  rm -rf "$remote_delete_files"
  echo "diretório limpo: $remote_delete_files"
else
  echo "diretório já inexistente: $remote_delete_files"
fi

# Status local
echo RUNNING > "$remote_status_file"

# Sessões ssh notty
notty=\$(ps -ef | grep 'sshd: ${remote_user}@notty' | grep -v grep | awk '{print \$2}')
if [ -n "\$notty" ]; then
  kill -9 \$notty 2>/dev/null || true
  echo "sessões ssh notty finalizadas: \$notty"
else
  echo "nenhuma sessão ssh notty encontrada"
fi
EOF_RESET
) || true

  if [ -n "$reset_out" ]; then
    # Loga cada linha de resultado como INFO, amigável
    while IFS= read -r line; do
      [ -n "$line" ] && log_i "$ip: $line"
    done <<< "$reset_out"
  else
    # Só aqui realmente indicamos problema real no reset
    log_w "$ip: não foi possível executar a limpeza remota (prosseguindo)."
  fi

  sleep 0.1
done
wait

echo
log_i "Reset remoto concluído."
echo

# =====================================================================
# 5c) Garantir diretório de resultados no master
# =====================================================================

log_i "Garantindo diretório de resultados no master: /users/${remote_user}/iss/current-deployment-data/raw-results"
ssh $ssh_options "${remote_user}@${master_ip}" "
  mkdir -p /users/${remote_user}/iss/current-deployment-data/raw-results
" >/dev/null 2>&1 || log_w "Não foi possível criar raw-results no master (prosseguindo)."

echo

# =====================================================================
# 6) Start master
# =====================================================================

log_i "Iniciando master em $master_ip..."

# Passamos explicitamente os parâmetros esperados por start-master.sh
#   $1 = remote_user
#   $2 = master_ip
#   $3 = remote_work_dir
#   $4 = remote_bin_dir
#   $5 = exp_data_dir
#   $6 = caminho local do master-commands.cmd
scripts/start-master.sh \
  "$remote_user" \
  "$master_ip" \
  "$remote_work_dir" \
  "$remote_bin_dir" \
  "$exp_data_dir" \
  "$exp_data_dir/$local_master_command_file" &

log_i "start-master.sh disparado (remote_user=$remote_user, master_ip=$master_ip)."

# =====================================================================
# 7) Start slaves (peers + 1client)
# =====================================================================

log_i "Iniciando slaves com tag=peers..."
# desired_count=0 => inicia todos os nós com a tag especificada
scripts/start-remote-slaves.sh "$exp_data_dir" 0 peers "$instance_info_file"

log_i "Iniciando slaves com tag=1client..."
scripts/start-remote-slaves.sh "$exp_data_dir" 0 1client "$instance_info_file"

log_i "Todos os slaves foram iniciados."

# =====================================================================
# 8) Fetch de resultados em background
# =====================================================================

log_i "Iniciando coleta de resultados em background..."
scripts/fetch-results.sh "$master_ip" "$exp_data_dir" \
  > "$exp_data_dir/$local_result_fetching_log" 2>&1 &

fetch_pid=$!

log_i "Aguardando término do experimento e da coleta de resultados."
log_i "Acompanhe o progresso em:"
log_i "  $exp_data_dir/$local_result_fetching_log"
echo

wait "$fetch_pid" || log_w "fetch-results.sh terminou com código diferente de zero (ver log acima)."

# =====================================================================
# 9) Cancelar instâncias (se configurado)
# =====================================================================

if $cancel_instances; then
  log_i "Encerrando máquinas na nuvem (cancel_instances=true)..."
  scripts/cancel-cloud-instances.sh "$exp_data_dir/$instance_info_file_name"
else
  echo -e "Lembre-se de encerrar as VMs com:\n  cancel-cloud-instances.sh $exp_data_dir/$instance_info_file_name\n"
fi

log_i "deploy-remote.sh finalizado."

