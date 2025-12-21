#!/usr/bin/env bash

# deploy-remote.sh (versão corrigida)

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
  log_e "deploy-remote.sh: exp_data_dir ou instance_info_file vazio."
  log_e "  exp_data_dir='${exp_data_dir:-}' instance_info_file='${instance_info_file:-}'"
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
ssh_options="${ssh_options:--o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ControlMaster=no -o ControlPath=none -o ControlPersist=no}"
remote_status_file="${remote_status_file:-${remote_work_dir}/status}"

# ---------------------------------------------------------------------
# Compatibilidade de variável de chave SSH
# ---------------------------------------------------------------------
# Alguns ambientes antigos podem passar 'remote_priv_key_file'.
# Normalizamos para 'remote_private_key_file' e garantimos que exista
# pelo menos como string vazia, para não quebrar com 'set -u'.

if [[ -n "${remote_priv_key_file:-}" && -z "${remote_private_key_file:-}" ]]; then
  remote_private_key_file="$remote_priv_key_file"
fi

: "${remote_private_key_file:=}"

log_i "SSH config:"
log_i "  remote_private_key_file = ${remote_private_key_file:-<none>}"
log_i "  ssh_options             = ${ssh_options:-<none>}"

# =====================================================================
# 2) Descobrir IP do master e copiar instance-info para o diretório do experimento
# =====================================================================

master_ip=$(awk '$4 == "master" {print $2}' "$instance_info_file" | head -n1)

if [ -z "$master_ip" ]; then
  log_e "deploy-remote.sh: could not obtain master ip from instance info file: $instance_info_file"
  exit 1
fi

cp "$instance_info_file" "$exp_data_dir/$instance_info_file_name"

log_i "Using instance info file: $instance_info_file"
log_i "Master IP address      : $master_ip"

# =====================================================================
# 3) Garantir que existe um master-commands-template.cmd
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
# 5) Reset remoto: matar processos antigos + limpar estado
# =====================================================================

log_i "Resetando estado das máquinas remotas (kill + prune + cleanup)..."

ssh $ssh_options "${remote_user}@${master_ip}" "
  set -e
  echo '[MASTER RESET] Killing old processes and cleaning state...'
  if [ -d /users/${remote_user}/iss ]; then
    pkill -u ${remote_user} discoverymaster || true
    pkill -u ${remote_user} discoveryslave || true
    pkill -u ${remote_user} orderingpeer || true
    pkill -u ${remote_user} orderingclient || true
    rm -rf /users/${remote_user}/iss/current-deployment-data || true
  fi
  mkdir -p /users/${remote_user}/iss/current-deployment-data
" </dev/null

log_i "Reset machine state."
echo

# =====================================================================
# 5b) Garantir diretório de resultados no master
# =====================================================================

log_i "Ensuring raw-results directory exists on master at /users/${remote_user}/iss/current-deployment-data/raw-results ..."
ssh $ssh_options "${remote_user}@${master_ip}" "
  mkdir -p /users/${remote_user}/iss/current-deployment-data/raw-results
" >/dev/null 2>&1 || log_w "Could not create raw-results dir on master (continuando)."

echo

# =====================================================================
# 6) Copiar arquivos de configuração e comandos para o master
# =====================================================================

log_i "Copiando arquivos de config e comandos para o master..."

scp $ssh_options "$exp_data_dir/$local_master_command_file" \
  "${remote_user}@${master_ip}:/users/${remote_user}/iss/current-deployment-data/master-commands.cmd"

scp $ssh_options "$exp_data_dir/$instance_info_file_name" \
  "${remote_user}@${master_ip}:/users/${remote_user}/iss/current-deployment-data/$instance_info_file_name"

scp $ssh_options "$exp_data_dir/deployment.dpl" \
  "${remote_user}@${master_ip}:/users/${remote_user}/iss/current-deployment-data/deployment.dpl"

log_i "Arquivos copiados para o master."
echo

# =====================================================================
# 7) Copiar deployment-data (config, tls-data, etc.) para o master
# =====================================================================

log_i "Copiando deployment-data do experimento para o master..."

ssh $ssh_options "${remote_user}@${master_ip}" "
  mkdir -p /users/${remote_user}/iss/current-deployment-data/experiment-config
  mkdir -p /users/${remote_user}/iss/current-deployment-data/tls-data
" </dev/null

