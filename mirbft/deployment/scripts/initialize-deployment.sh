#!/bin/bash

# scripts/initialize-deployment.sh
#
# Shared deployment initialization logic, reused by deploy.sh for local,
# cloud, and remote deployments.

set -euo pipefail

# Load global variables shared by all deployment scripts.
source scripts/global-vars.sh

# Defaults for generated files
: "${dpl_filename:=deployment.dpl}"
: "${csv_filename:=deployment.csv}"
: "${exp_id_digits:=4}"

configuration_generator_script=""
depl_type=""
exp_data_dir=""
new_experiment=false
exp_id_offset=0
deployment_file=""
deploy_schedule=""
instance_info_file=""
cancel_instances=false

# Helper: print usage and exit
usage() {
  >&2 echo "Usage: $0 <local|cloud|remote> [args ...]"
  >&2 echo
  >&2 echo "Examples:"
  >&2 echo "  $0 local new scripts/experiment-configuration/generate-config.sh"
  >&2 echo "  $0 cloud scripts/cloud-instance-info new scripts/experiment-configuration/generate-config.sh"
  >&2 echo "  $0 remote scripts/instance-info new scripts/experiment-configuration/generate-config.sh"
  exit 1
}

log_info() {
  echo "[initialize-deployment] $*"
}

# --------------------------------------------------------------------
# Parse first argument: deployment type
# --------------------------------------------------------------------
if [ -z "${1:-}" ]; then
  usage
fi

depl_type="$1"
shift || true

log_info "depl_type = ${depl_type}"

case "$depl_type" in
  local)
    new_experiment=true
    if [ "${1:-}" = "new" ]; then
      shift || true
      new_experiment=true
    else
      new_experiment=false
    fi

    if $new_experiment; then
      exp_data_dir="${deployment_data_root}/local-0000"
    else
      if [ -z "${1:-}" ]; then
        >&2 echo "initialize-deployment.sh: local deployment requires existing experiment data dir or 'new'"
        exit 1
      fi
      exp_data_dir="$1"
      shift || true
    fi

    if [ -n "${1:-}" ]; then
      configuration_generator_script="$1"
      shift || true
    fi

    instance_info_file=""
    cancel_instances=false
    ;;

  cloud)
    if [ -z "${1:-}" ]; then
      >&2 echo "initialize-deployment.sh: cloud deployment requires cloud-instance-info file path"
      exit 1
    fi
    instance_info_file="$1"
    shift || true

    if [ "${1:-}" = "new" ]; then
      shift || true
      new_experiment=true
      exp_data_dir="${deployment_data_root}/cloud-0000"
    else
      new_experiment=false
      if [ -z "${1:-}" ]; then
        >&2 echo "initialize-deployment.sh: cloud deployment requires existing experiment data dir or 'new'"
        exit 1
      fi
      exp_data_dir="$1"
      shift || true
    fi

    if [ -n "${1:-}" ]; then
      configuration_generator_script="$1"
      shift || true
    fi

    cancel_instances=true
    ;;

  remote)
    if [ -z "${1:-}" ]; then
      >&2 echo "initialize-deployment.sh: remote deployment requires instance-info file path"
      exit 1
    fi
    instance_info_file="$1"
    shift || true

    if [ "${1:-}" = "new" ]; then
      shift || true
      new_experiment=true
      exp_data_dir="${deployment_data_root}/remote-0000"
    else
      new_experiment=false
      if [ -z "${1:-}" ]; then
        >&2 echo "initialize-deployment.sh: remote deployment requires existing experiment data dir or 'new'"
        exit 1
      fi
      exp_data_dir="$1"
      shift || true
    fi

    if [ -n "${1:-}" ]; then
      configuration_generator_script="$1"
      shift || true
    fi

    cancel_instances=false
    ;;

  *)
    >&2 echo "initialize-deployment.sh: unknown deployment type: $depl_type (allowed: local, cloud, remote)"
    exit 1
    ;;
esac

log_info "instance_info_file = ${instance_info_file:-<none>}"
log_info "new_experiment      = ${new_experiment}"
log_info "exp_data_dir        = ${exp_data_dir}"
log_info "config_gen_script   = ${configuration_generator_script:-<none>}"

# --------------------------------------------------------------------
# Setup experiment directory
# --------------------------------------------------------------------
if $new_experiment; then
  mkdir -p "$exp_data_dir"
else
  if [ ! -d "$exp_data_dir" ]; then
    >&2 echo "initialize-deployment.sh: experiment data directory '$exp_data_dir' does not exist"
    exit 1
  fi
fi

deployment_file="$exp_data_dir/$dpl_filename"

# If new experiment, clear old configs
if $new_experiment; then
  rm -f "$deployment_file" "$exp_data_dir/$csv_filename"
  rm -rf "$exp_data_dir/config"
fi

# --------------------------------------------------------------------
# Determine exp_id_offset if old deployment file exists
# --------------------------------------------------------------------
if [ -f "$deployment_file" ]; then
  last_line=$(grep '^run ' "$deployment_file" | tail -n 1 || true)
  if [ -n "${last_line:-}" ]; then
    expID=$(echo "$last_line" | awk '{print $2}')
    if [ -n "${expID:-}" ]; then
      exp_id_offset=$((expID + 1))
    fi
  fi
else
  exp_id_offset=0
fi

log_info "deployment_file     = ${deployment_file}"
log_info "exp_id_offset       = ${exp_id_offset}"

# --------------------------------------------------------------------
# Export variables so deploy.sh and other scripts can use them
# --------------------------------------------------------------------
export configuration_generator_script
export depl_type
export exp_data_dir
export new_experiment
export exp_id_offset
export deployment_file
export instance_info_file
export cancel_instances
export dpl_filename
export csv_filename
export exp_id_digits

