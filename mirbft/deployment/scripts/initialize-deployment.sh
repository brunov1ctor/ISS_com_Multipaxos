#!/bin/bash
# Este script NÃO deve ser executado diretamente.
# Ele é sempre "sourced" por deploy.sh e outros scripts.
#
# Consome (da linha de comando do deploy.sh) até 4 parâmetros:
#   ./deploy.sh <depl_type> [instance-info] <existing|new> <config-generator.sh>
#
# Define as variáveis globais:
#   configuration_generator_script
#   depl_type
#   exp_data_dir
#   new_experiment
#   exp_id_offset
#   deployment_file
#   deploy_schedule
#   cancel_instances
#   instance_info_file   (para 'remote')
#
# DEPENDE de scripts/global-vars.sh já ter sido carregado.

# -----------------------------------------------------------------------------
# 1) Flag opcional "-c" (cancel instances)
# -----------------------------------------------------------------------------
if [ "${1-}" = "-c" ]; then
  cancel_instances=true
  shift
else
  cancel_instances=false
fi

# -----------------------------------------------------------------------------
# 2) Tipo de deploy: local | cloud | remote
# -----------------------------------------------------------------------------
if [ $# -lt 1 ]; then
  echo "initialize-deployment.sh: missing deployment type (local|cloud|remote)" >&2
  return 1
fi

depl_type="$1"
shift

case "$depl_type" in
  local|cloud)
    instance_info_file=""
    ;;
  remote)
    if [ $# -lt 1 ]; then
      echo "initialize-deployment.sh: remote deployment requires <instance-info> argument" >&2
      return 1
    fi
    instance_info_file="$1"
    shift
    ;;
  *)
    echo "initialize-deployment.sh: unknown deployment type: $depl_type (allowed: local, cloud, remote)" >&2
    return 1
    ;;
esac

# -----------------------------------------------------------------------------
# 3) existing | new
# -----------------------------------------------------------------------------
if [ $# -lt 1 ]; then
  echo "initialize-deployment.sh: missing <existing|new> argument" >&2
  return 1
fi

exp_mode="$1"
shift

# Garante que deployment_data_root e exp_id_digits existam (caso global-vars.sh falhe)
: "${deployment_data_root:=deployment-data}"
: "${exp_id_digits:=4}"

mkdir -p "$deployment_data_root"

if [ "$exp_mode" = "existing" ]; then
  # ---------------------------------------------------------------------------
  # Reusar experimento existente
  # ---------------------------------------------------------------------------
  new_experiment=false
  configuration_generator_script=""

  prefix="$depl_type-"

  latest=""
  for d in "$deployment_data_root"/${prefix}[0-9][0-9][0][0-9]; do
    [ -d "$d" ] || continue
    latest="$d"
  done

  if [ -z "$latest" ]; then
    echo "initialize-deployment.sh: no existing experiment directory for type '$depl_type' under '$deployment_data_root'" >&2
    return 1
  fi

  exp_data_dir="$latest"
  exp_id_offset=0

elif [ "$exp_mode" = "new" ]; then
  # ---------------------------------------------------------------------------
  # Criar novo experimento
  # ---------------------------------------------------------------------------
  new_experiment=true

  if [ $# -lt 1 ]; then
    echo "initialize-deployment.sh: new deployment requires <config-generator.sh>" >&2
    return 1
  fi

  configuration_generator_script="$1"
  shift

  if [ ! -x "$configuration_generator_script" ]; then
    echo "initialize-deployment.sh: configuration generator not found or not executable: $configuration_generator_script" >&2
    return 1
  fi

  # exp_id_offset opcional
  if [ $# -ge 1 ]; then
    exp_id_offset="$1"
    shift
  else
    exp_id_offset=0
  fi

  # Escolhe o próximo diretório: <depl_type>-NNNN
  max_id=-1
  for d in "$deployment_data_root"/"$depl_type"-[0-9][0-9][0-9][0-9]; do
    [ -d "$d" ] || continue
    base="${d##*/}"             # ex: remote-0003
    num="${base#"$depl_type"-}" # ex: 0003
    if [[ "$num" =~ ^[0-9]+$ ]] && [ "$num" -gt "$max_id" ]; then
      max_id="$num"
    fi
  done

  next=$((max_id + 1))
  printf -v exp_suffix "%0${exp_id_digits}d" "$next"
  exp_data_dir="$deployment_data_root/$depl_type-$exp_suffix"

  echo "Using experiment data directory: $exp_data_dir"
  mkdir -p "$exp_data_dir" || return 1

  # Gera arquivos de configuração (inclui deployment.dpl)
  "$configuration_generator_script" "$exp_data_dir" "$exp_id_offset" || return 1

  # Salva cópia do gerador no diretório do experimento
  cp "$configuration_generator_script" "$exp_data_dir" || true

else
  echo "initialize-deployment.sh: <existing|new> must be 'existing' or 'new'" >&2
  return 1
fi

# -----------------------------------------------------------------------------
# 4) Caminho para o arquivo .dpl
# -----------------------------------------------------------------------------
: "${dpl_filename:=deployment.dpl}"
deployment_file="$exp_data_dir/$dpl_filename"
echo "Using deployment file: $deployment_file"

# -----------------------------------------------------------------------------
# 5) deploy_schedule (apenas para local/cloud)
# -----------------------------------------------------------------------------
deploy_schedule=""

if [ "$depl_type" = "local" ] || [ "$depl_type" = "cloud" ]; then
  # Para manter compatibilidade com o comportamento original
  : "${local_master_command_template_file:=master-commands-template.cmd}"

  if [ -x "scripts/generate-master-commands.py" ]; then
    echo "initialize-deployment.sh: generating master commands (local/cloud)."
    deploy_schedule=$(
      python3 scripts/generate-master-commands.py \
        "$depl_type" \
        "$deployment_file" \
        "$exp_data_dir/$local_master_command_template_file" \
        "$exp_data_dir"
    )
    if [ $? -ne 0 ] || [ -z "$deploy_schedule" ]; then
      >&2 echo "initialize-deployment.sh: failed processing deployment file: $deployment_file"
      return 2
    fi
  fi
else
  # remote: deploy_schedule não é usado
  deploy_schedule=""
fi

# -----------------------------------------------------------------------------
# LOG DE DEBUG
# -----------------------------------------------------------------------------
echo "initialize-deployment.sh: debug info:"
echo "  depl_type          = ${depl_type:-<unset>}"
echo "  exp_data_dir       = ${exp_data_dir:-<unset>}"
echo "  new_experiment     = ${new_experiment:-<unset>}"
echo "  exp_id_offset      = ${exp_id_offset:-<unset>}"
echo "  deployment_file    = ${deployment_file:-<unset>}"
echo "  instance_info_file = ${instance_info_file:-<unset>}"
echo "  cancel_instances   = ${cancel_instances:-<unset>}"
echo "  deploy_schedule    = ${deploy_schedule:-<unset>}"

