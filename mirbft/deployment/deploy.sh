#!/bin/bash

# --------------------------------------------------------------------
# Carrega variáveis globais (deployment_data_root, csv_filename, etc.)
# --------------------------------------------------------------------
source scripts/global-vars.sh

# Kill all children of this script when exiting
trap "$trap_exit_command" EXIT

# --------------------------------------------------------------------
# Trata flag de inicialização apenas (-i / --init-only)
# --------------------------------------------------------------------
if [ "$1" = "-i" ] || [ "$1" = "--init-only" ]; then
  init_only=true
  shift
else
  init_only=false
fi

# --------------------------------------------------------------------
# Suporte ao modo "new": gerar configs antes do initialize-deployment
#
# Uso esperado:
#   ./deploy.sh remote scripts/instance-info new scripts/experiment-configuration/generate-config.sh
#
# Nesse caso:
#   - Gera configs em deployment-data/remote-0000
#   - Remove "new <script>" da linha de comando antes de chamar initialize-deployment.sh
# --------------------------------------------------------------------
if [ "$1" = "remote" ] && [ "$3" = "new" ]; then
  depl_type="$1"
  instance_info_file="$2"
  new_flag="$3"
  config_gen_script="$4"

  # Se por algum motivo não vier o script, usa um default
  if [ -z "$config_gen_script" ]; then
    config_gen_script="scripts/experiment-configuration/generate-config.sh"
  fi

  # Diretório de experimento que o initialize-deployment.sh espera.
  # Pelo erro anterior, sabemos que ele procura:
  #   deployment-data/remote-0000/deployment.dpl
  exp_data_dir="$deployment_data_root/remote-0000"

  mkdir -p "$exp_data_dir"

  echo "Using experiment data directory: $exp_data_dir"
  # exp_id_offset = 0 (primeiro experimento)
  "$config_gen_script" "$exp_data_dir" 0

  # Depois de gerar as configs, removemos o "new <script>" da linha de comando,
  # e deixamos apenas: remote scripts/instance-info
  set -- "$depl_type" "$instance_info_file"
fi

# --------------------------------------------------------------------
# Inicializa o deployment (lê args, seta depl_type, exp_data_dir, etc.)
# --------------------------------------------------------------------
# Este script consome os parâmetros atuais de linha de comando e define:
#  - configuration_generator_script
#  - depl_type
#  - exp_data_dir
#  - new_experiment
#  - exp_id_offset
#  - deployment_file
#  - deploy_schedule
#  - instance_info_file
#  - cancel_instances
source scripts/initialize-deployment.sh

# --------------------------------------------------------------------
# Se for só inicialização, sai aqui.
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
  >&2 echo "$0: unknown deployment type: $depl_type (allowed values: local, cloud, remote)"
fi

# --------------------------------------------------------------------
# Geração do resumo dos resultados
# --------------------------------------------------------------------
echo "Generating result summary."
scripts/analyze/summarize.sh \
  "$exp_data_dir/$csv_filename" \
  "$exp_data_dir/experiment-output" 2> /dev/null \
  | tee "$exp_data_dir/$result_summary_file"

echo "Done. Experiment data directory: $exp_data_dir"

