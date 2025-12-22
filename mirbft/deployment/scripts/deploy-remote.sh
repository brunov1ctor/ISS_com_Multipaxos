#!/usr/bin/env bash

# deploy-remote.sh (versão com DEPLOY_DIR robusto + chamada python3)

set -e

ts() { date +"%Y-%m-%d %H:%M:%S%z"; }
log_i() { echo "[INFO  ][$(ts)] $*"; }
log_w() { echo "[WARN  ][$(ts)] $*" >&2; }
log_e() { echo "[ERRO  ][$(ts)] $*" >&2; }

# Flag de cancelamento de instâncias (padrão: false)
: "${cancel_instances:=false}"

# Binário de Python a usar para scripts auxiliares
: "${PYTHON:=python3}"

# =====================================================================
# 0) Descobrir DEPLOY_DIR de forma robusta
# =====================================================================
# Se DEPLOY_DIR vier do ambiente (global-vars.sh / deploy.sh),
# usamos como está. Senão, inferimos a partir da localização deste script.
if [ -z "${DEPLOY_DIR:-}" ]; then
  # Caminho absoluto do script atual
  SCRIPT_PATH="$(readlink -f "$0" 2>/dev/null || realpath "$0" 2>/dev/null || echo "$0")"

  # Diretório do script (…/mirbft/deployment/scripts)
  SCRIPT_DIR="$(cd "$(dirname "$SCRIPT_PATH")" && pwd)"

  # DEPLOY_DIR é o pai de "scripts", ou seja, …/mirbft/deployment
  DEPLOY_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
fi

if [ ! -d "$DEPLOY_DIR" ]; then
  log_e "DEPLOY_DIR inválido: $DEPLOY_DIR"
  exit 1
fi

log_i "Usando DEPLOY_DIR: $DEPLOY_DIR"

# =====================================================================
# 1) Variáveis básicas
# =====================================================================

instance_info_file="$1"
mode="$2"             # "new" ou "reuse"
config_script="$3"    # scripts/experiment-configuration/generate-config.sh

if [ -z "$instance_info_file" ] || [ -z "$mode" ] || [ -z "$config_script" ]; then
  echo "Uso: $0 <instance-info-file> <new|reuse> <config-script>"
  exit 1
fi

if [ ! -f "$instance_info_file" ]; then
  log_e "Arquivo instance-info não encontrado: $instance_info_file"
  exit 1
fi

if [ ! -x "$config_script" ]; then
  log_e "Script de configuração não encontrado ou não executável: $config_script"
  exit 1
fi

# Diretório base para dados de experimento
data_root="$DEPLOY_DIR/deployment-data"
mkdir -p "$data_root"

# Descobre o próximo ID de experimento
if [ "$mode" = "new" ]; then
  # Conta quantos diretórios "remote-XXXX" já existem
  max_id=-1
  for d in "$data_root"/remote-*; do
    [ -d "$d" ] || continue
    base="$(basename "$d")"          # remote-0000
    num="${base#remote-}"            # 0000
    if [[ "$num" =~ ^[0-9]+$ ]]; then
      if [ "$num" -gt "$max_id" ]; then
        max_id="$num"
      fi
    fi
  done
  next_id=$((max_id + 1))
  exp_id="$(printf "%04d" "$next_id")"
  exp_dir="$data_root/remote-$exp_id"
  log_i "Novo experimento. Diretório escolhido: $exp_dir"
  mkdir -p "$exp_dir"
else
  # Modo reuse: espera-se que o diretório já exista
  exp_id="$(printf "%04d" 0)"
  exp_dir="$data_root/remote-$exp_id"
  if [ ! -d "$exp_dir" ]; then
    log_e "Modo reuse mas o diretório $exp_dir não existe."
    exit 1
  fi
  log_i "Reutilizando experimento em $exp_dir"
fi

# Garante subdiretórios locais básicos
mkdir -p "$exp_dir/logs"
mkdir -p "$exp_dir/_debug"
log_i "Garantidos diretórios locais em $exp_dir:"
log_i "  - raiz do experimento (config/ ficará a cargo do generate-config.sh)"
log_i "  - logs/"
log_i "  - _debug/"

# =====================================================================
# 2) Garantir binários locais
# =====================================================================

local_bin_dir="${GOBIN:-$HOME/go/bin}"
if [ ! -d "$local_bin_dir" ]; then
  local_bin_dir="$HOME/go/bin"
