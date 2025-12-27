cd /tmp/ISS_com_Multipaxos/mirbft/deployment

cp -a scripts/start-master.sh scripts/start-master.sh.BAK.$(date +%Y%m%d-%H%M%S) || true

cat > scripts/start-master.sh <<'EOF'
#!/usr/bin/env bash
# scripts/start-master.sh
#
# - inicia o discoverymaster remotamente
# - sincroniza master-commands + tls-data
# - publica configs do experimento em /users/Bruno/iss/experiment-config/
# - FORÇA o discoverymaster no modo: discoverymaster master <addr:port> <cmdfile>

set -euo pipefail

ts() { date +"%Y-%m-%d %H:%M:%S%z"; }
log_i() { echo "[start-master][$(ts)] $*"; }
log_w() { echo "[start-master][$(ts)] WARN: $*" >&2; }
log_e() { echo "[start-master][$(ts)] ERRO: $*" >&2; }

if [[ $# -lt 6 ]]; then
  cat >&2 <<USAGE
Uso:
  $0 <remote_user> <master_ip> <remote_work_dir> <remote_bin_dir> <exp_data_dir> <local_master_cmd>
USAGE
  exit 2
fi

remote_user="$1"
master_ip="$2"
remote_work_dir="$3"
remote_bin_dir="$4"
exp_data_dir="$5"
local_master_cmd="$6"

ssh_options="${ssh_options:--o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -T -o BatchMode=yes -o ConnectTimeout=8 -o ConnectionAttempts=1 -o ServerAliveInterval=5 -o ServerAliveCountMax=2 -o LogLevel=ERROR -o ControlMaster=no -o ControlPath=none -o ControlPersist=no}"
scp_options="${scp_options:--o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o BatchMode=yes -o ConnectTimeout=8 -o ConnectionAttempts=1 -o ServerAliveInterval=5 -o ServerAliveCountMax=2 -o LogLevel=ERROR -o ControlMaster=no -o ControlPath=none -o ControlPersist=no}"

rsh() { ssh $ssh_options "${remote_user}@${1}" "${2}"; }
scp_to_master() { scp $scp_options "${1}" "${remote_user}@${master_ip}:${2}"; }

status_file="${status_file:-${remote_work_dir}/status}"
ready_file="${ready_file:-${remote_work_dir}/master-ready}"
master_port="${master_port:-9999}"

status_exit_file="${status_exit_file:-${status_file}.exit}"
remote_tls_dir="${remote_work_dir}/tls-data"
remote_logs_dir="${remote_work_dir}/logs"
remote_exp_cfg_dir="${remote_work_dir}/experiment-config"

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
rsh "$master_ip" "mkdir -p '${remote_work_dir}' '${remote_tls_dir}' '${remote_logs_dir}' '${remote_exp_cfg_dir}'" || {
  log_e "Falha ao preparar diretórios remotos." | tee -a "$debug_log"
  exit 1
}

# 2) Copia master-commands para o master
scp_to_master "$local_master_cmd" "${remote_work_dir}/master-commands.cmd"

# 2b) Reforça tls-data (opcional)
local_tls_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/tls-data"
if [[ -d "$local_tls_dir" ]]; then
  tar -C "$local_tls_dir" -czf - . | rsh "$master_ip" "tar -C '${remote_tls_dir}' -xzf -" || true
fi

# 2c) Publica configs do experimento em /users/Bruno/iss/experiment-config/
publish_src=""
if [[ -d "${exp_data_dir}/experiment-config" ]]; then
  publish_src="${exp_data_dir}/experiment-config"
elif [[ -d "${exp_data_dir}/config" ]]; then
  publish_src="${exp_data_dir}/config"
fi

if [[ -n "$publish_src" ]]; then
  log_i "Publicando configs: ${publish_src} -> ${remote_exp_cfg_dir}/" | tee -a "$debug_log"
  shopt -s nullglob
  files=( "${publish_src}/"*.yml "${publish_src}/"*.yaml )
  if (( ${#files[@]} > 0 )); then
    scp $scp_options "${files[@]}" "${remote_user}@${master_ip}:${remote_exp_cfg_dir}/" \
      || log_w "Falha ao copiar configs para experiment-config/ (continuando)." | tee -a "$debug_log"
  else
    log_w "Nenhum .yml/.yaml em ${publish_src}; não publiquei configs." | tee -a "$debug_log"
  fi

  rsh "$master_ip" "test -s '${remote_exp_cfg_dir}/config-0000.yml'" \
    && log_i "experiment-config OK no master (config-0000.yml presente)." | tee -a "$debug_log" \
    || log_w "experiment-config sem config-0000.yml (fetch pode falhar)." | tee -a "$debug_log"
else
  log_w "Sem configs em ${exp_data_dir}/experiment-config ou ${exp_data_dir}/config; não publiquei configs." | tee -a "$debug_log"
fi

# 3) Mata instâncias antigas do discoverymaster
rsh "$master_ip" "killall -9 discoverymaster 2>/dev/null || true"

# 4) Inicia discoverymaster remoto sem escape quebrável: bash -s via heredoc
log_i "starting discoverymaster..." | tee -a "$debug_log"

ssh $ssh_options "${remote_user}@${master_ip}" \
  "REMOTE_WORK_DIR=$(printf '%q' "$remote_work_dir") \
   REMOTE_BIN_DIR=$(printf '%q' "$remote_bin_dir") \
   REMOTE_LOGS_DIR=$(printf '%q' "$remote_logs_dir") \
   STATUS_FILE=$(printf '%q' "$status_file") \
   STATUS_EXIT_FILE=$(printf '%q' "$status_exit_file") \
   READY_FILE=$(printf '%q' "$ready_file") \
   MASTER_PORT=$(printf '%q' "$master_port") \
   MASTER_IP=$(printf '%q' "$master_ip") \
   nohup bash -s </dev/null >/dev/null 2>&1 & echo STARTED" \
  | tee -a "$debug_log" \
  <<'REMOTE_WRAPPER'
set -euo pipefail

remote_work_dir="${REMOTE_WORK_DIR}"
remote_bin_dir="${REMOTE_BIN_DIR}"
remote_logs_dir="${REMOTE_LOGS_DIR}"
status_file="${STATUS_FILE}"
status_exit_file="${STATUS_EXIT_FILE}"
ready_file="${READY_FILE}"
master_port="${MASTER_PORT}"
master_ip="${MASTER_IP}"

cmd_file="${remote_work_dir}/master-commands.cmd"
log_file="${remote_logs_dir}/discoverymaster.log"
bin="${remote_bin_dir}/discoverymaster"

mkdir -p "$(dirname "$status_file")" "$(dirname "$status_exit_file")" "$(dirname "$ready_file")" "$(dirname "$log_file")"

cur="$(
  ( test -f "$status_file" && cat "$status_file" || true ) | tr -d "\r" | tail -n 1
)"
if [[ "$cur" != "DONE" && "$cur" != "ANALYZED" ]]; then
  echo RUNNING > "$status_file" 2>/dev/null || true
fi

: > "$status_exit_file" 2>/dev/null || true
echo READY > "$ready_file" 2>/dev/null || true

addr="${master_ip}:${master_port}"
run_cmd=( "$bin" master "$addr" "$cmd_file" )

set +e
"${run_cmd[@]}" >>"$log_file" 2>&1
rc=$?
set -e

echo "EXIT rc=$rc" > "$status_exit_file" 2>/dev/null || true

cur2="$(
  ( test -f "$status_file" && cat "$status_file" || true ) | tr -d "\r" | tail -n 1
)"
if [[ "$cur2" != "DONE" && "$cur2" != "ANALYZED" ]]; then
  echo "EXIT rc=$rc" > "$status_file" 2>/dev/null || true
fi

exit "$rc"
REMOTE_WRAPPER

# 5) Validação leve
sleep 1
if ! rsh "$master_ip" "ss -lntp 2>/dev/null | grep -q \":${master_port} \""; then
  log_w "discoverymaster ainda não aparece escutando ${master_port}. Veja: ${remote_logs_dir}/discoverymaster.log" | tee -a "$debug_log"
else
  log_i "discoverymaster LISTEN on ${master_ip}:${master_port}" | tee -a "$debug_log"
fi

exit 0
EOF

chmod +x scripts/start-master.sh
sed -i 's/\r$//' scripts/start-master.sh

bash -n scripts/start-master.sh

