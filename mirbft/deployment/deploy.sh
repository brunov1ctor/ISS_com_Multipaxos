#!/bin/bash

# deploy.sh
#
# Uso:
#   ./deploy.sh <local|remote> <instance-info> <existing|new> <config-generator.sh>
#
# Exemplos:
#   ./deploy.sh local  scripts/instance-info-existing  existing scripts/experiment-configuration/generate-config.sh
#   ./deploy.sh remote scripts/instance-info          new      scripts/experiment-configuration/generate-config.sh

set -euo pipefail

# Carrega variáveis globais (exp_data_root, trap_exit_command, etc.)
source scripts/global-vars.sh

# Garante que todos os processos filhos vão ser mortos ao sair
trap "$trap_exit_command" EXIT

if [ $# -ne 4 ]; then
  echo "Usage: $0 <local|remote> <instance-info> <existing|new> <config-generator>"
  exit 1
fi

mode="$1"                # local | remote
instance_info="$2"       # scripts/instance-info
deployment_mode="$3"     # existing | new
config_generator="$4"    # scripts/experiment-configuration/generate-config.sh

if [[ "$mode" != "local" && "$mode" != "remote" ]]; then
  echo "ERROR: mode must be 'local' or 'remote'."
  exit 1
fi

if [[ "$deployment_mode" != "existing" && "$deployment_mode" != "new" ]]; then
  echo "ERROR: deployment_mode must be 'existing' or 'new'."
  exit 1
fi

if [[ ! -f "$instance_info" ]]; then
  echo "ERROR: instance-info file not found: $instance_info"
  exit 1
fi

if [[ ! -x "$config_generator" ]]; then
  echo "ERROR: config generator script not found or not executable: $config_generator"
  exit 1
fi

echo
echo "============================================================"
echo "Initializing deployment data directory..."
echo "============================================================"
echo

# scripts/initialize-deployment.sh deve imprimir o diretório de dados
exp_data_dir="$(scripts/initialize-deployment.sh "$instance_info" "$deployment_mode" "$config_generator")"

if [[ -z "$exp_data_dir" ]]; then
  echo "ERROR: scripts/initialize-deployment.sh did not return experiment data directory."
  exit 1
fi

if [[ ! -d "$exp_data_dir" ]]; then
  echo "ERROR: experiment data directory does not exist: $exp_data_dir"
  exit 1
fi

echo "Experiment data directory: $exp_data_dir"
echo

case "$mode" in
  local)
    echo "============================================================"
    echo "Running LOCAL deployment..."
    echo "============================================================"
    echo
    scripts/deploy-local.sh "$instance_info" "$exp_data_dir"
    ;;
  remote)
    echo "============================================================"
    echo "Running REMOTE deployment..."
    echo "============================================================"
    echo
    scripts/deploy-remote.sh "$instance_info" "$exp_data_dir"
    ;;
esac

echo
echo "============================================================"
echo "Generating result summary..."
echo "============================================================"
echo

csv_filename="result-data.csv"
result_summary_file="result-summary.txt"

# *** CORREÇÃO AQUI ***
# Antes o caminho para 'experiment-output' estava truncado/errado.
# Agora usamos explicitamente $exp_data_dir/experiment-output.
scripts/analyze/summarize.sh \
  "$exp_data_dir/$csv_filename" \
  "$exp_data_dir/experiment-output" \
  2> /dev/null | tee "$exp_data_dir/$result_summary_file"

echo
echo "Result summary stored in:"
echo "  $exp_data_dir/$result_summary_file"
echo
echo "Deployment finished."

