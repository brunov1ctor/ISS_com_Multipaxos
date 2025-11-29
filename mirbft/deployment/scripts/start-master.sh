#!/bin/bash

# start-master.sh
#
# Script responsável por:
#   - Copiar o arquivo de master-commands e configs para o master
#   - Garantir diretórios remotos no master (iss, experiment-output, etc.)
#   - Iniciar o discoverymaster + orderingclient para rodar os experimentos
#   - (Opcional) Iniciar o analisador contínuo (analyze-continuously.sh)
#
# Este script é chamado localmente pelo deploy-remote.sh, mas
# ele gerencia o master via SSH.

set -euo pipefail

# Diretório de dados do experimento (ex: deployment-data/remote-0000)
exp_data_dir="$1"
# IP (ou hostname) do master (ex: 172.20.150.1)
master_ip="$2"

# Carrega variáveis globais (inclui remote_work_dir, remote_exp_dir, remote_ready_file, etc.)
source scripts/global-vars.sh

# Arquivos locais de interesse
local_master_cmd="${exp_data_dir}/master-commands.cmd"
local_deployment_csv="${exp_data_dir}/deployment.csv"
local_deployment_dpl="${exp_data_dir}/deployment.dpl"
local_config_dir="${exp_data_dir}/config"

# Arquivos remotos (no master)
remote_work_dir="${remote_work_dir:-/users/$USER/iss}"
remote_exp_dir="${remote_exp_dir:-${remote_work_dir}/current-deployment-data}"
remote_raw_results_dir="${remote_raw_results_dir:-${remote_exp_dir}/raw-results}"
remote_master_cmd="${remote_work_dir}/master-commands.cmd"
remote_status_file="${remote_status_file:-${remote_work_dir}/status}"
# *** IMPORTANTE: usar a MESMA variável que o fetch-results.sh usa (remote_ready_file) ***
remote_ready_file="${remote_ready_file:-${remote_work_dir}/master-ready}"

# Opções SSH mais "amigáveis" para deploy automático
ssh_opts="-o StrictHostKeyChecking=accept-new"

echo "Using experiment data directory: ${exp_data_dir}"
echo "Using master IP: ${master_ip}"
echo "Local master command script: ${local_master_cmd}"
echo "Remote work dir: ${remote_work_dir}"
echo "Remote READY file: ${remote_ready_file}"
echo

# ----------------------------------------------------------------------
# Verificações básicas
# ----------------------------------------------------------------------
if [[ ! -f "${local_master_cmd}" ]]; then
  echo "ERRO: Arquivo de comandos do master não encontrado: ${local_master_cmd}"
  exit 1
fi

if [[ ! -f "${local_deployment_csv}" ]]; then
  echo "ERRO: deployment.csv não encontrado em ${exp_data_dir}"
  exit 1
fi

if [[ ! -f "${local_deployment_dpl}" ]]; then
  echo "ERRO: deployment.dpl não encontrado em ${exp_data_dir}"
  exit 1
fi

if [[ ! -d "${local_config_dir}" ]]; then
  echo "ERRO: diretório de config não encontrado em ${local_config_dir}"
  exit 1
fi

# ----------------------------------------------------------------------
# Copia master-commands + configs para o master
# ----------------------------------------------------------------------
echo "Using pre-generated master command script at ${local_master_cmd}."
echo "Master command script written to ${local_master_cmd}."
echo

echo "Copying master commands and configs to master."

# Garante que o diretório scripts/ exista no master
echo "Ensuring scripts/ on master (${master_ip})."
ssh ${ssh_opts} "Bruno@${master_ip}" "
  mkdir -p '${remote_work_dir}/scripts'
" >/dev/null 2>&1

# Copia diretório scripts/ (inclui analyze/, etc.)
scp -r ${ssh_opts} \
  scripts \
  "Bruno@${master_ip}:${remote_work_dir}/" >/dev/null 2>&1

