#!/bin/bash

# --------------------------------------------------------------------
# Bootstrap de permissões:
# Garante que todos os scripts locais tenham permissão de execução,
# incluindo scripts/stubborn-scp.sh, antes de qualquer uso.
# --------------------------------------------------------------------
if [ -d "scripts" ]; then
  chmod +x scripts/*.sh 2>/dev/null || true
  chmod +x scripts/*/*.sh 2>/dev/null || true
fi

# Carrega variáveis globais compartilhadas pelos scripts de deployment.
source scripts/global-vars.sh

# Garante que trap_exit_command tenha um valor default mesmo se não
# tiver sido definido em global-vars.sh (evita "unbound variable").
: "${trap_exit_command:=:}"

# Kill all children of this script when exiting
trap "$trap_exit_command" EXIT

# The '-i' or '--init-only' flag makes the script exit after locally
# initializing the deployment, without running it.
if [ "$1" = "-i" ] || [ "$1" = "--init-only" ]; then
  init_only=true
  shift
else
  init_only=false
fi

# Initializes the deployment.
# Esta parte do script é separada, porque é reutilizada por outros
# tipos de deployment.
# Ela consome múltiplos parâmetros de linha de comando e define:
# - configuration_generator_script
# - depl_type
# - exp_data_dir
# - new_experiment
# - exp_id_offset
# - deployment_file
# - deploy_schedule
# - instance_info_file
# - cancel_instances
source scripts/initialize-deployment.sh

# Exit if only initialization is required.
if $init_only; then
  echo "Init only."
  echo "Experiment directory: $exp_data_dir"
  exit 0
fi

# Start the deployment
if [ "$depl_type" = "local" ]; then
  source scripts/deploy-local.sh
elif [ "$depl_type" = "cloud" ]; then
  source scripts/deploy-cloud.sh
elif [ "$depl_type" = "remote" ]; then
  source scripts/deploy-remote.sh
else
  >&2 echo "$0: unknown deployment type: $depl_type (allowed values: local, cloud, remote)"
fi

echo "Generating result summary."
scripts/analyze/summarize.sh \
  "$exp_data_dir/$csv_filename" \
  "$exp_data_dir/experiment-output" 2> /dev/null \
  | tee "$exp_data_dir/$result_summary_file"

echo "Done."
echo "Experiment data directory: $exp_data_dir"

