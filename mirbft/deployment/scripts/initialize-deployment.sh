#!/bin/bash

# ============================================================================
#  initialize-deployment.sh
#
#  ATENÇÃO: este script NÃO deve ser executado diretamente.
#  Ele é feito para ser "sourced" a partir de deploy.sh, deploy-remote.sh etc.
#
#  Ele consome (no máximo) os seguintes parâmetros da linha de comando:
#
#    [-c] <depl_type> [instance_info_file] <new|exp_data_dir> [config_gen_script] [exp_id_offset]
#
#  E define as variáveis:
#    - configuration_generator_script
#    - depl_type
#    - exp_data_dir
#    - new_experiment
#    - exp_id_offset
#    - deployment_file
#    - deploy_schedule
#    - cancel_instances
#    - instance_info_file   (para remote/cloud)
#
#  Obs: este script assume que scripts/global-vars.sh já foi "sourced"
#       (deployment_data_root, dpl_filename, local_master_command_template_file,
#        exp_id_digits etc).
# ============================================================================

# --------------------------------------------------------------------------
# 1) Flag opcional: "-c" (cancelar instâncias de nuvem ao final)
# --------------------------------------------------------------------------
cancel_instances=false
if [ "$1" = "-c" ]; then
  cancel_instances=true
  shift
fi

# --------------------------------------------------------------------------
# 2) Tipo de deployment: local | cloud | remote
# --------------------------------------------------------------------------
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

# --------------------------------------------------------------------------
# 3) Arquivo de instance info (somente para remote / cloud)
#    No seu uso:
#      ./deploy.sh remote scripts/instance-info new scripts/experiment-configuration/generate-config.sh
# --------------------------------------------------------------------------
instance_info_file=""
if [ "$depl_type" = "remote" ] || [ "$depl_type" = "cloud" ]; then
  if [ $# -lt 1 ]; then
    >&2 echo "initialize-deployment.sh: instance info file required for deployment type '$depl_type'."
    exit 1
  fi
  instance_info_file="$1"
  shift
fi

# --------------------------------------------------------------------------
# 4) Exp_data_dir OU palavra-chave "new"
#
#    Caso seja "new", criamos um diretório novo em $deployment_data_root
#    com prefixo de acordo com o tipo (local/cloud/remote) e chamamos o
#    script gerador de configuração.
# --------------------------------------------------------------------------
if [ $# -lt 1 ]; then
  >&2 echo "initialize-deployment.sh: 'new' or experiment directory required."
  exit 1
fi

if [ "$1" = "new" ]; then
  new_experiment=true
  shift

  # Script gerador de configuração
  if [ $# -lt 1 ]; then
    >&2 echo "initialize-deployment.sh: configuration generator script required after 'new'."
    exit 1
  fi
  configuration_generator_script="$1"
  shift

  # Offset opcional do ID do experimento
  if [ $# -ge 1 ]; then
    exp_id_offset="$1"
    shift
  else
    exp_id_offset=0
  fi

  # Prefixo do diretório de experimentos de acordo com o tipo
  case "$depl_type" in
    local)  exp_prefix="local"  ;;
    cloud)  exp_prefix="cloud"  ;;
    remote) exp_prefix="remote" ;;
  esac

  # exp_id_digits vem de scripts/global-vars.sh (padrão 4)
  if [ -z "$exp_id_digits" ]; then
    exp_id_digits=4
  fi

  exp_id=$(printf "%0${exp_id_digits}d" "$exp_id_offset")
  if [ -z "$deployment_data_root" ]; then
    deployment_data_root="deployment-data"
  fi

  exp_data_dir="${deployment_data_root}/${exp_prefix}-${exp_id}"
  echo "Using experiment data directory: $exp_data_dir"
  mkdir -p "$exp_data_dir" || exit 1

  # Gera as configurações (aqui nasce o deployment.dpl, config-000X.yml, etc.)
  "$configuration_generator_script" "$exp_data_dir" "$exp_id_offset" || exit 1

  # Guarda uma cópia do script de geração dentro do diretório para reprodutibilidade
  cp "$configuration_generator_script" "$exp_data_dir" || exit 1

else
  # Reutilizar experimento existente
  new_experiment=false
  exp_data_dir="$1"
  shift

  echo "Using experiment data directory: $exp_data_dir"
fi

# --------------------------------------------------------------------------
# 5) Arquivo de deployment (deployment.dpl dentro de exp_data_dir)
# --------------------------------------------------------------------------
if [ -z "$dpl_filename" ]; then
  dpl_filename="deployment.dpl"
fi

deployment_file="$exp_data_dir/$dpl_filename"
echo "Using deployment file: $deployment_file"

if [ ! -f "$deployment_file" ]; then
  >&2 echo "initialize-deployment.sh: deployment file not found: $deployment_file"
  exit 1
fi

# --------------------------------------------------------------------------
# 6) Geração do master-commands-template e do deploy_schedule
#
#    generate-master-commands.py espera:
#      1) tipo de deployment (local|cloud|remote)
#      2) arquivo de deployment (.dpl)
#      3) arquivo de saída (template de comandos do master)
#      4) diretório base do experimento
#
#    Ele imprime o deploy_schedule em stdout, que capturamos na variável.
# --------------------------------------------------------------------------
if [ -z "$local_master_command_template_file" ]; then
  local_master_command_template_file="master-commands-template.cmd"
fi

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

