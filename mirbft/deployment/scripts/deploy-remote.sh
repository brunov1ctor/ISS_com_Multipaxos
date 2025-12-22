#!/usr/bin/env bash
set -euo pipefail

# ============================================================================
# deploy-remote.sh (versão enxuta de logs)
# ============================================================================
# Uso:
#   ./deploy-remote.sh <exp_data_dir> <instance_info_file> <data_root_dir>
#
# Exemplo:
#   ./deploy-remote.sh \
#       deployment-data/remote-0000 \
#       scripts/instance-info \
#       deployment-data
#
# Responsabilidades:
#   1. Resetar estado nas máquinas remotas.
#   2. Garantir diretórios de resultados no master.
#   3. Subir o master (discoverymaster + master-commands.cmd).
#   4. Subir slaves (peers e clients) via start-remote-slaves.sh.
#   5. Iniciar coleta de resultados.
#
# Objetivo desta versão:
#   - Manter toda a lógica original.
#   - Reduzir verborragia de log, principalmente na parte inicial (reset).
#   - Produzir um log final resumido e legível.
# ============================================================================

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

# ----------------------------------------------------------------------------
# Funções auxiliares de log
# ----------------------------------------------------------------------------
ts() {
  date '+%Y-%m-%d %H:%M:%S-%z'
}

log_i() {
  echo "[INFO  ][$(ts)] $*"
}

log_w() {
  echo "[WARN  ][$(ts)] $*" >&2
}

log_e() {
  echo "[ERROR ][$(ts)] $*" >&2
}

