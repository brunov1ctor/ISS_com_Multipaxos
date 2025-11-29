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

# generate-master-commands.py expects:
#   1) deployment type (local|remote|cloud)
#   2) deployment file (.dpl)
#   3) output master command template file
#   4) experiment data directory
deploy_schedule=$(
  python3 scripts/generate-master-commands.py \
    "$depl_type" \
    "$deployment_file" \
    "$exp_data_dir/$local_master_command_template_file" \
    "$exp_data_dir"
)

if [ $? -ne 0 ] || [ -z "$deploy_schedule" ]; then
  >&2 echo "remote-deploy.sh: failed processing deployment file: $deployment_file"
  exit 2
fi