# Garante permissão de execução nos scripts
ssh ${ssh_opts} "Bruno@${master_ip}" "
  chmod +x '${remote_work_dir}/scripts/'*.sh 2>/dev/null || true
  chmod +x '${remote_work_dir}/scripts/'*/*.sh 2>/dev/null || true
" >/dev/null 2>&1

# Copia master-commands.cmd
scripts/stubborn-scp.sh 10 \
  "${local_master_cmd}" \
  "Bruno@${master_ip}:${remote_master_cmd}"

# Copia deployment.csv e deployment.dpl
scripts/stubborn-scp.sh 10 \
  "${local_deployment_csv}" \
  "Bruno@${master_ip}:${remote_exp_dir}/deployment.csv"

scripts/stubborn-scp.sh 10 \
  "${local_deployment_dpl}" \
  "Bruno@${master_ip}:${remote_exp_dir}/deployment.dpl"

# Copia arquivos de config/*.yml
for cfg in "${local_config_dir}"/config-*.yml; do
  [ -f "$cfg" ] || continue
  cfg_base="$(basename "$cfg")"
  remote_cfg="iss/experiment-config/${cfg_base}"
  echo "Copying ${cfg} to master as ${remote_cfg}"
  scripts/stubborn-scp.sh 10 \
    "${cfg}" \
    "Bruno@${master_ip}:${remote_cfg}"
done

echo "Done."
echo

# ----------------------------------------------------------------------
# Garante diretórios no master
# ----------------------------------------------------------------------
echo "Ensuring remote directories on master (${master_ip})."

ssh ${ssh_opts} "Bruno@${master_ip}" "
  mkdir -p '${remote_work_dir}'
  mkdir -p '${remote_exp_dir}'
  mkdir -p '${remote_raw_results_dir}'
  mkdir -p \"\$(dirname '${remote_status_file}')\"
  echo 'RUNNING' > '${remote_status_file}'
  # limpa READY antigo, se existir
  rm -f '${remote_ready_file}'
" >/dev/null 2>&1

echo "Remote directories ensured."
echo

# ----------------------------------------------------------------------
# Start do discoverymaster + orderingclient NO MASTER
# ----------------------------------------------------------------------
echo "Starting result processor and master server."

ssh ${ssh_opts} "Bruno@${master_ip}" "
  export GOPATH='/users/Bruno/go'
  export GOBIN='/users/Bruno/go/bin'
  export PATH=\"\$GOBIN:/usr/local/go/bin:\$PATH\"

  cd '${remote_work_dir}' || exit 1

  # PATH inclui tanto scripts/ quanto deployment/scripts (por compatibilidade)
  export PATH=\"${remote_work_dir}/scripts:${remote_work_dir}/deployment/scripts:\$PATH\"

  # Marca que o master está pronto (para fetch-results.sh)
  echo \"start-master.sh (remote): writing READY flag to ${remote_ready_file}\"
  echo 'READY' > '${remote_ready_file}'

  # Log de verificação
  ls -l '${remote_ready_file}' 2>/dev/null || echo 'start-master.sh (remote): WARNING: READY file not found after write'

  # Inicia o discoverymaster + orderingclient em background,
  # gravando logs em current-deployment-data/master-*.log
  nohup discoverymaster peers 0.0.0.0:9999 > '${remote_exp_dir}/master-discovery.log' 2>&1 &

  nohup orderingclient \
    '${remote_master_cmd}' \
    > '${remote_exp_dir}/master.log' 2>&1 &

" >/dev/null 2>&1

echo "Master discovery + orderingclient disparados."
echo

# ----------------------------------------------------------------------
# (Opcional) Inicia o analisador contínuo NO MASTER
# OBS: se você preferir analisar no node-0, pode comentar este bloco.
# ----------------------------------------------------------------------
ssh ${ssh_opts} "Bruno@${master_ip}" "
  export GOPATH='/users/Bruno/go'
  export GOBIN='/users/Bruno/go/bin'
  export PATH=\"\$GOBIN:/usr/local/go/bin:\$PATH\"

  cd '${remote_work_dir}' || exit 1

  if [ ! -x '${remote_work_dir}/scripts/analyze/analyze-continuously.sh' ]; then
    echo 'ERRO: ${remote_work_dir}/scripts/analyze/analyze-continuously.sh não encontrado ou não executável.' >&2
    exit 1
  fi

  # IMPORTANTE: aqui a chamada ainda está simplificada e não segue a assinatura
  # completa do analyze-continuously.sh. Se quiser usar análise contínua de fato,
  # depois ajustamos os parâmetros. Por enquanto, isso pode rodar só para debug/log.
  nohup '${remote_work_dir}/scripts/analyze/analyze-continuously.sh' \
    '${remote_raw_results_dir}' \
    '${remote_exp_dir}' \
    > '${remote_exp_dir}/continuous-analysis.log' 2>&1 &
" >/dev/null 2>&1 || true

echo "Continuous analysis (remote) disparada (ou ignorada em caso de erro)."
echo
echo "start-master.sh finished."

