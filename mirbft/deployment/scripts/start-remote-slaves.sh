#!/usr/bin/env bash
set -euo pipefail

# start-remote-slaves.sh
# Sobe slaves em máquinas remotas (Emulab) para uma tag específica (peers/1client).
# Copia scripts e binários para o nó e dispara start-slave.sh via nohup.

exp_data_dir="$1"
instance_info_file="$2"
wanted_tag="$3"
remote_user="$4"
remote_work_dir="$5"
remote_bin_dir="$6"
remote_exp_dir="$7"
local_bin_dir="$8"
ssh_options="$9"

log() { echo "[INFO  ][$(date '+%Y-%m-%d %H:%M:%S')] $*"; }
warn() { echo "[WARN  ][$(date '+%Y-%m-%d %H:%M:%S')] $*" >&2; }
die() { echo "[ERROR ][$(date '+%Y-%m-%d %H:%M:%S')] $*" >&2; exit 1; }

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# scripts a enviar
scripts_to_send=(
  "${SCRIPT_DIR}/global-vars.sh"
  "${SCRIPT_DIR}/start-slave.sh"
  "${SCRIPT_DIR}/stubborn-scp.sh"
)

# binários necessários
bins_to_send=(
  "discoverymaster"
  "discoveryslave"
  "orderingpeer"
  "orderingclient"
)

log "==== [start-remote-slaves] Contexto ====="
log "  exp_data_dir       = ${exp_data_dir}"
log "  instance_info_file = ${instance_info_file}"
log "  wanted_tag         = ${wanted_tag}"
log "  remote_user        = ${remote_user}"
log "  remote_work_dir    = ${remote_work_dir}"
log "  remote_bin_dir     = ${remote_bin_dir}"
log "  remote_exp_dir     = ${remote_exp_dir}"
log "  local_bin_dir      = ${local_bin_dir}"
log "  ssh_options        = ${ssh_options}"
log "  SSH_START_TIMEOUT  = 12s"
log ""

[[ -f "$instance_info_file" ]] || die "instance-info não existe: $instance_info_file"
[[ -d "$exp_data_dir" ]] || die "exp_data_dir não existe: $exp_data_dir"

# Descobre master_ip (tag master)
master_ip=""
while IFS= read -r line; do
  l="$(echo "$line" | sed 's/#.*$//' | xargs || true)"
  [[ -z "$l" ]] && continue
  tag="$(echo "$l" | awk '{print $4}')"
  if [[ "$tag" == "master" ]]; then
    master_ip="$(echo "$l" | awk '{print $2}')"
    break
  fi
done < "$instance_info_file"

[[ -n "$master_ip" ]] || die "Não encontrei master no instance-info."

log "[start-remote-slaves] master_ip = ${master_ip}"

tmp_scripts_dir="${remote_work_dir}/scripts"
tmp_bins_dir="${remote_bin_dir}"
tmp_logs_dir="${remote_work_dir}/logs"

lines_processed=0
matches=0

while IFS= read -r line; do
  lines_processed=$((lines_processed+1))
  l="$(echo "$line" | sed 's/#.*$//' | xargs || true)"
  [[ -z "$l" ]] && continue

  node="$(echo "$l" | awk '{print $1}')"
  pub_ip="$(echo "$l" | awk '{print $2}')"
  priv_ip="$(echo "$l" | awk '{print $3}')"
  tag="$(echo "$l" | awk '{print $4}')"

  if [[ "$tag" != "$wanted_tag" ]]; then
    continue
  fi

  matches=$((matches+1))
  log "[start-remote-slaves] Iniciando ${node} (${tag}) @ ${pub_ip}"

  # garante diretórios remotos
  ssh ${ssh_options} "${remote_user}@${pub_ip}" "mkdir -p '${tmp_scripts_dir}' '${tmp_bins_dir}' '${tmp_logs_dir}' '${remote_work_dir}/config' '${remote_exp_dir}'"

  # copia scripts
  for s in "${scripts_to_send[@]}"; do
    base="$(basename "$s")"
    "${SCRIPT_DIR}/stubborn-scp.sh" 10 "$s" "${remote_user}@${pub_ip}:${tmp_scripts_dir}/${base}"
    ssh ${ssh_options} "${remote_user}@${pub_ip}" "chmod +x '${tmp_scripts_dir}/${base}' || true"
  done

  # copia binários (atomic: envia para .tmp e move)
  for b in "${bins_to_send[@]}"; do
    src="${local_bin_dir}/${b}"
    [[ -x "$src" ]] || die "Binário não existe/executável localmente: $src"
    log "[copy] ${pub_ip}: enviando binário ${b} (atomic)"
    "${SCRIPT_DIR}/stubborn-scp.sh" 10 "$src" "${remote_user}@${pub_ip}:${tmp_bins_dir}/${b}.tmp"
    ssh ${ssh_options} "${remote_user}@${pub_ip}" "mv -f '${tmp_bins_dir}/${b}.tmp' '${tmp_bins_dir}/${b}' && chmod +x '${tmp_bins_dir}/${b}'"
  done

  # remote-check: lista scripts e bins (ajuda debug)
  ssh ${ssh_options} "${remote_user}@${pub_ip}" "
    echo '[remote-check] scripts:'; ls -la '${tmp_scripts_dir}' || true
    echo '[remote-check] bins:'; ls -la '${tmp_bins_dir}' || true
  " || true

  # dispara start-slave.sh (nohup)
  # args: <tag> <master_ip> <own_public_ip> <own_private_ip> <remote_exp_dir>
  log "[ssh] ${pub_ip}: cd '${tmp_scripts_dir}' && /usr/bin/nohup bash ./start-slave.sh '${tag}' '${master_ip}' '${pub_ip}' '${priv_ip}' '${remote_exp_dir}' > '${tmp_logs_dir}/slave-${node}.log' 2>&1 < /dev/null & echo STARTED; exit 0"
  ssh ${ssh_options} "${remote_user}@${pub_ip}" "
    cd '${tmp_scripts_dir}' &&
    /usr/bin/nohup bash ./start-slave.sh '${tag}' '${master_ip}' '${pub_ip}' '${priv_ip}' '${remote_exp_dir}' \
      > '${tmp_logs_dir}/slave-${node}.log' 2>&1 < /dev/null &
    echo STARTED
    exit 0
  "

done < "$instance_info_file"

log "[start-remote-slaves] Linhas processadas: ${lines_processed}, matches(tag=${wanted_tag}): ${matches}"
log "==== [start-remote-slaves] FIM ===="

