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
# Logging helpers
# --------------------------------------------------------------------
ts() { date +"%Y-%m-%d %H:%M:%S"; }
log_info() { echo "[INFO  ][$(ts)] $*"; }
log_warn() { echo "[WARN  ][$(ts)] $*" >&2; }
log_err()  { echo "[ERROR ][$(ts)] $*" >&2; }

# --------------------------------------------------------------------
# SSH known_hosts preflight (evita "Host key verification failed")
# - Remove entradas antigas do known_hosts para IPs do instance-info
# - Faz ssh-keyscan para pré-popular novas chaves
# --------------------------------------------------------------------
ssh_known_hosts_preflight() {
  local inst_file="$1"
  local known_hosts_file="${HOME}/.ssh/known_hosts"

  if [[ -z "${inst_file:-}" ]]; then
    log_warn "SSH preflight: instance-info vazio/não definido (pulando)"
    return 0
  fi

  if [[ ! -f "$inst_file" ]]; then
    log_warn "SSH preflight: instance-info não encontrado: $inst_file (pulando)"
    return 0
  fi

  mkdir -p "${HOME}/.ssh"
  touch "$known_hosts_file"
  chmod 600 "$known_hosts_file" 2>/dev/null || true

  log_info "SSH preflight: atualizando known_hosts usando: $inst_file"

  # lista IPs (ctrl_ip e data_ip) únicos
  local ips
  ips="$(awk 'NF>=3 {print $2"\n"$3}' "$inst_file" | sort -u | tr '\n' ' ')"
  log_info "SSH preflight: IPs detectados: ${ips}"

  awk 'NF>=3 {print $2"\n"$3}' "$inst_file" | sort -u | while read -r ip; do
    [[ -n "$ip" ]] || continue

    # Remove entradas antigas (se houver)
    ssh-keygen -R "$ip" >/dev/null 2>&1 || true

    # Pré-carrega host key atual (não falha se host ainda não estiver pronto)
    if ssh-keyscan -T 5 -H "$ip" >> "$known_hosts_file" 2>/dev/null; then
      log_info "SSH preflight: hostkey registrada para $ip"
    else
      log_warn "SSH preflight: não consegui ssh-keyscan em $ip (host pode não estar acessível ainda)."
    fi
  done

  log_info "SSH preflight: concluído."
}

# --------------------------------------------------------------------
# Suporte ao modo "new":
#   ./deploy.sh remote scripts/instance-info new scripts/experiment-configuration/generate-config.sh
# Neste modo, cria automaticamente o próximo deployment-data/<type>-XXXX e gera configs.
# --------------------------------------------------------------------
depl_type="$1"
instance_info_file="$2"

if [ "$3" = "new" ]; then
  config_gen_script="$4"
  if [ ! -x "$config_gen_script" ]; then
    echo "ERROR: script de geração de config não encontrado ou não executável: $config_gen_script"
    exit 1
  fi

  # Busca próximo diretório disponível
  exp_index=0
  while true; do
    candidate="$deployment_data_root/${depl_type}-$(printf "%04d" "$exp_index")"
    if [ ! -d "$candidate" ]; then
      exp_data_dir="$candidate"
      break
    fi
    exp_index=$((exp_index + 1))
  done

  mkdir -p "$exp_data_dir"

  log_info "Using experiment data directory: $exp_data_dir"
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

# Preflight SSH/known_hosts para evitar falhas de verificação de hostkey em ambientes efêmeros (Emulab/cloud)
ssh_known_hosts_preflight "$instance_info_file"

# --------------------------------------------------------------------
# Se for só inicialização (-i), sai aqui.
# --------------------------------------------------------------------
if $init_only; then
  exit 0
fi

# --------------------------------------------------------------------
# Reset (remote/local): mata processos antigos e limpa estado.
# --------------------------------------------------------------------
if [ "$depl_type" = "remote" ]; then
  echo
  echo "Limpando processos antigos e removendo possíveis limitações de banda nas máquinas remotas..."
  scripts/reset-proc-cloud.sh "$instance_info_file" || true

  echo
  scripts/reset-state-cloud.sh "$instance_info_file"

  echo
  echo "Estado das máquinas remotas resetado."
elif [ "$depl_type" = "local" ]; then
  scripts/reset-state-local.sh
else
  echo "ERROR: tipo de deployment desconhecido: $depl_type"
  exit 1
fi

# --------------------------------------------------------------------
# Executa deploy remoto/local (start master + start slaves + wait + fetch).
# --------------------------------------------------------------------
if [ "$depl_type" = "remote" ]; then
  scripts/deploy-remote.sh "$instance_info_file" "$exp_data_dir"
else
  scripts/deploy-local.sh "$exp_data_dir"
fi

# --------------------------------------------------------------------
# Gera summary no final (caso exista).
# --------------------------------------------------------------------
scripts/result-summary.sh "$exp_data_dir"