fi

# Lista de binários necessários
bins=(discoverymaster discoveryslave orderingpeer orderingclient)

log_i ""
log_i "=================================================="
log_i "[BUILD] Preflight: garantindo binários locais"
log_i "=================================================="

missing_bins=()
for b in "${bins[@]}"; do
  if [ ! -x "$local_bin_dir/$b" ]; then
    missing_bins+=("$b")
  fi
done

if [ "${#missing_bins[@]}" -gt 0 ]; then
  log_i "Binários ausentes: ${missing_bins[*]}"
  log_i "Tentando compilar binários com 'go build'…"

  # Ajustar para seu layout real de projeto
  # Supomos que estamos em DEPLOY_DIR/.. = mirbft
  project_root="$(cd "$DEPLOY_DIR/.." && pwd)"

  for b in "${missing_bins[@]}"; do
    pushd "$project_root" >/dev/null
    log_i "Compilando $b ..."
    # Ajuste o caminho de main se necessário (ex.: ./cmd/orderingpeer)
    go build -o "$local_bin_dir/$b" "./cmd/$b" || {
      log_e "Falha ao compilar $b"
      exit 1
    }
    popd >/dev/null
  done
else
  log_i "Todos os binários necessários já existem em $local_bin_dir."
fi

# =====================================================================
# 3) Gerar configs de experimento
# =====================================================================

log_i ""
log_i "=================================================="
log_i "[CONFIG] Gerando configurações de experimento"
log_i "=================================================="
log_i "Script: $config_script"
log_i "Exp dir: $exp_dir"
log_i "exp_id_offset: 0"

"$config_script" "$exp_dir" 0

# =====================================================================
# 4) Preparar config para deploy remoto
# =====================================================================

log_i ""
log_i "=================================================="
log_i "[CONFIG] Preparando experiment-config/ para deploy remoto"
log_i "=================================================="

config_src="$exp_dir"
config_dest="$exp_dir/experiment-config"

mkdir -p "$config_dest"
cp "$config_src"/config-*.yml "$config_dest/"

log_i "Configs copiados para $config_dest:"
ls "$config_dest"

# =====================================================================
# 5) Reset remoto: matar processos antigos + limpar estado
# =====================================================================

log_i ""
log_i "=================================================="
log_i "[DEPLOY] Iniciando deploy remoto"
log_i "=================================================="
log_i "Exp dir        : $exp_dir"
log_i "Instance-info  : $instance_info_file"
log_i "Data root      : $data_root"

# Lê a primeira linha para descobrir master_ip e remote_user
master_ip="$(awk 'NR==1 {print $2}' "$instance_info_file")"
remote_user="$(awk 'NR==1 {print $1}' "$instance_info_file")"

log_i "[INFO ] Using instance info file: $instance_info_file"
log_i "[INFO ] Master IP address      : $master_ip"

deployment_file="$exp_dir/deployment.dpl"
local_master_command_file="master-commands.cmd"

if [ ! -f "$deployment_file" ]; then
  log_e "Arquivo deployment.dpl não encontrado: $deployment_file"
  exit 1
fi

# Gera master-commands-template se não existir
if [ ! -f "$exp_dir/master-commands-template.cmd" ]; then
  log_i "master-commands-template.cmd não encontrado. Gerando via generate-master-commands.py..."
  log_i "  deployment_file (.dpl) = $deployment_file"
  log_i "  template out           = $exp_dir/master-commands-template.cmd"

  "$PYTHON" "$DEPLOY_DIR/scripts/generate-master-commands.py" \
    "$deployment_file" \
    "$exp_dir/master-commands-template.cmd"

  # Mostra um pedaço do template, opcional
  head -n 30 "$exp_dir/master-commands-template.cmd"
fi

log_i "Generating final master command file a partir do template..."

cp "$exp_dir/master-commands-template.cmd" "$exp_dir/$local_master_command_file"

log_i "Master command file pronto: $exp_dir/$local_master_command_file"

log_i ""
log_i "Killing everything that is alive and pruning state on the remote machines (including SSH) and removing potential bandwidth limit."
log_i ""

ssh_options="-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null"

# Mata analyze-continuously (se estiver rodando)
for ip in $(awk '{print $2}' "$instance_info_file"); do
  ssh $ssh_options "${remote_user}@${ip}" \
    "kill -9 \$(ps -ef | grep 'analyze-continuously' | grep -v \$\$ | awk '{print \$2}')" \
    >/dev/null 2>&1 || log_w "$ip: could not kill analyze-continuously (continuando)."
  sleep 0.1
