#!/bin/bash
#
# scripts/deploy-remote.sh
#
# Remote deployment driver for ISS experiments.
#
# Esta versão:
#   - reseta o estado das máquinas remotas (mata processos antigos e limpa dirs);
#   - garante master-commands.cmd e todos os *.yml no master;
#   - copia o diretório scripts/ para o master (para existir iss/scripts/start-master.sh,
#     iss/scripts/analyze/analyze-continuously.sh, etc.);
#   - inicia o master server e o result fetcher;
#   - dispara os slaves conforme o deploy_schedule.
#
# Variáveis esperadas (definidas por initialize-deployment.sh / global-vars.sh):
#   exp_data_dir
#   deployment_file
#   instance_info_file
#   deploy_schedule
#   local_master_command_template_file
#   local_master_command_file
#   instance_info_file_name
#   remote_private_key_file
#   ssh_options
#   remote_work_dir
#   remote_exp_dir        (se não houver, definimos default)
#   remote_status_file
#   remote_delete_files
#   local_result_fetching_log
#   cancel_instances
#

set -euo pipefail

###############################################################################
# Helper: lista todos os IPs públicos a partir do instance-info
# Formato de linha útil:
#   <instance_id> <public_ip> <private_ip> <role> <tag>
###############################################################################
all_hosts() {
  awk 'BEGIN{FS="[ \t]+"} !/^#/ && NF >= 2 {print $2}' "$instance_info_file" | sort -u
}

###############################################################################
# Descobre o IP do master a partir do instance-info
###############################################################################
master_ip=$(
  awk 'BEGIN{FS="[ \t]+"} !/^#/ && $4 == "master" {print $2; exit}' "$instance_info_file"
)

if [ -z "$master_ip" ]; then
  >&2 echo "deploy-remote.sh: não foi possível obter o IP do master em $instance_info_file"
  exit 1
fi

# Guarda uma cópia do instance-info no diretório do experimento
cp "$instance_info_file" "$exp_data_dir/$instance_info_file_name"

echo "Using instance info file: $instance_info_file"
echo "       Master IP address: $master_ip"

###############################################################################
# Caminhos para master-commands (forçamos a ficar dentro do exp_data_dir)
###############################################################################
# Mesmo que initialize-deployment.sh tenha definido algo, garantimos que
# fiquem no diretório do experimento (como o ISS original faz).
local_master_command_template_file="$exp_data_dir/master-commands-template.cmd"
local_master_command_file="$exp_data_dir/master-commands.cmd"

###############################################################################
# Gera / confirma o arquivo de comandos do master
###############################################################################
if [ -f "$local_master_command_file" ]; then
  echo "Using pre-generated master command script at $local_master_command_file."
else
  echo "No pre-generated master command script, copying template."

  if [ ! -f "$local_master_command_template_file" ]; then
    echo "ERROR: template file not found: $local_master_command_template_file"
    echo "       (esperado que generate-config.sh tenha criado esse arquivo)"
    exit 1
  fi

  cp "$local_master_command_template_file" "$local_master_command_file"
fi

echo "Master command script written to $local_master_command_file."

###############################################################################
# Reset das máquinas remotas: mata processos antigos e limpa estado
###############################################################################
echo "Killing everything that is alive and pruning state on the remote machines (including SSH) and removing potential bandwidth limit."

# Mata scripts de análise contínua (best-effort)
for ip in $(all_hosts); do
  ssh $ssh_options "$ip" "kill -9 \$(ps -ef | grep 'analyze-continuously' | grep -v \$\$ | awk '{print \$2}') 2>/dev/null || true" || true
done

echo
echo "Killed continuous analysis scripts."
echo

# Se remote_exp_dir não tiver sido definido em global-vars, define um padrão
if [ -z "${remote_exp_dir:-}" ]; then
  remote_exp_dir="$remote_work_dir/current-deployment-data"
fi

# Limpa e prepara diretórios remotos
for ip in $(all_hosts); do
  ssh $ssh_options "$ip" "
    # Mata binários antigos e limpa dados do experimento anterior
    killall -9 discoverymaster discoveryslave orderingpeer orderingclient scp rsync 2>/dev/null || true
    rm -rf $remote_delete_files

    # Garante diretórios de trabalho
    mkdir -p $remote_work_dir $remote_exp_dir
    mkdir -p \$(dirname $remote_status_file)

    echo RUNNING > $remote_status_file

    # Mata sessões sshd antigas (notty), se existirem
    kill -9 \$(ps -ef | grep 'sshd: notty' | awk '{print \$2}') 2>/dev/null || true
  " &
  sleep 0.1
done
wait

echo
echo " Reset machine state."
echo

###############################################################################
# Garante scripts + configs no master
###############################################################################
echo "Ensuring remote directories on master ($master_ip)."
ssh $ssh_options "$master_ip" "
  mkdir -p $remote_work_dir/scripts
  mkdir -p $remote_work_dir/experiment-config
"

echo "Copying master commands and configs to master."

# master-commands.cmd
scripts/stubborn-scp.sh 10 "$local_master_command_file" "$master_ip:$remote_work_dir/master-commands.cmd"

# Todos os configs (*.yml) para $remote_work_dir/experiment-config
if ls "$exp_data_dir/config"/*.yml >/dev/null 2>&1; then
  for cfg in "$exp_data_dir"/config/*.yml; do
    cfg_base=\$(basename "$cfg")
    echo "Copying $cfg to master as $remote_work_dir/experiment-config/$cfg_base"
    scripts/stubborn-scp.sh 10 "$cfg" "$master_ip:$remote_work_dir/experiment-config/$cfg_base"
  done
else
  echo "WARNING: no *.yml config files found under $exp_data_dir/config; master will have no experiment-configs."
fi

# Copia o diretório scripts/ inteiro para o master
echo "Copying scripts/ directory to master ($master_ip)."
rsync -az -e "ssh $ssh_options" scripts/ "$master_ip:$remote_work_dir/scripts/" >/dev/null

###############################################################################
# Inicia o master server (em background na máquina de deployment)
# Usa scripts/start-master.sh (no deployment local), que por sua vez faz ssh
# para o master e roda iss/scripts/start-master.sh lá.
###############################################################################
echo "Starting result processor and master server."
scripts/start-master.sh "$exp_data_dir" "$master_ip" &

###############################################################################
# Inicia os slaves conforme o schedule
###############################################################################
scripts/deploy-slaves-remote.sh "$exp_data_dir" "$instance_info_file" "$master_ip" $deploy_schedule &

###############################################################################
# Inicia o fetch de resultados e espera tudo terminar
###############################################################################
scripts/fetch-results.sh "$master_ip" "$exp_data_dir" > "$local_result_fetching_log" 2>&1 &

wait

# Opcional: cancela instâncias cloud após o experimento
if [ "$cancel_instances" = true ]; then
  echo "Cancelling cloud instances."
  scripts/cancel-cloud-instances.sh "$deployment_file"
fi

echo "Remote slave deployment finished."

