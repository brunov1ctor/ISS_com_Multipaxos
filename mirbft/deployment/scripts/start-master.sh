#!/bin/bash
# scripts/start-master.sh
set -euo pipefail

this_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
deployment_dir="$(cd "$this_dir/.." && pwd)"

# shellcheck source=/dev/null
. "$deployment_dir/scripts/global-vars.sh"

exp_data_dir="$1"
master_ip="$2"

local_master_cmd="$exp_data_dir/master-commands.cmd"
remote_master_cmd="$remote_work_dir/master-commands.cmd"

ssh_options="${ssh_options:--o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null}"
remote_user="${remote_user:-$USER}"

local_bin_dir="${LOCAL_BIN_DIR:-${GOBIN:-${GOPATH:-$HOME/go}/bin}}"
DISCOVERY_PORT="${master_port:-9999}"

ts(){ date +"%Y-%m-%d %H:%M:%S"; }
info(){ echo "[start-master][$(ts)] $*"; }
err(){  echo "[start-master][$(ts)][ERRO] $*" >&2; }

info "Using experiment data directory: $exp_data_dir"
info "Using master IP: $master_ip"
info "Local master command script: $local_master_cmd"
info "Remote work dir: $remote_work_dir"
info "Remote master command path: $remote_master_cmd"
info "Local bin dir: $local_bin_dir"
info "Discovery port: $DISCOVERY_PORT"
echo

info "Ensuring remote directories on master ($master_ip)."
ssh $ssh_options "${remote_user}@${master_ip}" " \
  mkdir -p \
    '$remote_work_dir' \
    '$remote_work_dir/config' \
    '$remote_work_dir/logs' \
    '$remote_work_dir/scripts' \
    '$remote_work_dir/tls-data' \
    '$remote_exp_dir' \
    '$remote_exp_dir/raw-results' \
    '$remote_work_dir/experiment-config' \
    '$remote_bin_dir' \
" </dev/null

info "Copying master commands and helper scripts to master."
scp $ssh_options "$local_master_cmd" "${remote_user}@${master_ip}:${remote_master_cmd}"
scp $ssh_options "$deployment_dir/scripts/start-slave.sh" "${remote_user}@${master_ip}:${remote_work_dir}/scripts/start-slave.sh"
scp $ssh_options "$deployment_dir/scripts/stubborn-scp.sh" "${remote_user}@${master_ip}:${remote_work_dir}/scripts/stubborn-scp.sh"
scp $ssh_options "$deployment_dir/scripts/global-vars.sh" "${remote_user}@${master_ip}:${remote_work_dir}/scripts/global-vars.sh"

info "Copying required binaries to master (if found locally)."
for bin in discoverymaster discoveryslave orderingpeer orderingclient; do
  if [[ -x "${local_bin_dir}/${bin}" ]]; then
    info "  - sending ${bin} -> ${remote_bin_dir}/${bin}"
    scp $ssh_options "${local_bin_dir}/${bin}" "${remote_user}@${master_ip}:${remote_bin_dir}/${bin}"
  else
    info "  [WARN] local bin not found: ${local_bin_dir}/${bin}"
  fi
done

info "Copying experiment config files to master."
local_config_src_dir="$exp_data_dir/config"
if ls "$local_config_src_dir"/config-*.yml >/dev/null 2>&1; then
  scp $ssh_options "$local_config_src_dir"/config-*.yml "${remote_user}@${master_ip}:${remote_work_dir}/experiment-config/"
else
  info "WARNING: nenhum arquivo config-XXXX.yml encontrado em $local_config_src_dir; configs não foram copiadas."
fi
echo

# ---------------------------------------------------------------------
# 1) Inicia discoverymaster em modo LEGACY (porta como argumento)
#    e valida que ele fica vivo.
# ---------------------------------------------------------------------
info "Starting discoverymaster LEGACY on remote master ($master_ip)."

ssh $ssh_options "${remote_user}@${master_ip}" " \
  set -e; \
  chmod +x '$remote_bin_dir/discoverymaster' 2>/dev/null || true; \
  if [[ ! -x '$remote_bin_dir/discoverymaster' ]]; then \
    echo 'ERRO: discoverymaster não existe ou não é executável em $remote_bin_dir/discoverymaster'; \
    ls -la '$remote_bin_dir' | head -n 80; \
    exit 2; \
  fi; \
  pkill -9 -f '$remote_bin_dir/discoverymaster' 2>/dev/null || true; \
  cd '$remote_work_dir'; \
  /usr/bin/nohup '$remote_bin_dir/discoverymaster' '$DISCOVERY_PORT' > main_log.log 2>&1 < /dev/null & \
  echo \$! > .discoverymaster.pid; \
  sleep 1; \
  if ! kill -0 \$(cat .discoverymaster.pid) 2>/dev/null; then \
    echo 'ERRO: discoverymaster morreu ao iniciar. main_log:'; \
    tail -n 120 '$remote_work_dir/main_log.log' 2>/dev/null || true; \
    exit 3; \
  fi; \
  echo 'OK: discoverymaster pid=' \$(cat .discoverymaster.pid); \
" </dev/null

# ---------------------------------------------------------------------
# 2) Inicia a execução real dos experimentos chamando master-commands.cmd
#    diretamente (não depende do discoverymaster suportar -commands).
# ---------------------------------------------------------------------
info "Starting master-commands.cmd directly on master (nohup)."

ssh $ssh_options "${remote_user}@${master_ip}" " \
  set -e; \
  chmod +x '$remote_master_cmd' '$remote_work_dir/scripts/start-slave.sh' '$remote_work_dir/scripts/stubborn-scp.sh' 2>/dev/null || true; \
  cd '$remote_work_dir'; \
  /usr/bin/nohup bash '$remote_master_cmd' > '$remote_work_dir/logs/master-commands.nohup.log' 2>&1 < /dev/null & \
  echo \$! > .master-commands.pid; \
  sleep 1;

