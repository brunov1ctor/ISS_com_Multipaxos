#!/usr/bin/env bash
set -euo pipefail

ts() { date +"%Y-%m-%d %H:%M:%S%z"; }
log_i() { echo "[start-master][$(ts)] $*"; }
log_e() { echo "[start-master][$(ts)][ERRO] $*" >&2; }

# -----------------------------------------------------------------------------
# ASSINATURA (precisa bater com deploy-remote.sh):
#   1: remote_user
#   2: master_ip
#   3: remote_work_dir          (pesado, /tmp/iss-<user>)
#   4: remote_bin_dir           (/users/<user>/go/bin)
#   5: exp_data_dir (LOCAL)     (deployment-data/remote-000X)
#   6: local_master_cmd (LOCAL) (deployment-data/remote-000X/master-commands.cmd)
# -----------------------------------------------------------------------------
remote_user="${1:?remote_user requerido}"
master_ip="${2:?master_ip requerido}"
remote_work_dir="${3:?remote_work_dir requerido}"
remote_bin_dir="${4:?remote_bin_dir requerido}"
exp_data_dir="${5:?exp_data_dir local requerido}"
local_master_cmd="${6:?local_master_cmd local requerido}"

# Base estável (configs/TLS) no remoto
remote_base_dir="/users/${remote_user}/iss"
remote_config_dir="${remote_base_dir}/experiment-config"

master_port="${master_port:-9999}"

status_file="${remote_work_dir}/status"
ready_file="${remote_work_dir}/master-ready"
log_dir="${remote_work_dir}/logs"
log_file="${log_dir}/discoverymaster.log"
cmd_file="${remote_work_dir}/master-commands.cmd"

# -----------------------------------------------------------------------------
# Validar caminhos locais
# -----------------------------------------------------------------------------
if [[ ! -d "${exp_data_dir}" ]]; then
  log_e "Local exp_data_dir does not exist: ${exp_data_dir}"
  exit 1
fi

if [[ ! -f "${local_master_cmd}" ]]; then
  log_e "Local master cmd not found: ${local_master_cmd}"
  exit 1
fi

if [[ ! -d "${exp_data_dir}/config" ]]; then
  log_e "Local config dir not found: ${exp_data_dir}/config"
  exit 1
fi

# -----------------------------------------------------------------------------
# Contexto
# -----------------------------------------------------------------------------
log_i "master_ip=${master_ip} port=${master_port} user=${remote_user}"
log_i "remote_work_dir=${remote_work_dir} remote_bin_dir=${remote_bin_dir}"
log_i "remote_base_dir=${remote_base_dir} remote_config_dir=${remote_config_dir}"
log_i "local_master_cmd=${local_master_cmd}"
log_i "status_file=${status_file} ready_file=${ready_file}"

# -----------------------------------------------------------------------------
# Preparar diretórios remotos (work + base)
# -----------------------------------------------------------------------------
ssh "${remote_user}@${master_ip}" "mkdir -p '${remote_work_dir}' '${log_dir}' '${remote_base_dir}/config' '${remote_base_dir}/tls-data' '${remote_config_dir}'"

# -----------------------------------------------------------------------------
# Copiar master-commands.cmd para o work dir do master
# -----------------------------------------------------------------------------
log_i "Enviando master-commands.cmd para o master..."
rsync -az "${local_master_cmd}" "${remote_user}@${master_ip}:${cmd_file}"

# -----------------------------------------------------------------------------
# Publicar configs do experimento no master (local config -> /users/<user>/iss/experiment-config/)
# -----------------------------------------------------------------------------
log_i "Publicando configs do experimento no master: ${exp_data_dir}/config -> ${remote_config_dir}/"
rsync -az "${exp_data_dir}/config/" "${remote_user}@${master_ip}:${remote_config_dir}/"

# Validar presença de um config esperado
ssh "${remote_user}@${master_ip}" "test -s '${remote_config_dir}/config-0000.yml'" \
  || log_e "WARN: config-0000.yml não encontrado em ${remote_config_dir} (mas continuando)"

# -----------------------------------------------------------------------------
# Iniciar discoverymaster no master (modo master)
# -----------------------------------------------------------------------------
log_i "starting discoverymaster..."
ssh "${remote_user}@${master_ip}" "bash -lc '
set -euo pipefail

bin=\"${remote_bin_dir}/discoverymaster\"
test -x \"\$bin\" || { echo \"bin not found: \$bin\" >&2; exit 1; }

mkdir -p \"$(dirname "${status_file}")\" \"$(dirname "${ready_file}")\" \"$(dirname "${log_file}")\"

cur=\"$( ( test -f "${status_file}" && cat "${status_file}" || true ) | tr -d \"\r\" | tail -n 1 )\"
if [[ \"\$cur\" != \"ANALYZED\" ]]; then
  echo RUNNING > \"${status_file}\" 2>/dev/null || true
fi

echo READY > \"${ready_file}\" 2>/dev/null || true

addr=\"${master_ip}:${master_port}\"

# FORÇA master mode
run_cmd=(\"\$bin\" master \"\$addr\" \"${cmd_file}\")

# Start em background (para o deploy continuar)
nohup \"\${run_cmd[@]}\" >>\"${log_file}\" 2>&1 &
echo STARTED
'"

# -----------------------------------------------------------------------------
# Esperar porta abrir
# -----------------------------------------------------------------------------
for _ in $(seq 1 30); do
  if ssh "${remote_user}@${master_ip}" "ss -lntp | grep -q ':${master_port} '"; then
    log_i "discoverymaster LISTEN on ${master_ip}:${master_port}"
    exit 0
  fi
  sleep 0.2
done

log_e "discoverymaster não abriu a porta ${master_port} a tempo. Últimas linhas do log:"
ssh "${remote_user}@${master_ip}" "tail -n 80 '${log_file}' || true" || true
exit 1

