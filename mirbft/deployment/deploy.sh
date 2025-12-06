#!/bin/bash

# --------------------------------------------------------------------
# Bootstrap de permissões:
# Garante que todos os scripts locais tenham permissão de execução
# (incluindo scripts/stubborn-scp.sh) antes de qualquer uso.
# --------------------------------------------------------------------
if [ -d "scripts" ]; then
  chmod +x scripts/*.sh 2>/dev/null || true
  chmod +x scripts/*/*.sh 2>/dev/null || true
fi

# Carrega variáveis globais compartilhadas pelos scripts de deployment.
# (define trap_exit_command, csv_filename, etc.)
source scripts/global-vars.sh

# Mata todos os filhos deste script ao sair (usa trap_exit_command do global-vars.sh).
trap "$trap_exit_command" EXIT

# A flag '-i' ou '--init-only' faz o script sair após inicializar o deployment,
# sem de fato executar nada nos nós.
if [ "$1" = "-i" ] || [ "$1" = "--init-only" ]; then
  init_only=true
  shift
else
  init_only=false
fi

# Inicializa o deployment.
# Este trecho é separado porque é reutilizado por outros tipos de deployment.
# Ele consome vários parâmetros de linha de comando e define as variáveis:
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

# Se for apenas inicialização, sai aqui.
if $init_only; then
  echo "Init only."
  echo "Experiment directory: $exp_data_dir"
  exit 0
fi

# Inicia o deployment dependendo do tipo (local / cloud / remote)
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

