#!/bin/bash
#
# scripts/deploy-remote.sh
#
# Este script é chamado pelo deploy.sh (no diretório deployment/)
# quando o tipo de deployment é "remote".
#
# Ele assume que *já* foram feitos:
#   - source scripts/global-vars.sh
#   - source scripts/initialize-deployment.sh
#
# Ou seja, as variáveis a seguir já existem:
#   - exp_data_dir
#   - deployment_file
#   - instance_info_file
#   - exp_id_offset
#   - ssh_options
#   - remote_work_dir, remote_exp_dir, remote_status_file, remote_ready_file
#   - remote_master_command_file, remote_delete_files, remote_config_dir
#   - local_master_command_file, local_result_fetching_log
#   - master_port
#

set -euo pipefail

echo "Killing everything that is alive and pruning state on the remote machines (including SSH) and removing potential bandwidth limit."

###############################################################################
# 1) Descobrir IPs das máquinas a partir de scripts/instance-info
###############################################################################

# Formato esperado de cada linha em $instance_info_file:
# node-0  172.19.124.1  10.10.1.1  master master
# node-1  172.19.124.2  10.10.1.2  slave  peers
# ...

if [ ! -f "$instance_info_file" ]; then
  echo "ERROR: instance_info_file '$instance_info_file' não existe."
  exit 1
fi

# IP público do master (primeira linha com função 'master')
master_ip=$(
  awk '
    NF >= 4 && $4 == "master" {
      print $2
      exit
    }
  ' "$instance_info_file"
)

if [ -z "$master_ip" ]; then
  echo "ERROR: não foi possível encontrar uma linha com role master em $instance_info_file"
  exit 1
fi

# Lista de TODOS os IPs (públicos) para reset (master + slaves)
all_ips=$(awk 'NF >= 2 { print $2 }' "$instance_info_file")

###############################################################################
# 2) Matar analyze-continuously e afins em todas as máquinas
###############################################################################

for ip in $all_ips; do
  # Mesma linha que aparece nos seus logs antigos:
  # scripts/deploy-remote.sh: line 95:  Killed ssh $ssh_options ...
  ssh $ssh_options "Bruno@$ip" \
    "kill -9 \$(ps -ef | grep 'analyze-continuously' | grep -v \$\$ | awk '{print \$2}') 2>/dev/null || true" \
    || true
done

echo
echo "Killed continuous analysis scripts."
echo

###############################################################################
# 3) Reset do estado de experimento em cada máquina (master + slaves)
###############################################################################

for ip in $all_ips; do
  ssh $ssh_options "Bruno@$ip" "
    # Mata binários antigos e limpa dados do experimento anterior
    killall -9 discoverymaster discoveryslave orderingpeer orderingclient scp rsync 2>/dev/null || true
    rm -rf $remote_delete_files

    # Garante diretórios de trabalho
    mkdir -p '$remote_work_dir' '$remote_exp_dir'
    mkdir -p \"\$(dirname '$remote_status_file')\"

    echo RUNNING > '$remote_status_file'

    # Mata sessões sshd antigas (notty), se existirem
    kill -9 \$(ps -ef | grep 'sshd: notty' | awk '{print \$2}') 2>/dev/null || true
  " || true
done

echo
echo " Reset machine state."
echo

###############################################################################
# 4) Garantir diretórios no MASTER e copiar master-commands + configs
###############################################################################

echo "Ensuring remote directories on master ($master_ip)."

ssh $ssh_options "Bruno@$master_ip" "
  mkdir -p '$remote_work_dir' '$remote_exp_dir' '$remote_config_dir'
" || true

echo "Copying master commands and configs to master."

# Arquivo master-commands.cmd local fica em $exp_data_dir/$local_master_command_file
local_master_cmd_path="$exp_data_dir/$local_master_command_file"

if [ ! -f "$local_master_cmd_path" ]; then
  echo "ERROR: arquivo de comandos do master '$local_master_cmd_path' não encontrado."
  exit 1
