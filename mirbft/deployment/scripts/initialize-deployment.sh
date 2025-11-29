#!/bin/bash
# This script is not to be ran on its own.
# It is the only included as the common part of the different kinds of deployments,
# Initializing the experiment, reading command-line arguments and setting some variables.
# It must be sourced, NOT ran even inside those scripts.
# It consumes up to 4 command line parameters and sets the following variables:
# - configuration_generator_script
# - depl_type
# - exp_data_dir
# - new_experiment
# - exp_id_offset
# - deployment_file
# - deploy_schedule
# - cancel_instances
# Some of them are used only internally some of them are used by this including script.

# -----------------------------------------------------------------------------
# 1) Optional "-c" flag (cancel instances)
# -----------------------------------------------------------------------------
if [ "$1" = "-c" ]; then
  cancel_instances=true
  shift
else
  cancel_instances=false
fi

# -----------------------------------------------------------------------------
# 2) Deployment type: local | cloud | remote
# -----------------------------------------------------------------------------
if [ -n "$1" ] && [ "$1" = "local" ]; then
  depl_type="$1"
  shift
elif [ -n "$1" ] && [ "$1" = "cloud" ]; then
  depl_type="$1"
  shift
elif [ -n "$1" ] && [ "$1" = "remote" ]; then
  depl_type="$1"
  instance_info_file="$2"
  shift 2
else
  depl_type="local" # Default deployment type
fi

# -----------------------------------------------------------------------------
# 3) Experiment data directory or "new"
# -----------------------------------------------------------------------------
if [ "$1" = "new" ]; then
  exp_data_dir=$(scripts/new-experiment-state.sh "$depl_type")
  new_experiment=true
else
  exp_data_dir="$1"
  new_experiment=false
fi
shift
echo "Using experiment data directory: $exp_data_dir"

# -----------------------------------------------------------------------------
# 4) If new experiment, generate configs
# -----------------------------------------------------------------------------
if $new_experiment; then
  # Config generator script
  configuration_generator_script="$1"
  shift

  # Experiment id offset (optional)
  if [ -n "$1" ]; then
    exp_id_offset="$1"
    shift
  else
    exp_id_offset=0
  fi

  # Generate deployment + configs
  "$configuration_generator_script" "$exp_data_dir" "$exp_id_offset" || exit 1

  # Save copy of generator
  cp "$configuration_generator_script" "$exp_data_dir"
fi

# -----------------------------------------------------------------------------
# 5) Parse deployment file and generate master command template + schedule
# -----------------------------------------------------------------------------
deployment_file="$exp_data_dir/$dpl_filename"
echo "Using deployment file: $deployment_file"

echo "initialize-deployment.sh: debug info:"
echo "  depl_type          = ${depl_type:-<unset>}"
echo "  exp_data_dir       = ${exp_data_dir:-<unset>}"
echo "  new_experiment     = ${new_experiment:-<unset>}"
echo "  exp_id_offset      = ${exp_id_offset:-<unset>}"
echo "  deployment_file    = ${deployment_file:-<unset>}"
echo "  instance_info_file = ${instance_info_file:-<unset>}"

# IMPORTANTE:
# Para o fluxo atual, NÃO vamos chamar generate-master-commands.py.
# No modo remoto, o ISS usa scripts/instance-info para descobrir os IPs,
# e o deploy_schedule não é usado. Para evitar o erro e ficar mais fácil
# debugar, apenas definimos deploy_schedule como vazio.
deploy_schedule=""

echo "initialize-deployment.sh: skipping generate-master-commands.py (deploy_schedule não será usado para 'remote')."

# Como este script é 'sourced' por deploy.sh,
# usamos 'return' se possível, senão 'exit' (caso alguém rode standalone).
return 0 2>/dev/null || exit 0

