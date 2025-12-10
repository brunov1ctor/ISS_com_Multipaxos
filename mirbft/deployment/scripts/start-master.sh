#!/bin/bash
#
# start-master.sh
#
# Script responsável por:
#  - Copiar master-commands.cmd e arquivos de deployment para o master remoto
#  - Garantir diretórios remotos
#  - Gerar o script remoto que dispara discoverymaster + orderingclient
#  - Iniciar o master remoto e deixar o resto do deploy seguir
#

set -euo pipefail

source scripts/global-vars.sh

# Mata os filhos ao sair
trap "$trap_exit_command" EXIT

if [[ $# -lt 2 ]]; then
  echo "Uso: $0 <exp_data_dir> <master_ip>"
  exit 1
fi

exp_data_dir="$1"
master_ip="$2"

echo "Using experiment data directory: ${exp_data_dir}"
echo "Using master IP: ${master_ip}"

###################################################
# Arquivos locais e caminhos remotos
###################################################

# Script de comandos do master gerado pelo initialize-deployment.sh
local_master_cmd="${exp_data_dir}/master-commands.cmd"

# Arquivos de deployment (CSV e DPL) gerados pelo generate-config
local_deployment_csv="${exp_data_dir}/${csv_filename}"
local_deployment_dpl="${exp_data_dir}/${dpl_filename}"

# Diretórios remotos (com defaults defensivos caso algo não venha de global-vars.sh)
remote_work_dir="${remote_work_dir:-/users/${remote_user}/iss}"
remote_exp_dir="${remote_exp_dir:-${remote_work_dir}/current-deployment-data}"
remote_config_dir="${remote_config_dir:-${remote_work_dir}/experiment-config}"

# Caminho remoto preferido do master-commands.cmd (pode ser sobrescrito via remote_master_command_file)
remote_master_cmd="${remote_master_command_file:-${remote_work_dir}/master-commands.cmd}"

# Arquivos de log/ready remotos
remote_main_log="${remote_main_log:-${remote_work_dir}/main_log.log}"
remote_ready_file="${remote_ready_file:-${remote_work_dir}/master-ready}"

# Arquivo de script remoto (corpo do user-script)
remote_user_script_body="${remote_work_dir}/start-master-remote.sh"
remote_user_script_uploaded="${remote_work_dir}/start-master-remote.uploaded"

echo "Local master command script: ${local_master_cmd}"
echo "Remote work dir: ${remote_work_dir}"
echo "Remote master command path: ${remote_master_cmd}"
echo

# Verificação simples de existência do master-commands.cmd
if [[ ! -f "${local_master_cmd}" ]]; then
  echo "AVISO: Arquivo de comandos do master não encontrado localmente: ${local_master_cmd}"
  echo "       Vou prosseguir assim mesmo; certifique-se de que o master-commands.cmd já exista no master."
fi

###################################################
# Garante diretórios remotos
###################################################

echo "Ensuring remote directories on master (${master_ip})."

ssh ${ssh_options} "${remote_user}@${master_ip}" "mkdir -p \
  '${remote_work_dir}' \
  '${remote_exp_dir}' \
  '${remote_config_dir}' \
  '${remote_work_dir}/scripts' \
  '${remote_work_dir}/logs' \
  '${remote_work_dir}/tls-data'" >/dev/null 2>&1 || true

echo "Remote directories ensured."
echo

###################################################
# Copia arquivos necessários para o master
###################################################

echo "Copying master commands and configs to master."

# Copia master-commands.cmd (se existir)
if [[ -f "${local_master_cmd}" ]]; then
  bash scripts/stubborn-scp.sh 10 \
    "${local_master_cmd}" \
    "${remote_user}@${master_ip}:${remote_master_cmd}"
else
  echo "AVISO: pulando cópia do master-commands.cmd (arquivo local inexistente: ${local_master_cmd})"
fi

# Copia deployment.csv e deployment.dpl (se existirem)
if [[ -f "${local_deployment_csv}" ]]; then
  bash scripts/stubborn-scp.sh 10 \
    "${local_deployment_csv}" \
    "${remote_user}@${master_ip}:${remote_exp_dir}/deployment.csv"
else
  echo "AVISO: deployment.csv não encontrado em ${exp_data_dir} (a cópia para o master será pulada)."
fi

if [[ -f "${local_deployment_dpl}" ]]; then
  bash scripts/stubborn-scp.sh 10 \
    "${local_deployment_dpl}" \
    "${remote_user}@${master_ip}:${remote_exp_dir}/deployment.dpl"
else
  echo "AVISO: deployment.dpl não encontrado em ${exp_data_dir} (a cópia para o master será pulada)."
fi

# Copia diretório de configs (config-000X.yml) se existir
if [[ -d "${exp_data_dir}/config" ]]; then
  echo "Copying experiment config files to master."
  bash scripts/stubborn-scp.sh 10 \
    "${exp_data_dir}/config/" \
    "${remote_user}@${master_ip}:${remote_config_dir}/"
else
  echo "AVISO: diretório de configs não encontrado em ${exp_data_dir}/config (nenhum YAML será copiado)."
fi

echo "Done."
echo

###################################################
# Gera script remoto que de fato inicia o master
###################################################

cat > "/tmp/user-script-master-body.$$" <<EOF
#!/usr/bin/env bash
set -euo pipefail

export PATH="${remote_bin_dir}:/usr/local/go/bin:\$PATH"

echo "Master remote script started." >> "${remote_main_log}"
echo "Master IP: ${master_ip}" >> "${remote_main_log}"
echo "Experiment directory: ${remote_exp_dir}" >> "${remote_main_log}"

# Marca READY como "iniciando"
echo "STARTING" > "${remote_ready_file}"

# Inicia discoverymaster
echo "Iniciando discoverymaster no master..." >> "${remote_main_log}"
nohup discoverymaster \
  --deployment-file "${remote_exp_dir}/deployment.dpl" \
  --master-address "${master_ip}:${master_port}" \
  >> "${remote_main_log}" 2>&1 &

# Inicia orderingclient de acordo com master-commands.cmd
echo "Iniciando orderingclient (via master-commands.cmd)..." >> "${remote_main_log}"
nohup bash "${remote_master_cmd}" >> "${remote_main_log}" 2>&1 &

# Marca READY
echo "READY" > "${remote_ready_file}"
echo "Master READY em \$(date)" >> "${remote_main_log}"
EOF

# Copia o corpo do script para o master
bash scripts/stubborn-scp.sh 10 \
  "/tmp/user-script-master-body.$$" \
  "${remote_user}@${master_ip}:${remote_user_script_body}"

# Garante permissão de execução e marca upload concluído
ssh ${ssh_options} "${remote_user}@${master_ip}" "chmod +x '${remote_user_script_body}' && echo 1 > '${remote_user_script_uploaded}'"

rm -f "/tmp/user-script-master-body.$$"

###################################################
# Dispara o script remoto no master
###################################################

echo "Starting result processor and master server."
ssh ${ssh_options} "${remote_user}@${master_ip}" "nohup '${remote_user_script_body}' >/dev/null 2>&1 &"

echo "Master discovery + orderingclient disparados."
echo "Continuous analysis (remote) disparada (ou ignorada em caso de erro)."

echo "start-master.sh finished."

