#!/bin/bash
#
# start-remote-slaves.sh
#
# Versão para ambiente remoto (Emulab / instance-info / binários já instalados).
#
# Chamado pelo deploy.sh tipicamente como:
#   scripts/start-remote-slaves.sh <exp_data_dir> <algum_numero> <tag> <instance_info_file>
#   Ex.: scripts/start-remote-slaves.sh deployment-data/remote-0000 5 peers   scripts/instance-info
#        scripts/start-remote-slaves.sh deployment-data/remote-0000 1 1client scripts/instance-info
#
# Este script:
#   - resolve o exp_data_dir de forma robusta (deployment/ vs repo root)
#   - detecta a tag (peers, 1client, etc.)
#   - detecta o arquivo instance-info
#   - garante diretórios remotos e copia scripts/binários
#   - dispara start-slave.sh em cada nó que tenha a tag desejada
#

set -euo pipefail

source scripts/global-vars.sh

if [[ $# -lt 2 ]]; then
  echo "Uso: $0 <exp_data_dir> <outros argumentos...>"
  exit 1
fi

############################################
# Diretórios base
############################################

this_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
deployment_dir="$(cd "$this_dir/.." && pwd)"
repo_dir="$(cd "$deployment_dir/.." && pwd)"

exp_data_dir_arg="$1"
shift 1  # resto dos args: podem ser número, tag, instance-info etc.

############################################
# Resolver exp_data_dir (sem exigir master-commands)
############################################

resolve_exp_dir_slaves() {
  local arg="$1"
  local cand_abs=""
  local cand1=""
  local cand2=""

  if [[ "$arg" = /* ]]; then
    cand_abs="$arg"
    if [[ -d "$cand_abs" ]]; then
      echo "$cand_abs"
      return 0
    fi
  fi

  cand1="$deployment_dir/$arg"
  cand2="$repo_dir/$arg"

  if [[ -d "$cand1" ]]; then
    echo "$cand1"
    return 0
  fi
  if [[ -d "$cand2" ]]; then
    echo "$cand2"
    return 0
  fi

  return 1
}

if ! exp_data_dir="$(resolve_exp_dir_slaves "$exp_data_dir_arg")"; then
  echo "[ERRO  ][$(date +"%Y-%m-%d %H:%M:%S")] Diretório de experimento não encontrado para '$exp_data_dir_arg'."
  echo "[ERRO  ][$(date +"%Y-%m-%d %H:%M:%S")]   Tentativas:"
  echo "[ERRO  ][$(date +"%Y-%m-%d %H:%M:%S")]     - $deployment_dir/$exp_data_dir_arg"
  echo "[ERRO  ][$(date +"%Y-%m-%d %H:%M:%S")]     - $repo_dir/$exp_data_dir_arg"
  exit 1
fi

############################################
# Detectar instance_info_file e wanted_tag
############################################

instance_info_file=""
wanted_tag=""

is_tag() {
  case "$1" in
    peers|1client|clients|client|peers+clients)
      return 0
      ;;
    *)
      return 1
      ;;
  esac
}

for arg in "$@"; do
  # Primeiro arquivo existente vira instance_info_file
  if [[ -z "$instance_info_file" && -f "$arg" ]]; then
    instance_info_file="$arg"
    continue
  fi

  # Primeira tag conhecida vira wanted_tag
  if [[ -z "$wanted_tag" ]] && is_tag "$arg"; then
    wanted_tag="$arg"
    continue
  fi
done

if [[ -z "$instance_info_file" ]]; then
  echo "[ERRO  ][$(date +"%Y-%m-%d %H:%M:%S")] Não foi possível detectar o arquivo instance-info entre os argumentos:"
  echo "[ERRO  ][$(date +"%Y-%m-%d %H:%M:%S")]   exp_data_dir = $exp_data_dir_arg"
  echo "[ERRO  ][$(date +"%Y-%m-%d %H:%M:%S")]   args        = $*"
  exit 1
fi

if [[ -z "$wanted_tag" ]]; then
  echo "[ERRO  ][$(date +"%Y-%m-%d %H:%M:%S")] Não foi possível detectar a tag (peers, 1client, etc.) entre os argumentos:"
  echo "[ERRO  ][$(date +"%Y-%m-%d %H:%M:%S")]   exp_data_dir = $exp_data_dir_arg"
  echo "[ERRO  ][$(date +"%Y-%m-%d %H:%M:%S")]   args        = $*"
  exit 1
fi

echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] ==== [start-remote-slaves] Diretórios detectados ====="
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   this_dir           = $this_dir"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   deployment_dir     = $deployment_dir"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   repo_dir           = $repo_dir"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   exp_data_dir       = $exp_data_dir"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   instance_info_file = $instance_info_file"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   wanted_tag        = $wanted_tag"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   remote_user        = $remote_user"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   remote_gopath      = $remote_gopath"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   remote_bin_dir     = $remote_bin_dir"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   remote_work_dir    = $remote_work_dir"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   remote_exp_dir     = $remote_exp_dir"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] "

if [[ ! -f "$instance_info_file" ]]; then
  echo "[ERRO  ][$(date +"%Y-%m-%d %H:%M:%S")] Arquivo instance-info não encontrado: $instance_info_file"
  exit 1
fi

# Número de tentativas do stubborn-scp (pode sobrescrever via env scp_retries)
scp_retries="${scp_retries:-10}"

# ===================================================================
# 1) Distribui scripts e binários para os slaves da tag desejada
# ===================================================================

echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] ==== [start-remote-slaves] Distribuindo scripts/binários aos slaves ====""

# Prepara diretório temporário de scripts a serem enviados
tmp_scripts_dir="${exp_data_dir}/scripts-temp"
rm -rf "$tmp_scripts_dir"
mkdir -p "$tmp_scripts_dir"
cp "$this_dir/start-slave.sh" "$tmp_scripts_dir/"
cp "$this_dir/stubborn-scp.sh" "$tmp_scripts_dir/"
cp "$this_dir/new-experiment-state.sh" "$tmp_scripts_dir/"

process_instance_line() {
  local line="$1"

  # Ignora vazias e comentários
  [[ -z "$line" ]] && return 0
  [[ "$line" =~ ^[[:space:]]*# ]] && return 0

  # Formato:
  #   <tag> <ctrl_ip> <private_ip> <public_ip> <instance_id>
  set -- $line
  local tag="$1"
  local ctrl_ip="$2"
  local private_ip="$3"
  local public_ip="$4"
  local instance_id="$5"

  # Só nos interessam as linhas da tag desejada
  if [[ "$tag" != "$wanted_tag" ]]; then
    return 0
  fi

  echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] ---------------------------------------------------------------------"
  echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   [REMOTO] Garantindo ambiente em $ctrl_ip"
  echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]            instance_id = $instance_id"
  echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]            tag         = $tag"

  ssh ${ssh_options} "${remote_user}@${ctrl_ip}" "
    mkdir -p \
      '$remote_work_dir' \
      '$remote_exp_dir' \
      '$remote_config_dir' \
      '$remote_work_dir/scripts' \
      '$remote_work_dir/logs' \
      '$remote_work_dir/tls-data' \
      '$remote_bin_dir'
  " >/dev/null 2>&1 || true

  # Copia scripts (incluindo stubborn-scp)
  bash "$this_dir/stubborn-scp.sh" \
    "$scp_retries" \
    "$tmp_scripts_dir/" \
    "${remote_user}@${ctrl_ip}:$remote_work_dir/scripts/"

  # Copia binários (orderingpeer, orderingclient, discovery*)
  bash "$this_dir/stubborn-scp.sh" \
    "$scp_retries" \
    "$remote_bin_dir/" \
    "${remote_user}@${ctrl_ip}:$remote_bin_dir/"

  echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]     [REMOTO] OK: ambiente garantido em $ctrl_ip."
}

while IFS= read -r line; do
  process_instance_line "$line"
done < "$instance_info_file"

echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] ==== [start-remote-slaves] Distribuição concluída. ===="
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] "

# ===================================================================
# 2) Dispara os slaves da tag desejada
# ===================================================================

echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] ==== [start-remote-slaves] Disparando slaves da tag '$wanted_tag' ===="

start_instance_line() {
  local line="$1"

  [[ -z "$line" ]] && return 0
  [[ "$line" =~ ^[[:space:]]*# ]] && return 0

  set -- $line
  local tag="$1"
  local ctrl_ip="$2"
  local private_ip="$3"
  local public_ip="$4"
  local instance_id="$5"

  if [[ "$tag" != "$wanted_tag" ]]; then
    return 0
  fi

  echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   [DEPLOY] Iniciando slave em $ctrl_ip (instance_id=$instance_id, tag=$tag)"

  ssh ${ssh_options} "${remote_user}@${ctrl_ip}" "
    nohup '$remote_work_dir/scripts/start-slave.sh' \
      '$exp_data_dir' \
      '$ctrl_ip' \
      '$private_ip' \
      '$public_ip' \
      '$instance_id' \
      '$tag' \
      > '$remote_work_dir/logs/slave-${instance_id}.log' 2>&1 &
  " >/dev/null 2>&1 || true
}

while IFS= read -r line; do
  start_instance_line "$line"
done < "$instance_info_file"

echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] "
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] ==== [start-remote-slaves] Todos os slaves disparados. ===="
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] ==== [start-remote-slaves] FIM ==========================================="

