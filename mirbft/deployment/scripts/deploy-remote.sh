#!/usr/bin/env bash

# deploy-remote.sh (versão com DEPLOY_DIR robusto + chamada python3)

set -e

ts() { date +"%Y-%m-%d %H:%M:%S%z"; }
log_i() { echo "[INFO  ][$(ts)] $*"; }
log_w() { echo "[WARN  ][$(ts)] $*" >&2; }
log_e() { echo "[ERRO  ][$(ts)] $*" >&2; }

# Flag de cancelamento de instâncias (padrão: false)
: "${cancel_instances:=false}"

# Binário de Python a usar para scripts auxiliares
: "${PYTHON:=python3}"

# =====================================================================
# 0) Descobrir DEPLOY_DIR de forma robusta
# =====================================================================
# Se DEPLOY_DIR vier do ambiente (global-vars.sh / deploy.sh), usa ele.
# Caso contrário, usa o diretório pai deste script como fallback.
: "${DEPLOY_DIR:=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"

# =====================================================================
# 1) Variáveis esperadas do ambiente (de deploy.sh + global-vars.sh)
# =====================================================================
#
# Este script pode ser chamado de duas formas:
#   (a) via 'source' a partir de deploy.sh, que exporta as variáveis abaixo; ou
#   (b) diretamente como binário/shell, passando parâmetros:
#       deploy-remote.sh <exp_data_dir> <instance_info_file> [deployment_data_root] [dpl_filename] [csv_filename]
#
# Para tornar o script mais robusto, se as variáveis de ambiente vierem vazias
# mas existirem argumentos posicionais, usamos esses argumentos como fallback.

if [[ -z "${exp_data_dir:-}" && "$#" -ge 1 ]]; then
  exp_data_dir="$1"
fi

if [[ -z "${instance_info_file:-}" && "$#" -ge 2 ]]; then
  instance_info_file="$2"
fi

if [[ -z "${deployment_data_root:-}" && "$#" -ge 3 ]]; then
  deployment_data_root="$3"
fi

if [[ -z "${dpl_filename:-}" && "$#" -ge 4 ]]; then
  dpl_filename="$4"
fi

if [[ -z "${csv_filename:-}" && "$#" -ge 5 ]]; then
  csv_filename="$5"
fi

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

remote_user="${remote_user:-${USER:-unknown}}"
remote_work_dir="${remote_work_dir:-/users/${remote_user}/iss}"
remote_bin_dir="${remote_bin_dir:-/users/${remote_user}/go/bin}"
ssh_options="${ssh_options:--o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -T -o BatchMode=yes -o ConnectTimeout=8 -o ConnectionAttempts=1 -o ServerAliveInterval=5 -o ServerAliveCountMax=2 -o LogLevel=ERROR -o ControlMaster=no -o ControlPath=none -o ControlPersist=no}"
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
# 3) Garantir master-commands-template.cmd e deployment.dpl
# =====================================================================

template_path="$exp_data_dir/$local_master_command_template_file"
deployment_file="$exp_data_dir/${dpl_filename:-deployment.dpl}"

if [ ! -f "$deployment_file" ]; then
  # fallback clássico: arquivo padrão
  deployment_file="$exp_data_dir/deployment.dpl"
fi

if [ ! -f "$deployment_file" ]; then
  log_e "Arquivo deployment.dpl não encontrado em $exp_data_dir."
  exit 1
fi

if [ ! -f "$template_path" ]; then
  log_i "Gerando master-commands-template.cmd..."
  # ✅ CORREÇÃO: passa o tipo de deploy como primeiro argumento ("remote")
  if ! "$PYTHON" "$DEPLOY_DIR/scripts/generate-master-commands.py" remote "$deployment_file" "$template_path" "$exp_data_dir"; then
    log_e "Falha ao gerar master-commands-template.cmd."
    exit 1
  fi
fi

# =====================================================================
# 4) Gerar master-commands.cmd
# =====================================================================

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
  ssh $ssh_options "${remote_user}@${ip}" "\
    rm -rf '${remote_work_dir}/current-deployment-data' \
           '${remote_work_dir}/experiment-config' \
           '${remote_work_dir}/status' \
           '${remote_work_dir}/master-ready'; \
    pkill -9 -f '${remote_bin_dir}/discoverymaster' 2>/dev/null || true; \
    pkill -9 -f '${remote_bin_dir}/discoveryslave' 2>/dev/null || true; \
    pkill -9 -f '${remote_bin_dir}/orderingpeer' 2>/dev/null || true; \
    pkill -9 -f '${remote_bin_dir}/orderingclient' 2>/dev/null || true; \
  " </dev/null || log_w "$ip: não foi possível limpar estado remoto."
done

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

"$DEPLOY_DIR/scripts/start-master.sh" \
  "$master_ip" \
  "$exp_data_dir/$instance_info_file_name" \
  "$exp_data_dir/$local_master_command_file" \
  "$remote_work_dir" \
  "$remote_bin_dir"

# =====================================================================
# 7) Start slaves (peers + 1client)
# =====================================================================

log_i "Iniciando slaves (peers)..."
"$DEPLOY_DIR/scripts/start-remote-slaves.sh" "$exp_data_dir" 0 peers "$instance_info_file"

log_i "Iniciando slaves (1client)..."
"$DEPLOY_DIR/scripts/start-remote-slaves.sh" "$exp_data_dir" 0 1client "$instance_info_file"

log_i "Slaves iniciados."

# =====================================================================
# 8) Coleta de resultados
# =====================================================================

log_i "Iniciando coleta de resultados..."
"$DEPLOY_DIR/scripts/fetch-results.sh" "$master_ip" "$exp_data_dir" "$local_result_fetching_log"

log_i "Coleta de resultados concluída."

# =====================================================================
# 9) Cancelar instâncias (se configurado)
# =====================================================================

if $cancel_instances; then
  log_i "Cancelando máquinas na nuvem..."
  "$DEPLOY_DIR/scripts/cancel-cloud-instances.sh" "$exp_data_dir/$instance_info_file_name"
else
  echo -e "Lembre-se de cancelar as VMs com:\n  cancel-cloud-instances.sh $exp_data_dir/$instance_info_file_name\n"
fi

log_i "deploy-remote.sh concluído."