# ----------------------------------------------------------------------------
# Validação de argumentos
# ----------------------------------------------------------------------------
if [[ $# -ne 3 ]]; then
  log_e "Uso: $0 <exp_data_dir> <instance_info_file> <data_root_dir>"
  exit 1
fi

exp_data_dir="$1"
instance_info_file="$2"
data_root_dir="$3"

if [[ ! -d "$exp_data_dir" ]]; then
  log_e "Diretório de experimento não encontrado: $exp_data_dir"
  exit 1
fi

if [[ ! -f "$instance_info_file" ]]; then
  log_e "Arquivo instance-info não encontrado: $instance_info_file"
  exit 1
fi

if [[ ! -d "$data_root_dir" ]]; then
  log_e "Diretório data_root_dir não encontrado: $data_root_dir"
  exit 1
fi

# ----------------------------------------------------------------------------
# Lê master IP do instance-info
# ----------------------------------------------------------------------------
master_ip="$(awk '/^master/ {print $2; exit}' "$instance_info_file")"
if [[ -z "$master_ip" ]]; then
  log_e "Não foi possível detectar master_ip em $instance_info_file"
  exit 1
fi

log_i "Master: $master_ip (instance-info: $instance_info_file)"

# ----------------------------------------------------------------------------
# Caminhos remotos e locais
# ----------------------------------------------------------------------------
remote_user="${USER}"
remote_work_dir="/users/${remote_user}/iss"
remote_bin_dir="/users/${remote_user}/go/bin"
remote_exp_dir="${remote_work_dir}/current-deployment-data"
local_bin_dir="${HOME}/go/bin"

master_commands="${exp_data_dir}/master-commands.cmd"

# ----------------------------------------------------------------------------
# Reset de estado nas máquinas remotas
# ----------------------------------------------------------------------------
log_i "Resetando estado nas máquinas remotas..."

# Mata analyze-continuously (se estiver rodando) com log agregado
failed_analyze_hosts=()
for ip in $(awk '{print $2}' "$instance_info_file"); do
  if ! ssh $ssh_options "${remote_user}@${ip}" \
    "kill -9 \$(ps -ef | grep 'analyze-continuously' | grep -v \$\$ | awk '{print \$2}')" \
    >/dev/null 2>&1; then
    failed_analyze_hosts+=("$ip")
  fi
  sleep 0.1
done
if [ ${#failed_analyze_hosts[@]} -gt 0 ]; then
  log_w "Não foi possível encerrar 'analyze-continuously' em ${#failed_analyze_hosts[@]} máquina(s): ${failed_analyze_hosts[*]}"
fi

# Limpa estado, mata binários velhos, reseta status com log agregado
failed_reset_hosts=()
for ip in $(awk '{print $2}' "$instance_info_file"); do
  remote_delete_files="$remote_work_dir"
  remote_status_file="$remote_work_dir/status"

  if ! ssh $ssh_options "${remote_user}@${ip}" "
    tc qdisc del dev eth0 root tbf rate 1gbit burst 320kbit latency 400ms 2>/dev/null || true
    killall -9 discoverymaster discoveryslave orderingpeer orderingclient scp rsync 2>/dev/null || true
    rm -rf $remote_delete_files
    echo RUNNING > $remote_status_file
    kill -9 \$(ps -ef | grep 'sshd: ${remote_user}@notty' | awk '{print \$2}') 2>/dev/null || true
  " >/dev/null 2>&1; then
    failed_reset_hosts+=("$ip")
  fi
  sleep 0.1
done
wait
if [ ${#failed_reset_hosts[@]} -gt 0 ]; then
  log_w "Falha no reset remoto em ${#failed_reset_hosts[@]} máquina(s): ${failed_reset_hosts[*]}"
fi

log_i "Reset remoto concluído."

# ----------------------------------------------------------------------------
# Garante diretórios de resultados no master
# ----------------------------------------------------------------------------
log_i "Garantindo diretório de resultados no master..."
ssh $ssh_options "${remote_user}@${master_ip}" "
  mkdir -p '${remote_exp_dir}/raw-results'
  mkdir -p '${remote_exp_dir}/logs'
" >/dev/null 2>&1
log_i "Diretórios de resultados garantidos no master."

# ----------------------------------------------------------------------------
# Start do master
# ----------------------------------------------------------------------------
log_i "Iniciando master em ${master_ip}..."
debug_log="${exp_data_dir}/_debug/start-master.${master_ip}.log"
mkdir -p "$(dirname "$debug_log")"

(
  echo "[start-master][$(ts)] remote_user=${remote_user}"
  echo "[start-master][$(ts)] master_ip=${master_ip}"
  echo "[start-master][$(ts)] ssh_options=${ssh_options}"
  echo "[start-master][$(ts)] remote_work_dir=${remote_work_dir}"
  echo "[start-master][$(ts)] remote_bin_dir=${remote_bin_dir}"
  echo "[start-master][$(ts)] exp_data_dir=${exp_data_dir}"
  echo "[start-master][$(ts)] DISCOVERY_PORT=9999"
  echo "[start-master][$(ts)] local_master_cmd=${master_commands}"
  echo "[start-master][$(ts)] debug_log=${debug_log}"

  "${SCRIPT_DIR}/start-master.sh" \
    "$master_ip" \
    "$master_commands" \
    "$remote_user" \
    "$remote_work_dir" \
    "$remote_bin_dir" \
    "$exp_data_dir" \
    "$instance_info_file" \
    >"${debug_log}" 2>&1 &
) >/dev/null 2>&1 &

log_i "start-master.sh em background."

# ----------------------------------------------------------------------------
# Start dos slaves (peers)
# ----------------------------------------------------------------------------
log_i "Iniciando slaves (peers)..."
"${SCRIPT_DIR}/start-remote-slaves.sh" \
  "$exp_data_dir" \
  "$instance_info_file" \
  "peers" \
  "$remote_user" \
  "$remote_work_dir" \
  "$remote_bin_dir" \
  "$remote_exp_dir" \
  "$local_bin_dir"

# ----------------------------------------------------------------------------
# Start dos slaves (1client)
# ----------------------------------------------------------------------------
log_i "Iniciando slaves (1client)..."
"${SCRIPT_DIR}/start-remote-slaves.sh" \
  "$exp_data_dir" \
  "$instance_info_file" \
  "1client" \
  "$remote_user" \
  "$remote_work_dir" \
  "$remote_bin_dir" \
  "$remote_exp_dir" \
  "$local_bin_dir"

log_i "Slaves iniciados."

# ----------------------------------------------------------------------------
# Coleta de resultados
# ----------------------------------------------------------------------------
log_i "Iniciando coleta de resultados..."
fetch_log="${exp_data_dir}/result-fetching.log"
"${SCRIPT_DIR}/fetch-results.sh" \
  "$exp_data_dir" \
  "$instance_info_file" \
  "$data_root_dir" \
  >"$fetch_log" 2>&1 &

log_i "Log de coleta: $fetch_log"
log_i "Lembre-se de cancelar as VMs com:"
echo "  cancel-cloud-instances.sh ${exp_data_dir}/instance-info"

