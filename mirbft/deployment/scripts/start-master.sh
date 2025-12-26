#!/usr/bin/env bash
# start-master.sh
# - inicia o discoverymaster remotamente
# - sincroniza master-commands + tls-data + (FIX) experiment-config
# - semântica:
#     * status_file = estado lógico do experimento (DONE/ANALYZED terminal)
#     * status_exit_file = resultado do processo do discoverymaster (EXIT rc=...)
# - compatível com discoverymaster que suporta:
#     A) "master addr:port cmdfile"  (preferido neste repo)
#     B) legacy: "<port> file <cmdfile>"
#     C) (fallback) flags -addr/-cmd (caso exista em outro commit)

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

# scp não entende -T; então usamos um conjunto compatível se quiser sobrescrever externamente.
scp_options="${scp_options:--o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o BatchMode=yes -o ConnectTimeout=8 -o ConnectionAttempts=1 -o ServerAliveInterval=5 -o ServerAliveCountMax=2 -o LogLevel=ERROR -o ControlMaster=no -o ControlPath=none -o ControlPersist=no}"

rsh() { ssh $ssh_options "${remote_user}@${1}" "${2}"; }
scp_to_master() { scp $scp_options "${1}" "${remote_user}@${master_ip}:${2}"; }

status_file="${status_file:-${remote_work_dir}/status}"
ready_file="${ready_file:-${remote_work_dir}/master-ready}"
master_port="${master_port:-9999}"

status_exit_file="${status_exit_file:-${status_file}.exit}"
remote_tls_dir="${remote_work_dir}/tls-data"
remote_logs_dir="${remote_work_dir}/logs"
remote_experiment_config_dir="${remote_work_dir}/experiment-config"

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
rsh "$master_ip" "mkdir -p '${remote_work_dir}' '${remote_tls_dir}' '${remote_logs_dir}' '${remote_experiment_config_dir}'" || {
  log_e "Falha ao preparar diretórios remotos." | tee -a "$debug_log"
  exit 1
}

# 2) Copia master-commands para o remote_work_dir
scp_to_master "$local_master_cmd" "${remote_work_dir}/master-commands.cmd"

# 2b) Reforça tls-data (opcional)
local_tls_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/tls-data"
if [[ -d "$local_tls_dir" ]]; then
  tar -C "$local_tls_dir" -czf - . | rsh "$master_ip" "tar -C '${remote_tls_dir}' -xzf -" || true
fi

# 2c) (FIX) Publicar experiment-config no master ANTES de iniciar discoverymaster
log_i "Publicando experiment-config/ no master..." | tee -a "$debug_log"

if [[ ! -d "${exp_data_dir}/experiment-config" ]]; then
  log_e "Diretório local ausente: ${exp_data_dir}/experiment-config" | tee -a "$debug_log"
  exit 1
fi

# copia arquivos (não o diretório) para manter layout /users/Bruno/iss/experiment-config/*.yml
(
  shopt -s nullglob
  files=( "${exp_data_dir}/experiment-config/"* )
  if (( ${#files[@]} == 0 )); then
    log_e "Nenhum arquivo em ${exp_data_dir}/experiment-config (nada para publicar)." | tee -a "$debug_log"
    exit 1
  fi

  rsh "$master_ip" "mkdir -p '${remote_experiment_config_dir}'" || true

  # scp dos arquivos
  scp $scp_options -r "${files[@]}" "${remote_user}@${master_ip}:${remote_experiment_config_dir}/" \
    || { log_e "Falha ao copiar experiment-config/ para o master." | tee -a "$debug_log"; exit 1; }
)

# sanity check forte (o mesmo caminho que o stubborn-scp usa)
if ! rsh "$master_ip" "test -s '${remote_experiment_config_dir}/config-0000.yml'"; then
  log_e "Master NÃO tem ${remote_experiment_config_dir}/config-0000.yml após publish." | tee -a "$debug_log"
  rsh "$master_ip" "ls -lah '${remote_experiment_config_dir}' || true" || true
  exit 1
fi

log_i "experiment-config OK no master." | tee -a "$debug_log"

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

addr=\"0.0.0.0:\$master_port\"
port=\"\$master_port\"

# Descobre qual CLI existe neste binário.
help_out=\"\"
if \"\$bin\" --help >/tmp/dm.help 2>&1; then
  help_out=\"\$(cat /tmp/dm.help || true)\"
else
  help_out=\"\$(cat /tmp/dm.help || true)\"
fi

run_cmd=()

# Preferido neste repo: modo \"master\"
if echo \"\$help_out\" | grep -q \"discoverymaster master\"; then
  run_cmd=(\"\$bin\" master \"\$addr\" \"\$cmd_file\")
# Fallback: flags (se algum commit antigo usar isso)
elif echo \"\$help_out\" | grep -q \"-addr\" && echo \"\$help_out\" | grep -q \"-cmd\"; then
  run_cmd=(\"\$bin\" -addr \"\$addr\" -cmd \"\$cmd_file\")
# Último fallback: modo legado (porta pura + file)
else
  run_cmd=(\"\$bin\" \"\$port\" file \"\$cmd_file\")
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

# 5) Validação leve (o deploy-remote valida de verdade)
sleep 1
if ! rsh "$master_ip" "ss -lntp 2>/dev/null | grep -q \":${master_port} \""; then
  log_w "discoverymaster ainda não aparece escutando ${master_port}. Veja: ${remote_logs_dir}/discoverymaster.log" | tee -a "$debug_log"
else
  log_i "discoverymaster LISTEN on ${master_ip}:${master_port}" | tee -a "$debug_log"
fi

exit 0