fi

# Usa o stubborn-scp.sh que criamos/modificamos
scripts/stubborn-scp.sh 10 \
  "$local_master_cmd_path" \
  "Bruno@$master_ip:$remote_master_command_file"

# Copiar diretório config/ (gerado pela generate-config.sh) para o master
# Usamos tar+ssh para copiar a árvore inteira.
if [ -d "$exp_data_dir/config" ]; then
  tar -C "$exp_data_dir/config" -cf - . \
    | ssh $ssh_options "Bruno@$master_ip" "mkdir -p '$remote_config_dir' && tar -C '$remote_config_dir' -xf -"
else
  echo "WARNING: diretório '$exp_data_dir/config' não existe; seguindo mesmo assim."
fi

###############################################################################
# 5) Descobrir quantos peers e clientes a partir do deployment.dpl
###############################################################################

if [ ! -f "$deployment_file" ]; then
  echo "ERROR: deployment_file '$deployment_file' não encontrado."
  exit 1
fi

# Exemplo de linhas relevantes no deployment.dpl:
#   deploy 1 1client
#   deploy 4 peers
num_clients=$(
  awk '
    $1 == "deploy" && $3 == "1client" {
      print $2
      exit
    }
  ' "$deployment_file"
)

if [ -z "$num_clients" ]; then
  num_clients=1
fi

num_peers=$(
  awk '
    $1 == "deploy" && $3 == "peers" {
      print $2
      exit
    }
  ' "$deployment_file"
)

if [ -z "$num_peers" ]; then
  num_peers=4
fi

peer_tag="peers"
client_tag="1client"

###############################################################################
# 6) Função de result fetching (roda em background)
###############################################################################

# result-fetching.log fica em $exp_data_dir/$local_result_fetching_log
local_result_fetch_log="$exp_data_dir/$local_result_fetching_log"

(
  echo "Waiting for master server." > "$local_result_fetch_log"

  # Espera até que o arquivo remote_ready_file exista no MASTER
  while true; do
    if ssh $ssh_options "Bruno@$master_ip" "[ -f '$remote_ready_file' ]"; then
      break
    fi
    echo "cat: $remote_ready_file: No such file or directory" >> "$local_result_fetch_log"
    echo "Master not ready. Retrying in 5 seconds." >> "$local_result_fetch_log"
    sleep 5
  done

  # Quando master-ready aparecer, copiamos experiment-output-*.tar.gz de volta
  # (se scripts/start-master.sh estiver empacotando resultados como no ISS original).
  # Mesmo que não exista, não queremos quebrar o script.
  scripts/stubborn-scp.sh 3 \
    "Bruno@$master_ip:$remote_work_dir/experiment-output-0000.tar.gz" \
    "$exp_data_dir/experiment-output-0000.tar.gz" \
    || true

  # Tenta descompactar, se o tar existir
  if [ -f "$exp_data_dir/experiment-output-0000.tar.gz" ]; then
    mkdir -p "$exp_data_dir/experiment-output"
    tar -C "$exp_data_dir/experiment-output" -xzf "$exp_data_dir/experiment-output-0000.tar.gz" || true
  fi

) &

###############################################################################
# 7) Disparar slaves (1client e peers)
###############################################################################

# Precisamos passar todos os campos do instance-info para start-remote-slaves.sh:
#   instance_id public_ip private_ip role tag
instance_args=$(awk 'NF >= 5 { print $1, $2, $3, $4, $5 }' "$instance_info_file")

echo "Deploying slaves: $num_clients $client_tag"
scripts/start-remote-slaves.sh \
  "$exp_data_dir" \
  "$client_tag" \
  "$num_clients" \
  "$master_ip" \
  $instance_args

echo "Deploying slaves: $num_peers $peer_tag"
scripts/start-remote-slaves.sh \
  "$exp_data_dir" \
  "$peer_tag" \
  "$num_peers" \
  "$master_ip" \
  $instance_args

echo "Remote slave deployment finished."

