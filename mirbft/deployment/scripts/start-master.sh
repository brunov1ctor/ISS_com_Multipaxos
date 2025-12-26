#!/usr/bin/env bash
# start-master.sh
# - inicia o discoverymaster remotamente
# - sincroniza master-commands + tls-data
# - semântica:
#     * status_file = estado lógico do experimento (DONE/ANALYZED terminal)
#     * status_exit_file = resultado do processo do discoverymaster (EXIT rc=...)
# - compatível com discoverymaster que usa flags (-addr/-cmd) OU args posicionais.

set -euo pipefail

ts() { date +"%Y-%m-%d %H:%M:%S%z"; }
log_i() { echo "[start-master][$(ts)] $*"; }
log_w() { echo "[start-master][$(ts)] WARN: $*" >&2; }
log_e() { echo "[start-master][$(ts)] ERRO: $*" >&2; }

if [[ $# -lt 6 ]]; then
  cat >&2 <<EOF
Uso:
  $0 <remote_user> <master_ip> <remote_work_dir> <remote_bin_dir> <exp_data_dir> <local_master_cmd>

Exemplo:
  $0 Bruno 172.21.17.1 /users/Bruno/iss /users/Bruno/go/bin /tmp/.../remote-0000 /tmp/.../remote-0000/master-commands.cmd
EOF
  exit 2
fi

remote_user="$1"
master_ip="$2"
remote_work_dir="$3"
remote_bin_dir="$4"
exp_data_dir="$5"
local_master_cmd="$6"

ssh_options="${ssh_options:--o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -T -o BatchMode=yes -o ConnectTimeout=8 -o ConnectionAttempts=1 -o ServerAliveInterval=5 -o ServerAliveCountMax=2 -o LogLevel=ERROR -o ControlMaster=no -o ControlPath=none -o ControlPersist=no}"

rsh() { ssh $ssh_options "${remote_user}@${1}" "${2}"; }
scp_to_master() { scp $ssh_options "${1}" "${remote_user}@${master_ip}:${2}"; }

status_file="${status_file:-${remote_work_dir}/status}"
ready_file="${ready_file:-${remote_work_dir}/master-ready}"
master_port="${master_port:-9999}"

status_exit_file="${status_exit_file:-${status_file}.exit}"
remote_tls_dir="${remote_work_dir}/tls-data"
remote_logs_dir="${remote_work_dir}/logs"

debug_log="${exp_data_dir}/_debug/start-master.${master_ip}.log"
mkdir -p "$(dirname "$debug_log")"

log_i "master_ip=${master_ip} port=${master_port} user=${remote_user}" | tee -a "$debug_log"
log_i "remote_work_dir=${remote_work_dir} remote_bin_dir=${remote_bin_dir}" | tee -a "$debug_log"
log_i "local_master_cmd=${local_master_cmd}" | tee -a "$debug_log"
log_i "status_file=${status_file} status_exit_file=${status_exit_file} ready_file=${ready_file}" | tee -a "$debug_log"

if [[ ! -f "$local_master_cmd" ]]; then
  log_e "master-commands não encontrado: $local_master_cmd" | tee -a "$debug_log"
  exit 1
fi

# 1) Layout remoto mínimo
rsh "$master_ip" "mkdir -p '${remote_work_dir}' '${remote_tls_dir}' '${remote_logs_dir}'" || {
  log_e "Falha ao preparar diretórios remotos." | tee -a "$debug_log"
  exit 1
}

# 2) Copia master-commands para o remote_work_dir
scp_to_master "$local_master_cmd" "${remote_work_dir}/master-commands.cmd"

# 2b) Reforça tls-data (opcional)
if [[ -d "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/tls-data" ]]; then
  local_tls_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/tls-data"
  tar -C "$local_tls_dir" -czf - . | rsh "$master_ip" "tar -C '${remote_tls_dir}' -xzf -" || true
fi

# 3) Mata instâncias antigas do discoverymaster
rsh "$master_ip" "killall -9 discoverymaster 2>/dev/null || true"

# 4) Inicia discoverymaster com wrapper robusto
log_i "starting discoverymaster..." | tee -a "$debug_log"

rsh "$master_ip" "bash -lc '
set -euo pipefail

remote_work_dir=\"${remote_work_dir}\"
remote_bin_dir=\"${remote_bin_dir}\"
remote_logs_dir=\"${remote_logs_dir}\"
status_file=\"${status_file}\"
status_exit_file=\"${status_exit_file}\"
ready_file=\"${ready_file}\"
master_port=\"${master_port}\"

cmd_file=\"${remote_work_dir}/master-commands.cmd\"
log_file=\"${remote_logs_dir}/discoverymaster.log\"
bin=\"${remote_bin_dir}/discoverymaster\"

mkdir -p \"\$(dirname \"\$status_file\")\" \"\$(dirname \"\$status_exit_file\")\" \"\$(dirname \"\$ready_file\")\" \"\$(dirname \"\$log_file\")\"

# Se status já estiver terminal, preserva.
cur=\"\$(
  ( test -f \"\$status_file\" && cat \"\$status_file\" || true ) | tr -d \"\r\" | tail -n 1
)\"
if [[ \"\$cur\" != \"DONE\" && \"\$cur\" != \"ANALYZED\" ]]; then
  echo RUNNING > \"\$status_file\" 2>/dev/null || true
fi

: > \"\$status_exit_file\" 2>/dev/null || true
echo READY > \"\$ready_file\" 2>/dev/null || true

# Detecta CLI do discoverymaster:
# - se --help mencionar \"-addr\" e \"-cmd\" => usa flags
# - caso contrário => usa args posicionais: <addr> <cmdfile>
help_out=\"\"
if \"\$bin\" --help >/tmp/dm.help 2>&1; then
  help_out=\"\$(cat /tmp/dm.help || true)\"
else
  help_out=\"\$(cat /tmp/dm.help || true)\"
fi

addr=\"0.0.0.0:\$master_port\"

run_cmd=()
if echo \"\$help_out\" | grep -q \"-addr\" && echo \"\$help_out\" | grep -q \"-cmd\"; then
  run_cmd=(\"\$bin\" -addr \"\$addr\" -cmd \"\$cmd_file\")
else
  run_cmd=(\"\$bin\" \"\$addr\" \"\$cmd_file\")
fi

# Executa sem deixar -e matar o wrapper antes de capturar rc
set +e
\"\${run_cmd[@]}\" >>\"\$log_file\" 2>&1
rc=\$?
set -e

echo \"EXIT rc=\$rc\" > \"\$status_exit_file\" 2>/dev/null || true

cur2=\"\$(
  ( test -f \"\$status_file\" && cat \"\$status_file\" || true ) | tr -d \"\r\" | tail -n 1
)\"
if [[ \"\$cur2\" != \"DONE\" && \"\$cur2\" != \"ANALYZED\" ]]; then
  echo \"EXIT rc=\$rc\" > \"\$status_file\" 2>/dev/null || true
fi

exit \$rc
' >/dev/null 2>&1 & echo STARTED" | tee -a "$debug_log"

# 5) Validação leve (não aborta aqui; deploy-remote valida de verdade)
sleep 1
if ! rsh "$master_ip" "ss -lntp 2>/dev/null | grep -q \":${master_port} \""; then
  log_w "discoverymaster ainda não aparece escutando ${master_port}. Veja: ${remote_logs_dir}/discoverymaster.log" | tee -a "$debug_log"
else
  log_i "discoverymaster LISTEN on ${master_ip}:${master_port}" | tee -a "$debug_log"
fi

exit 0

