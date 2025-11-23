#!/bin/bash

# This script is not to be ran on its own.
# It is only included as the common part of the different kinds of deployments,
# initializing the experiment, reading command-line arguments and setting some variables.
# It must be sourced, NOT ran even inside those scripts.
#
# It consumes command line parameters and sets the following variables:
# - configuration_generator_script
# - depl_type
# - exp_data_dir
# - new_experiment
# - exp_id_offset
# - deployment_file
# - deploy_schedule
# - instance_info_file
# - cancel_instances
# Some of them are used only internally, some of them are used by the including script.

###############################################################################
# 1) Optional "-c" flag (cancel cloud instances)
###############################################################################

cancel_instances=false
if [ "$1" = "-c" ]; then
  cancel_instances=true
  shift
fi

###############################################################################
# 2) Deployment type: local | cloud | remote
###############################################################################

if [ $# -lt 1 ]; then
  >&2 echo "initialize-deployment.sh: deployment type (local|cloud|remote) is required."
  exit 1
fi

depl_type="$1"
shift

case "$depl_type" in
  local|cloud|remote)
    ;;
  *)
    >&2 echo "initialize-deployment.sh: unknown deployment type: $depl_type (allowed: local, cloud, remote)"
    exit 1
    ;;
esac

###############################################################################
# 3) Instance info file (only for remote / cloud)
###############################################################################

instance_info_file=""

if [ "$depl_type" = "remote" ] || [ "$depl_type" = "cloud" ]; then
  if [ $# -lt 1 ]; then
    >&2 echo "initialize-deployment.sh: instance info file required for deployment type '$depl_type'."
    exit 1
  fi
  instance_info_file="$1"
  shift
fi

###############################################################################
# 4) Experiment data directory OR keyword "new"
###############################################################################

if [ $# -lt 1 ]; then
  >&2 echo "initialize-deployment.sh: 'new' or experiment directory required."
  exit 1
fi

if [ "$1" = "new" ]; then
  new_experiment=true
else
  new_experiment=false
fi

if $new_experiment; then
  # We will create a new experiment directory later
  shift
else
  # Use existing experiment directory
  exp_data_dir="$1"
  shift
  echo "Using experiment data directory: $exp_data_dir"
fi

###############################################################################
# 5) New experiment: create exp_data_dir and generate configs
###############################################################################

if $new_experiment; then
  # Configuration generator script
  if [ $# -lt 1 ]; then
    >&2 echo "initialize-deployment.sh: configuration generator script required after 'new'."
    exit 1
  fi
  configuration_generator_script="$1"
  shift

  # Experiment ID offset (optional)
  if [ $# -ge 1 ]; then
    exp_id_offset="$1"
    shift
  else
    exp_id_offset=0
  fi

  # Defaults from global-vars.sh
  : "${deployment_data_root:=deployment-data}"
  : "${exp_id_digits:=4}"

  case "$depl_type" in
    local)  exp_prefix="local"  ;;
    cloud)  exp_prefix="cloud"  ;;
    remote) exp_prefix="remote" ;;
  esac

  exp_id=$(printf "%0${exp_id_digits}d" "$exp_id_offset")
  exp_data_dir="${deployment_data_root}/${exp_prefix}-${exp_id}"

  echo "Using experiment data directory: $exp_data_dir"
  mkdir -p "$exp_data_dir" || exit 1

  echo "Generated 4 experiments."
  "$configuration_generator_script" "$exp_data_dir" "$exp_id_offset" || exit 1

  # Save a copy of the configuration generator for easier reproducibility
  cp "$configuration_generator_script" "$exp_data_dir"
fi

###############################################################################
# 6) Parse deployment file and generate master command template + schedule
###############################################################################

: "${dpl_filename:=deployment.dpl}"
deployment_file="$exp_data_dir/$dpl_filename"
echo "Using deployment file: $deployment_file"

if [ ! -f "$deployment_file" ]; then
  >&2 echo "initialize-deployment.sh: deployment file not found: $deployment_file"
  exit 1
fi

: "${local_master_command_template_file:=master-commands-template.cmd}"

# generate-master-commands.py expects:
#   1) deployment type (local|cloud|remote)
#   2) deployment file
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

