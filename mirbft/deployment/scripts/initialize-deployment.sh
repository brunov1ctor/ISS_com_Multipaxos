#!/bin/bash
# initialize-deployment.sh
# --------------------------------------------------------------------
# Script comum de inicialização para todos os tipos de deploy.
# Deve ser SEMPRE usado via:  source scripts/initialize-deployment.sh "$@"
# NUNCA chamado diretamente (./scripts/initialize-deployment.sh).
#
# Consome os parâmetros vindos do deploy.sh:
#   ./deploy.sh <depl_type> [instance-info] <existing|new> <config-generator.sh> [exp_id_offset]
#
# Onde:
#   depl_type       : local | cloud | remote
#   instance-info   : (apenas para remote) caminho do scripts/instance-info
#   existing|new    : reutiliza dir existente OU gera experimento novo
#   config-generator: script de geração (ex: scripts/experiment-configuration/generate-config.sh)
#   exp_id_offset   : opcional, inteiro (default 0)
#
# Define (entre outras) as variáveis globais:
#   depl_type
#   exp_data_dir
#   new_experiment
#   exp_id_offset
#   deployment_file
#   deploy_schedule
#   instance_info_file
#   configuration_generator_script
#   cancel_instances
#
# Requer que scripts/global-vars.sh já tenha sido source'ado antes.

# --------------------------------------------------------------------
# 0) Flag opcional "-c" (cancel instances)
# --------------------------------------------------------------------
if [ "${1-}" = "-c" ]; then
  cancel_instances=true
  shift
else
  cancel_instances=false
fi

# --------------------------------------------------------------------
# 1) Tipo de deploy
# --------------------------------------------------------------------
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

# --------------------------------------------------------------------
# 2) existing | new
# --------------------------------------------------------------------
if [ $# -lt 1 ]; then
  echo "initialize-deployment.sh: missing <existing|new> argument" >&2
  return 1
fi

exp_mode="$1"
shift

# Garante defaults caso global-vars.sh não tenha sido carregado por algum motivo
: "${deployment_data_root:=deployment-data}"
: "${dpl_filename:=deployment.dpl}"
: "${csv_filename:=deployment.csv}"
: "${exp_id_digits:=4}"
: "${local_master_command_template_file:=master-commands-template.cmd}"

mkdir -p "$deployment_data_root"

new_experiment=false
configuration_generator_script=""
exp_id_offset=0

# --------------------------------------------------------------------
# 3) Escolha / criação do diretório de experimento
# --------------------------------------------------------------------
if [ "$exp_mode" = "existing" ]; then
  # Reutiliza um diretório existente (deve ser passado explicitamente)
  if [ $# -lt 1 ]; then
    echo "initialize-deployment.sh: existing deployment requires <exp_data_dir> argument" >&2
    return 1
  fi
  exp_data_dir="$1"
  shift
  new_experiment=false

elif [ "$exp_mode" = "new" ]; then
  new_experiment=true

  if [ $# -lt 1 ]; then
    echo "initialize-deployment.sh: new deployment requires <config-generator.sh> argument" >&2
    return 1
  fi

  configuration_generator_script="$1"
  shift

  if [ ! -x "$configuration_generator_script" ]; then
    echo "initialize-deployment.sh: configuration generator not found or not executable: $configuration_generator_script" >&2
    return 1
  fi

  if [ $# -ge 1 ]; then
    exp_id_offset="$1"
    shift
  else
    exp_id_offset=0
  fi

  # Cria diretório de experimento seguindo o padrão <depl_type>-NNNN
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

  mkdir -p "$exp_data_dir" || return 1

  echo "Using experiment data directory: $exp_data_dir"

  # Gera deployment.dpl + configs
  "$configuration_generator_script" "$exp_data_dir" "$exp_id_offset" || return 1

  # Guarda cópia do script usado
  cp "$configuration_generator_script" "$exp_data_dir" 2>/dev/null || true

else
  echo "initialize-deployment.sh: <existing|new> must be 'existing' or 'new'" >&2
  return 1
fi

# Se for existing, loga o diretório para manter compatível com o que você já estava vendo
if [ "$exp_mode" = "existing" ]; then
  echo "Using experiment data directory: $exp_data_dir"
fi

# --------------------------------------------------------------------
# 4) Arquivo de deployment (.dpl)
# --------------------------------------------------------------------
deployment_file="$exp_data_dir/$dpl_filename"
echo "Using deployment file: $deployment_file"

# --------------------------------------------------------------------
# 5) Geração de master-commands-template.cmd (LOG PESADO AQUI)
# --------------------------------------------------------------------
deploy_schedule=""

echo "initialize-deployment.sh: about to generate master commands:"
echo "  depl_type                     = $depl_type"
echo "  deployment_file (.dpl)        = $deployment_file"
echo "  local_master_command_template = $local_master_command_template_file"
echo "  output template path          = $exp_data_dir/$local_master_command_template_file"

if [ -f "scripts/generate-master-commands.py" ]; then
  echo "initialize-deployment.sh: calling: python3 scripts/generate-master-commands.py \\"
  echo "  $depl_type \\"
  echo "  $deployment_file \\"
  echo "  $exp_data_dir/$local_master_command_template_file \\"
  echo "  $exp_data_dir"

  deploy_schedule=$(python3 scripts/generate-master-commands.py \
      "$depl_type" \
      "$deployment_file" \
      "$exp_data_dir/$local_master_command_template_file" \
      "$exp_data_dir")
  status=$?

  echo "initialize-deployment.sh: generate-master-commands.py exit code = $status"
  echo "initialize-deployment.sh: deploy_schedule (raw) = '${deploy_schedule}'"

  if [ $status -ne 0 ]; then
    >&2 echo "initialize-deployment.sh: failed processing deployment file: $deployment_file"
    return 2
  fi
else
  echo "initialize-deployment.sh: WARNING: scripts/generate-master-commands.py not found; skipping master command generation."
fi

# Para 'remote', o deploy_schedule em si não é usado, mas o arquivo
# master-commands-template.cmd PRECISA existir para o deploy-remote.sh.
if [ "$depl_type" = "remote" ]; then
  if [ ! -f "$exp_data_dir/$local_master_command_template_file" ]; then
    echo "initialize-deployment.sh: ERROR: expected '$exp_data_dir/$local_master_command_template_file' to exist, but it does not." >&2
    echo "initialize-deployment.sh: ls -l '$exp_data_dir':"
    ls -l "$exp_data_dir" 2>&1 || true
    return 2
  fi
  # Garantimos que deploy_schedule fique definido, ainda que vazio.
  deploy_schedule=""
fi

# --------------------------------------------------------------------
# LOG FINAL
# --------------------------------------------------------------------
echo "initialize-deployment.sh: debug info:"
echo "  depl_type          = ${depl_type:-<unset>}"
echo "  exp_data_dir       = ${exp_data_dir:-<unset>}"
echo "  new_experiment     = ${new_experiment:-<unset>}"
echo "  exp_id_offset      = ${exp_id_offset:-<unset>}"
echo "  deployment_file    = ${deployment_file:-<unset>}"
echo "  instance_info_file = ${instance_info_file:-<unset>}"
echo "  cancel_instances   = ${cancel_instances:-<unset>}"
echo "  deploy_schedule    = ${deploy_schedule:-<unset>}"

