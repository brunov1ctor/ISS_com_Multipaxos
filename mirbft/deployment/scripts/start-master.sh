#!/bin/bash

# ============================================================================
# start-master.sh
#   - Roda no master remoto
#   - Pega master-commands.cmd enviado pelo deploy e ajusta caminhos/binários
#   - Garante que discoverymaster está up e porta de discovery está escutando
# ============================================================================

set -euo pipefail

timestamp() {
  date +"%Y-%m-%d %H:%M:%S-%z"
}

log() {
  echo "[start-master][$(timestamp)] $*"
}

# ---------------------------------------------------------------------------
# 1) Parâmetros e contexto
# ---------------------------------------------------------------------------
remote_user="${USER:-Bruno}"
master_ip="$(hostname -I | awk '{print $1}')"
ssh_options="-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null"

DISCOVERY_PORT="${DISCOVERY_PORT:-9999}"

# Diretórios remotos sugeridos pelo deploy.sh
remote_work_dir="${remote_work_dir:-/users/Bruno/iss}"
remote_bin_dir="${remote_bin_dir:-/users/Bruno/go/bin}"
exp_data_dir="${exp_data_dir:-/users/Bruno/iss/current-deployment-data}"

local_master_cmd="${local_master_cmd:-$exp_data_dir/master-commands.cmd}"
debug_log="${debug_log:-$exp_data_dir/_debug/start-master.$master_ip.log}"

log "remote_user=$remote_user"
log "master_ip=$master_ip"
log "ssh_options=$ssh_options"
log "remote_work_dir=$remote_work_dir"
log "remote_bin_dir=$remote_bin_dir"
log "exp_data_dir=$exp_data_dir"
log "local_master_cmd=$local_master_cmd"
log "debug_log=$debug_log"
log "DISCOVERY_PORT=$DISCOVERY_PORT"

mkdir -p "$(dirname "$debug_log")"

# ---------------------------------------------------------------------------
# 2) Função para patch de master-commands.cmd
# ---------------------------------------------------------------------------
patch_master_commands() {
  local f="$1"
  log "Patching master-commands.cmd (PATH-proof + config absoluto)..."

  local backup="${f}.bak.$(date +%s)"
  cp "$f" "$backup"
  log "Backup: $backup"

  # Garante que config/ exista no remote_work_dir
  grep -q "mkdir -p ${remote_work_dir}/config" "$f" || {
    log "Inserindo mkdir -p ${remote_work_dir}/config no master-commands..."
    sed -i "1i mkdir -p ${remote_work_dir}/config" "$f"
  }

  # Atualiza caminhos de binários para o diretório remoto de binários
  sed -i "s| orderingpeer | ${remote_bin_dir}/orderingpeer |g" "$f"
  sed -i "s| orderingclient | ${remote_bin_dir}/orderingclient |g" "$f"
  sed -i "s| discoverymaster | ${remote_bin_dir}/discoverymaster |g" "$f"
  sed -i "s| discoveryslave | ${remote_bin_dir}/discoveryslave |g" "$f"

  # Garante caminhos de config.yml absolutos dentro de ${remote_work_dir}/config
  sed -i "s|config/config-0000.yml|${remote_work_dir}/config/config.yml|g" "$f"
  sed -i "s|config/config-0001.yml|${remote_work_dir}/config/config.yml|g" "$f"
  sed -i "s|config/config-0002.yml|${remote_work_dir}/config/config.yml|g" "$f"
  sed -i "s|config/config-0003.yml|${remote_work_dir}/config/config.yml|g" "$f"

  # Substitui caminhos relativos de config nas linhas de scp/cp
  sed -i "s|experiment-config/config-0000.yml|${remote_work_dir}/experiment-config/config-0000.yml|g" "$f"
  sed -i "s|experiment-config/config-0001.yml|${remote_work_dir}/experiment-config/config-0001.yml|g" "$f"
  sed -i "s|experiment-config/config-0002.yml|${remote_work_dir}/experiment-config/config-0002.yml|g" "$f"
  sed -i "s|experiment-config/config-0003.yml|${remote_work_dir}/experiment-config/config-0003.yml|g" "$f"

  # (Opcional) Verificação mais enxuta dos trechos críticos
  log "Trechos críticos: verificação rápida de consistência."
  local grep_count
  grep_count=$(egrep -n "stubborn-scp|orderingpeer|orderingclient|config/...-|mkdir -p ${remote_work_dir}/config" "$f" | wc -l || echo 0)
  log "Linhas relevantes encontradas: $grep_count. (Se quiser detalhes, rode grep manualmente no master-commands.cmd.)"
  log "Patch OK."
}

# ---------------------------------------------------------------------------
# 3) Garante que diretório remoto existe e copia arquivos necessários
# ---------------------------------------------------------------------------
log "Ensuring remote workdir exists..."
ssh $ssh_options "$remote_user@$master_ip" "mkdir -p $remote_work_dir" >> "$debug_log" 2>&1

log "Copying master-commands.cmd to remote..."
scp $ssh_options "$local_master_cmd" "$remote_user@$master_ip:$remote_work_dir/master-commands.cmd" >> "$debug_log" 2>&1

log "Copying helper scripts (stubborn-scp.sh, global-vars.sh) to master..."
scp $ssh_options "$exp_data_dir/scripts/stubborn-scp.sh" "$remote_user@$master_ip:$remote_work_dir/scripts/" >> "$debug_log" 2>&1 || true
scp $ssh_options "$exp_data_dir/scripts/global-vars.sh" "$remote_user@$master_ip:$remote_work_dir/scripts/" >> "$debug_log" 2>&1 || true

log "Copying generated configs to master from $exp_data_dir/experiment-config ..."
scp $ssh_options "$exp_data_dir/experiment-config"/*.yml "$remote_user@$master_ip:$remote_work_dir/experiment-config/" >> "$debug_log" 2>&1 || true

# ---------------------------------------------------------------------------
# 4) Inicia discoverymaster em background
# ---------------------------------------------------------------------------
log "Iniciando discoverymaster no master (nohup)..."
ssh $ssh_options "$remote_user@$master_ip" "cd $remote_work_dir && nohup $remote_bin_dir/discoverymaster -port $DISCOVERY_PORT > discoverymaster.log 2>&1 &" >> "$debug_log" 2>&1

# Espera porta subir
log "Verificando se o master está escutando na porta $DISCOVERY_PORT..."
for i in {1..12}; do
  if ssh $ssh_options "$remote_user@$master_ip" "netstat -ntlp 2>/dev/null | grep -q \":$DISCOVERY_PORT\""; then
    log "Master started successfully e está escutando em $master_ip:$DISCOVERY_PORT."
    break
  fi
  sleep 1
done

# ---------------------------------------------------------------------------
# 5) Patching final e execução de master-commands no master
# ---------------------------------------------------------------------------
log "Patchando master-commands no master e iniciando experimento..."
ssh $ssh_options "$remote_user@$master_ip" "
  cd $remote_work_dir
  ./scripts/start-master-inner.sh
" >> "$debug_log" 2>&1 &

log "start-master.sh em background."