scp $ssh_options "$exp_data_dir"/experiment-config/* \
  "${remote_user}@${master_ip}:/users/${remote_user}/iss/current-deployment-data/experiment-config/"

if [ -d "$deployment_data_root/tls-data" ]; then
  scp $ssh_options "$deployment_data_root"/tls-data/* \
    "${remote_user}@${master_ip}:/users/${remote_user}/iss/current-deployment-data/tls-data/" || true
fi

log_i "Deployment-data enviado ao master."
echo

# =====================================================================
# 8) Copiar scripts necessários para o master
# =====================================================================

log_i "Copiando scripts principais para o master..."

scp $ssh_options scripts/start-remote-slaves.sh \
  "${remote_user}@${master_ip}:/users/${remote_user}/iss/current-deployment-data/start-remote-slaves.sh"

scp $ssh_options scripts/start-slave.sh \
  "${remote_user}@${master_ip}:/users/${remote_user}/iss/current-deployment-data/start-slave.sh"

scp $ssh_options scripts/stubborn-scp.sh \
  "${remote_user}@${master_ip}:/users/${remote_user}/iss/current-deployment-data/stubborn-scp.sh"

scp $ssh_options scripts/fetch-results.sh \
  "${remote_user}@${master_ip}:/users/${remote_user}/iss/current-deployment-data/fetch-results.sh"

log_i "Scripts copiados."
echo

# =====================================================================
# 9) Copiar binários locais para o master (para redistribuição aos slaves)
# =====================================================================

log_i "Copiando binários locais para o master (para redistribuição)..."

# Descobrimos o diretório de binários local exatamente como em ensure_local_binaries
local_bin_dir=""
if [[ -n "${GOBIN:-}" ]]; then
  local_bin_dir="$GOBIN"
else
  local_bin_dir="$(go env GOBIN || true)"
  if [[ -z "$local_bin_dir" ]]; then
    local_bin_dir="$(go env GOPATH 2>/dev/null)/bin"
    if [[ -z "$local_bin_dir" ]]; then
      local_bin_dir="${HOME}/go/bin"
    fi
  fi
fi

log_i "Diretório de binários local: $local_bin_dir"

for bin in discoverymaster discoveryslave orderingpeer orderingclient; do
  local_path="${local_bin_dir}/${bin}"
  if [[ ! -x "$local_path" ]]; then
    log_w "Binário não encontrado localmente (será ignorado): $local_path"
    continue
  fi
  log_i "Enviando binário $bin para o master..."
  scp $ssh_options "$local_path" \
    "${remote_user}@${master_ip}:/users/${remote_user}/iss/current-deployment-data/${bin}" || \
    log_w "Falha ao enviar binário $bin para o master (continuando)."
done

echo

# =====================================================================
# 10) Executar master-commands no master
# =====================================================================

log_i "Executando master-commands.cmd no master..."

ssh $ssh_options "${remote_user}@${master_ip}" "
  set -e
  cd /users/${remote_user}/iss/current-deployment-data
  chmod +x start-remote-slaves.sh start-slave.sh stubborn-scp.sh fetch-results.sh || true
  # Descobre caminho de ssh privado (se existir) a partir de remote_private_key_file
  ssh_key_arg=''
  if [ -n \"$remote_private_key_file\" ] && [ -f \"$remote_private_key_file\" ]; then
    ssh_key_arg=\"-i $remote_private_key_file\"
  fi
  echo \"[MASTER] Iniciando workers remotos...\"
  ./start-remote-slaves.sh \"$instance_info_file_name\" > start-remote-slaves.log 2>&1
  echo \"[MASTER] Rodando master-commands via mir-deploy-master...\"
  mir-deploy-master master-commands.cmd > master-commands.log 2>&1 || true
" </dev/null

log_i "master-commands.cmd executado (ou tentou)."
echo

# =====================================================================
# 11) Buscar resultados do experimento
# =====================================================================

log_i "Buscando resultados do experimento a partir do master..."

ssh $ssh_options "${remote_user}@${master_ip}" "
  set -e
  cd /users/${remote_user}/iss/current-deployment-data
  ./fetch-results.sh \"$instance_info_file_name\" > \"$local_result_fetching_log\" 2>&1 || true
" </dev/null

log_i "fetch-results.sh executado no master (ver $local_result_fetching_log no diretório do experimento)."
echo

# =====================================================================
# 12) Mensagem final
# =====================================================================

log_i "deploy-remote.sh finalizado. Verifique:"
log_i "  - $exp_data_dir/$local_result_fetching_log"
log_i "  - $exp_data_dir for experiment-output e raw-results"
log_i "  - _debug/master-diag.txt, se existir"

