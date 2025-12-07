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
# Suporte ao modo "new":
#   ./deploy.sh remote scripts/instance-info new scripts/experiment-configuration/generate-config.sh
#
# Aqui:
#   - Escolhemos um diretório do tipo deployment-data/remote-0000
#   - Rodamos o generate-config.sh nesse diretório
#   - Removemos "new <script>" dos argumentos antes de chamar initialize-deployment.sh,
#     MAS mantendo o exp_data_dir como 3º argumento.
# --------------------------------------------------------------------
if [ "$1" = "remote" ] && [ "$3" = "new" ]; then
  depl_type="$1"
  instance_info_file="$2"
  new_flag="$3"
  config_gen_script="$4"

  # Se o script não vier no 4º argumento, usa um default.
  if [ -z "$config_gen_script" ]; then
    config_gen_script="scripts/experiment-configuration/generate-config.sh"
  fi

  # Escolhe um diretório deployment-data/remote-XXXX ainda não usado.
  exp_index=0
  while :; do
    candidate=$(printf "%s/remote-%04d" "$deployment_data_root" "$exp_index")
    if [ ! -d "$candidate" ]; then
      exp_data_dir="$candidate"
      break
    fi
    exp_index=$((exp_index + 1))
  done

  mkdir -p "$exp_data_dir"

  echo "Using experiment data directory: $exp_data_dir"
  # exp_id_offset = 0 (primeiro experimento)
  "$config_gen_script" "$exp_data_dir" 0
  if [ $? -ne 0 ]; then
    echo "ERROR: $config_gen_script falhou ao gerar configurações em $exp_data_dir"
    exit 1
  fi

  # *** AQUI ESTAVA O BUG ***
  # Antes: set -- "$depl_type" "$instance_info_file"
  # Agora: passamos também o exp_data_dir para o initialize-deployment.sh
  set -- "$depl_type" "$instance_info_file" "$exp_data_dir"
fi

# --------------------------------------------------------------------
# Inicializa o deployment (lê args e seta:
#  - depl_type
#  - exp_data_dir
#  - deployment_file (deployment.dpl)
#  - csv_filename, etc.)
# --------------------------------------------------------------------
source scripts/initialize-deployment.sh

# --------------------------------------------------------------------
# Se for só inicialização (-i), sai aqui.
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

