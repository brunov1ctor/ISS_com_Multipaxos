#!/bin/bash

# -------------------------------------------------------------
# deploy.sh
#
# Usage:
#   ./deploy.sh <local|remote|cloud> <instance-info> <new|reuse> <config-gen-script> [flags]
#
# Example:
#   ./deploy.sh remote scripts/instance-info new scripts/experiment-configuration/generate-config.sh
# -------------------------------------------------------------

set -euo pipefail

deployment_dir="$(cd "$(dirname "$0")" && pwd)"
cd "$deployment_dir"

source scripts/global-vars.sh

if [ $# -lt 4 ]; then
  echo "Usage: $0 <local|remote|cloud> <instance-info> <new|reuse> <config-gen-script> [flags]"
  exit 1
fi

depl_type="$1"
instance_info_file="$2"
new_or_reuse="$3"
config_gen_script="$4"
shift 4

init_only=false
cancel_instances=false

while getopts "ic" opt; do
  case "$opt" in
    i) init_only=true ;;
    c) cancel_instances=true ;;
    *) ;;
  esac
done

# --------------------------------------------------------------------
# Determina o diretório de experimento (exp_data_dir)
# --------------------------------------------------------------------
if [ "$new_or_reuse" = "new" ]; then
  exp_index=0
  while true; do
    candidate=$(printf "%s/remote-%04d" "$deployment_data_root" "$exp_index")
    if [ ! -d "$candidate" ]; then
      exp_data_dir="$candidate"
      break
    fi
    exp_index=$((exp_index + 1))
  done

  mkdir -p "$exp_data_dir"

  echo "Using experiment data directory: $exp_data_dir"

  echo
  echo "=================================================="
  echo "[SSH] Preflight: atualizando known_hosts para hosts do instance-info"
  echo "=================================================="
  if [[ "$depl_type" == "remote" ]]; then
    known_hosts_file="${DEPLOY_KNOWN_HOSTS_FILE:-$HOME/.ssh/known_hosts}"
    mkdir -p "$(dirname "$known_hosts_file")"
    touch "$known_hosts_file"
    chmod 600 "$known_hosts_file" 2>/dev/null || true

    # Extrai IPs (ctrl_ip e data_ip) do instance-info.
    mapfile -t _ips < <(awk 'NF>=3 {print $2"\n"$3}' "$instance_info_file" | sed '/^$/d' | sort -u)

    echo "[SSH] known_hosts_file = $known_hosts_file"
    echo "[SSH] hosts encontrados = ${#_ips[@]}"
    for ip in "${_ips[@]}"; do
      echo "[SSH] - refresh $ip"
      ssh-keygen -R "$ip" -f "$known_hosts_file" >/dev/null 2>&1 || true
      # Pré-carrega a host key; não falha se o nó ainda não estiver acessível.
      ssh-keyscan -T 5 -H "$ip" >> "$known_hosts_file" 2>/dev/null || true
    done
    echo "[SSH] Preflight concluído."
  else
    echo "[SSH] (skipped) depl_type=$depl_type"
  fi
  echo

  # exp_id_offset = 0 (primeiro experimento)
  "$config_gen_script" "$exp_data_dir" 0
  if [ $? -ne 0 ]; then
    echo "ERROR: $config_gen_script falhou ao gerar configurações em $exp_data_dir"
    exit 1
  fi

else
  # reuse: seleciona o diretório mais recente
  exp_data_dir=$(ls -1d "$deployment_data_root"/remote-* 2>/dev/null | tail -n 1 || true)
  if [ -z "${exp_data_dir:-}" ]; then
    echo "ERROR: nenhum diretório encontrado em $deployment_data_root para reuse."
    exit 1
  fi
  echo "Reusing experiment data directory: $exp_data_dir"
fi

# --------------------------------------------------------------------
# Normaliza caminhos e exporta para scripts sourceados
# --------------------------------------------------------------------
export exp_data_dir
export instance_info_file
export depl_type
export cancel_instances

# Antes: set -- "$depl_type" "$instance_info_file"
# Agora passamos exp_data_dir também (caso alguém precise)
set -- "$depl_type" "$instance_info_file" "$exp_data_dir"

# --------------------------------------------------------------------
# Inicializa deployment: cria master-commands.cmd, cloud-instance-info, etc.
# --------------------------------------------------------------------
source scripts/initialize-deployment.sh

# --------------------------------------------------------------------
# Se for só inicialização (-i), sai aqui.
# --------------------------------------------------------------------
if $init_only; then
  echo "Init only. Experiment directory: $exp_data_dir"
  exit 0
fi

# --------------------------------------------------------------------
# Inicia de fato o deployment (local / cloud / remote)
# --------------------------------------------------------------------
if [ "$depl_type" = "local" ]; then
  source scripts/deploy-local.sh
elif [ "$depl_type" = "cloud" ]; then
  source scripts/deploy-cloud.sh
elif [ "$depl_type" = "remote" ]; then
  source scripts/deploy-remote.sh
else
  echo "ERROR: deployment type inválido: $depl_type"
  exit 1
fi

echo "Done. Experiment data directory: $exp_data_dir"
echo "Generating result summary."
source scripts/fetch-results.sh

