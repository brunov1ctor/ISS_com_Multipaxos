#!/bin/bash

# deploy.sh
# --------------------------------------------------------------------
# Script principal de deployment (local, cloud, remote).
# Para REMOTE:
#   ./deploy.sh remote scripts/instance-info new scripts/experiment-configuration/generate-config.sh
#
# Este script agora:
#   1) Inicializa o experimento (initialize-deployment.sh)
#   2) Faz o deploy (local/cloud/remote)
#   3) SE for remote:
#        - busca resultados do master/slaves (fetch-results.sh)
#        - roda analyze.sh em cada experimento
#   4) Gera o result-summary.csv com summarize.sh
# --------------------------------------------------------------------

source scripts/global-vars.sh

# Kill all children of this script when exiting
trap "$trap_exit_command" EXIT

# The '-i' or '--init-only' flag makes the script exit after locally initializing the deployment, without running it.
if [ "${1-}" = "-i" ] || [ "${1-}" = "--init-only" ]; then
  init_only=true
  shift
else
  init_only=false
fi

# Initializes the deployment.
# This sets, among others:
#   depl_type, exp_data_dir, deployment_file,
#   instance_info_file, csv_filename, result_summary_file, exp_id_digits, ...
source scripts/initialize-deployment.sh "$@"

if $init_only; then
  echo "Done. Experiment data directory: $exp_data_dir"
  exit 0
fi

# --------------------------------------------------------------------
# 1) Executa o deploy conforme o tipo
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
# 2) Para REMOTE: buscar resultados e rodar análise automaticamente
# --------------------------------------------------------------------
if [ "$depl_type" = "remote" ]; then
  echo "============================================================"
  echo "Fetching and analyzing remote experiment results..."
  echo "  exp_data_dir       = $exp_data_dir"
  echo "  csv_filename       = $csv_filename"
  echo "  result_summary_file= $result_summary_file"
  echo "============================================================"

  # Garante um valor default para instance_info_file, se não tiver sido setado
  if [ -z "${instance_info_file:-}" ]; then
    instance_info_file="scripts/instance-info"
  fi

  # Descobre o IP do master a partir do instance-info:
  # Formato esperado das linhas:
  #   node-0  <ctrl_ip>  <exp_ip>  master  master
  master_ip=""
  if [ -f "$instance_info_file" ]; then
    master_ip=$(awk 'NF>=5 && $4=="master"{print $2; exit}' "$instance_info_file")
  fi

  if [ -z "$master_ip" ]; then
    echo "WARNING: could not determine master IP from $instance_info_file; skipping automatic fetch/analyze." >&2
  else
    echo "  Using master IP: $master_ip"

    # 2.1) Busca logs do master/slaves (inclui fallback pros slaves)
    echo "  [step] Fetching raw logs from master/slaves..."
    scripts/fetch-results.sh "$master_ip" "$exp_data_dir" | tee "$exp_data_dir/$local_result_fetching_log"

    # 2.2) Descobre quantos experimentos existem pelo deployment.csv
    if [ -f "$exp_data_dir/$csv_filename" ]; then
      num_exps=$(($(wc -l < "$exp_data_dir/$csv_filename") - 1))
    else
      num_exps=0
    fi

    echo "  [info] Number of experiments detected: $num_exps"

    # 2.3) Roda analyze.sh em cada experimento 0000..(num_exps-1)
    if [ "$num_exps" -gt 0 ]; then
      echo "  [step] Running analyze.sh for each experiment..."

      exp_id=0
      while [ "$exp_id" -lt "$num_exps" ]; do
        printf -v exp_suffix "%0${exp_id_digits}d" "$exp_id"
        exp_dir="$exp_data_dir/experiment-output/$exp_suffix"

        if [ -d "$exp_dir" ]; then
          echo "    [analyze] $exp_dir"
          scripts/analyze/analyze.sh "$exp_dir" || \
            echo "    [warn] analyze.sh failed for $exp_dir" >&2
        else
          echo "    [warn] experiment directory not found: $exp_dir" >&2
        fi

        exp_id=$((exp_id+1))
      done
    else
      echo "  [warn] No experiments detected in $exp_data_dir/$csv_filename (no analysis performed)." >&2
    fi
  fi

  echo "Finished fetching and analyzing remote results."
  echo "============================================================"
fi

# --------------------------------------------------------------------
# 3) Gera result-summary.csv (parâmetros + métricas, se existirem)
# --------------------------------------------------------------------
echo "Generating result summary."
scripts/analyze/summarize.sh \
  "$exp_data_dir/$csv_filename" \
  "$exp_data_dir/experiment-output" \
  2> /dev/null | tee "$exp_data_dir/$result_summary_file"

echo "Done. Experiment data directory: $exp_data_dir"

