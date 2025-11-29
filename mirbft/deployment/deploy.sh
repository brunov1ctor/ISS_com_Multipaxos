#!/bin/bash

# Carrega variáveis globais
source scripts/global-vars.sh

# Garante que todos os filhos desse script morram ao sair
trap "$trap_exit_command" EXIT

# Flag opcional: -i / --init-only  (só inicializa, não roda experimento)
if [ "$1" = "-i" ] || [ "$1" = "--init-only" ]; then
  init_only=true
  shift
else
  init_only=false
fi

# -----------------------------------------------------------------------------
# Inicializa o deployment (define: depl_type, exp_data_dir, new_experiment,
# exp_id_offset, deployment_file, deploy_schedule, instance_info_file, etc.)
# -----------------------------------------------------------------------------
# IMPORTANTE: aqui apenas "source", NÃO capturar stdout. O initialize-deployment.sh
# usa os argumentos remanescentes ($@) do deploy.sh:
#   ./deploy.sh <depl_type> [instance-info] <existing|new> <config-generator>
# Ex.: ./deploy.sh remote scripts/instance-info new scripts/experiment-configuration/generate-config.sh
# -----------------------------------------------------------------------------
source scripts/initialize-deployment.sh "$@"

# Se for só para inicializar, saímos aqui.
if $init_only; then
  echo "Init only. Experiment directory: $exp_data_dir"
  exit 0
fi

# -----------------------------------------------------------------------------
# Inicia o deployment conforme o tipo
# -----------------------------------------------------------------------------
if [ "$depl_type" = "local" ]; then
  source scripts/deploy-local.sh
elif [ "$depl_type" = "cloud" ]; then
  source scripts/deploy-cloud.sh
elif [ "$depl_type" = "remote" ]; then
  source scripts/deploy-remote.sh
else
  >&2 echo "$0: unknown deployment type: $depl_type (allowed values: local, cloud, remote)"
fi

# -----------------------------------------------------------------------------
# Geração do resumo final (CSV + result-summary)
# -----------------------------------------------------------------------------
echo "Generating result summary."
scripts/analyze/summarize.sh \
  "$exp_data_dir/$csv_filename" \
  "$exp_data_dir/experiment-output" \
  2> /dev/null | tee "$exp_data_dir/$result_summary_file"

echo "Done. Experiment data directory: $exp_data_dir"

