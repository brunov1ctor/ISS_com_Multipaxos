#!/usr/bin/env bash
set -euo pipefail

ts(){ date +"%Y-%m-%d %H:%M:%S"; }
log(){ echo "[start-master][$(ts)] $*"; }
warn(){ echo "[start-master][$(ts)] WARN: $*" >&2; }

# Uso:
#   start-master.sh <exp_data_dir> <master_ip>
#
# Requer vars (vindas do deploy.sh/global-vars.sh):
#   remote_user, ssh_options, remote_work_dir, remote_bin_dir
#   DISCOVERY_PORT (ou master_port), remote_status_file, remote_ready_file

exp_data_dir="${1:?exp_data_dir required}"
master_ip="${2:?master_ip required}"

remote_user="${remote_user:-${REMOTE_USER:-${USER}}}"
ssh_options="${ssh_options:--o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null}"
remote_work_dir="${remote_work_dir:-/users/${remote_user}/iss}"
remote_bin_dir="${remote_bin_dir:-/users/${remote_user}/go/bin}"

DISCOVERY_PORT="${DISCOVERY_PORT:-${master_port:-9999}}"

remote_status_file="${remote_status_file:-${remote_work_dir}/status}"
remote_ready_file="${remote_ready_file:-${remote_work_dir}/master-ready}"

local_master_cmd="${exp_data_dir}/master-commands.cmd"
if [[ ! -f "$local_master_cmd" ]]; then
  echo "master-commands.cmd não encontrado: $local_master_cmd" >&2
  exit 2
fi

log "remote_user=${remote_user}"
log "master_ip=${master_ip}"
log "ssh_options=${ssh_options}"
log "remote_work_dir=${remote_work_dir}"
log "remote_bin_dir=${remote_bin_dir}"
log "exp_data_dir=${exp_data_dir}"
log "DISCOVERY_PORT=${DISCOVERY_PORT}"
log "local_master_cmd=${local_master_cmd}"

log "Ensuring remote workdir exists..."
ssh ${ssh_options} "${remote_user}@${master_ip}" "\
  mkdir -p '${remote_work_dir}' \
           '${remote_work_dir}/logs' \
           '${remote_work_dir}/scripts' \
           '${remote_work_dir}/experiment-config' \
           '${remote_work_dir}/current-deployment-data' \
" </dev/null

log "Copying master-commands.cmd to remote..."
scp ${ssh_options} "${local_master_cmd}" "${remote_user}@${master_ip}:${remote_work_dir}/master-commands.cmd" >/dev/null

# ------------------------------------------------------------------
# Copiar configs gerados localmente (exp_data_dir/config/*) para:
#   ${remote_work_dir}/experiment-config/
#
# Motivo: master-commands manda os slaves fazerem scp do MASTER:
#   master:experiment-config/config-XXXX.yml
#
# Evitamos tar pipe via ssh porque login scripts podem sujar STDIN/STDOUT
# e quebrar o stream ("This does not look like a tar archive").
# ------------------------------------------------------------------
if [[ -d "${exp_data_dir}/config" ]]; then
  if compgen -G "${exp_data_dir}/config/*" > /dev/null; then
    log "Copying generated configs to master (experiment-config/) via scp..."
    # mkdir já feito acima, mas reforça:
    ssh ${ssh_options} "${remote_user}@${master_ip}" "mkdir -p '${remote_work_dir}/experiment-config'" </dev/null

    # Copia todos os arquivos do diretório local/config para o master.
    # (Se tiver muitos, scp ainda é ok aqui — e é determinístico.)
    scp ${ssh_options} ${exp_data_dir}/config/* \
      "${remote_user}@${master_ip}:${remote_work_dir}/experiment-config/" >/dev/null

    # Checagem rápida:
    ssh ${ssh_options} "${remote_user}@${master_ip}" "\
      echo '[start-master] remote experiment-config:'; \
      ls -la '${remote_work_dir}/experiment-config' | head -n 80 \
    " </dev/null
  else
    warn "Diretório ${exp_data_dir}/config existe, mas está vazio. Isso vai quebrar o scp dos slaves."
  fi
else
  warn "Diretório local de configs não encontrado: ${exp_data_dir}/config (isso pode quebrar o scp dos slaves)."
fi

log "Killing previous discoverymaster (if any)..."
set +e
ssh ${ssh_opti_