done

log_i "Killed continuous analysis scripts."
log_i ""

# Limpa estado, mata binários velhos, reseta status
for ip in $(awk '{print $2}' "$instance_info_file"); do
  remote_delete_files="$remote_work_dir"
  remote_status_file="$remote_work_dir/status"

  # Reset simplificado: apenas marca o status como RUNNING
  ssh $ssh_options "${remote_user}@${ip}" "echo RUNNING > $remote_status_file" >/dev/null 2>&1 \
    || log_w "$ip: reset failed (continuando)."
  sleep 0.1
done
wait

echo
log_i "Reset machine state."
echo

# =====================================================================
# 5b) Garantir diretório de resultados no master
# =====================================================================

log_i "Ensuring raw-results directory exists on master at /users/${remote_user}/iss/current-deployment-data/raw-results ..."
ssh $ssh_options "${remote_user}@${master_ip}" "
  mkdir -p /users/${remote_user}/iss/current-deployment-data/raw-results
" >/dev/null 2>&1 || log_w "Could not create raw-results dir on master (continuando)."

echo

# =====================================================================
# 6) Start master (AGORA COM ARGUMENTOS CORRETOS)
# =====================================================================

log_i "Starting master on $master_ip..."
ssh $ssh_options "${remote_user}@${master_ip}" "
  cd /users/${remote_user}/iss
  nohup ./scripts/start-master.sh \
    \"$local_master_command_file\" \
    \"$instance_info_file\" \
    > current-deployment-data/master.log 2>&1 &
" || {
  log_e "Falha ao disparar start-master.sh no master."
  exit 1
}

log_i "start-master.sh disparado em background (remote_user=$remote_user, master_ip=$master_ip)."

# =====================================================================
# 7) start-remote-slaves para peers e 1client
# =====================================================================

start_remote_slaves() {
  local tag="$1"
  log_i "Starting $tag slaves (tag=$tag)..."
  ssh $ssh_options "${remote_user}@${master_ip}" "
    cd /users/${remote_user}/iss
    ./scripts/start-remote-slaves.sh \
      \"$exp_dir\" \
      \"$instance_info_file\" \
      \"$tag\" \
      \"$local_master_command_file\" \
      \"$local_bin_dir\"
  " || log_w "Falha ao disparar start-remote-slaves.sh para tag=$tag (continuando)."
}

start_remote_slaves "peers"
start_remote_slaves "1client"

log_i "All slaves started."

# =====================================================================
# 8) Result fetching (em background)
# =====================================================================

log_i "Starting result fetching in the background..."
ssh $ssh_options "${remote_user}@${master_ip}" "
  cd /users/${remote_user}/iss
  nohup ./scripts/fetch-results-loop.sh \"$exp_dir\" \
    > current-deployment-data/result-fetching.log 2>&1 &
" || log_w "Falha ao iniciar fetch-results-loop.sh (continuando)."

log_i "Waiting for deployment process and result fetching to finish."
log_i "For progress on experiment result fetching, see:"
log_i "  $exp_dir/result-fetching.log"

echo "Do not forget to cancel the used virtual servers using cancel-cloud-instances.sh $exp_dir/instance-info "
echo

# =====================================================================
# 9) Pós-processamento local (sumário)
# =====================================================================

log_i "deploy-remote.sh finished."
echo
log_i "=================================================="
log_i "[VERIFY] Checando se existem resultados reais"
log_i "=================================================="

if [ -d "$exp_dir/experiment-output" ] && [ "$(ls -A "$exp_dir/experiment-output" 2>/dev/null)" ]; then
  log_i "OK: experiment-output contém dados ($(ls "$exp_dir/experiment-output" | wc -l) dirs)."
else
  log_w "Nenhum dado em experiment-output."
fi

echo
log_i "=================================================="
log_i "[SUMMARY] Gerando result-summary.csv"
log_i "=================================================="

if [ -x "$DEPLOY_DIR/scripts/process-results.sh" ]; then
  "$DEPLOY_DIR/scripts/process-results.sh" "$exp_dir"
else
  log_w "Script process-results.sh não encontrado; pulando sumário."
fi

echo "Done. Experiment data directory: $exp_dir"

