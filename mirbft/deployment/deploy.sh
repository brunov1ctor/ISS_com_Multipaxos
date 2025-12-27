#!/bin/bash
set -euo pipefail

########################################
# Helpers de log
########################################

ts() { date +"%Y-%m-%d %H:%M:%S%z"; }

log_sep() {
  echo
  echo "=================================================="
  echo "$1"
  echo "=================================================="
}

log_info() { echo "[INFO ][$(ts)] $*"; }
log_warn() { echo "[WARN ][$(ts)] $*"; }
log_err()  { echo "[ERRO ][$(ts)] $*" >&2; }

########################################
# Diretórios base / variáveis globais
########################################

deploy_dir="$(cd "$(dirname "$0")" && pwd)"

# Carrega variáveis globais compartilhadas (remote_user, remote_work_dir, etc.)
# shellcheck source=/dev/null
source "$deploy_dir/scripts/global-vars.sh"

# Raiz dos dados de deployment (onde ficam remote-0000, local-0000, etc.)
: "${deployment_data_root:="$deploy_dir/deployment-data"}"

# Arquivos padrão dentro de cada experimento
: "${dpl_filename:=deployment.dpl}"
: "${csv_filename:=deployment.csv}"
: "${result_summary_file:=result-summary.csv}"

########################################
# Preflight: garante que os binários existem localmente
########################################

ensure_local_binaries() {
  if [ "${DEPLOY_SKIP_BUILD:-0}" = "1" ]; then
    log_warn "DEPLOY_SKIP_BUILD=1 -> pulando build automático."
    return 0
  fi

  local repo_dir
  repo_dir="$(cd "$deploy_dir/.." && pwd)"

  if ! command -v go >/dev/null 2>&1; then
    log_err "go não encontrado no PATH. Não dá para compilar binários automaticamente."
    return 1
  fi

  local local_bin_dir
  local_bin_dir="${GOBIN:-}"
  if [ -z "$local_bin_dir" ]; then
    local_bin_dir="$(go env GOBIN 2>/dev/null || true)"
  fi
  if [ -z "$local_bin_dir" ]; then
    local gp
    gp="$(go env GOPATH 2>/dev/null || true)"
    if [ -n "$gp" ]; then
      local_bin_dir="${gp%/}/bin"
    fi
  fi
  if [ -z "$local_bin_dir" ]; then
    local_bin_dir="$HOME/go/bin"
  fi

  local req_bins="discoverymaster discoveryslave orderingpeer orderingclient"
  local missing=()

  for b in $req_bins; do
    if [ ! -x "$local_bin_dir/$b" ]; then
      missing+=("$b")
    fi
  done

  if [ "${#missing[@]}" -eq 0 ]; then
    log_info "Todos os binários necessários já existem em $local_bin_dir."
    return 0
  fi

  log_sep "[BUILD] Preflight: garantindo binários locais"
  log_info "Repo dir       : $repo_dir"
  log_info "Bin dir (GOBIN): $local_bin_dir"
  log_info "go version     : $(go version 2>/dev/null || true)"
  log_warn "Binários faltando: ${missing[*]}"
  log_info "Compilando apenas os binários faltantes via 'go install ./cmd/<bin>'..."

  (
    cd "$repo_dir" || {
      log_err "Não consegui entrar em $repo_dir"
      exit 1
    }

    for b in "${missing[@]}"; do
      log_info "go install ./cmd/$b"
      if ! go install "./cmd/$b"; then
        log_err "Falha ao compilar $b. Rode 'go install ./cmd/$b' manualmente para ver o erro completo."
        exit 1
      fi
    done
  ) || return 1

  echo
  log_info "Verificando binários após build..."
  for b in $req_bins; do
    if [ -x "$local_bin_dir/$b" ]; then
      log_info "OK: $local_bin_dir/$b"
    else
      log_err "Ainda faltando: $local_bin_dir/$b"
      return 1
    fi
  done

  log_info "Preflight de build concluído."
  return 0
}

########################################
# Uso
########################################

usage() {
  cat >&2 <<EOF
Uso:
  $0 remote <instance-info> new [config_generator]
  $0 remote <instance-info> <exp_data_dir>

Exemplos:
  $0 remote scripts/instance-info new scripts/experiment-configuration/generate-config.sh
EOF
  exit 1
}

########################################
# Parse de argumentos (apenas modo remote customizado)
########################################

init_only=false
if [ "${1:-}" = "-i" ] || [ "${1:-}" = "--init-only" ]; then
  init_only=true
  shift
fi

if [ "$#" -lt 1 ]; then
  usage
fi

depl_type="$1"
shift

if [ "$depl_type" != "remote" ]; then
  log_err "Este deploy.sh customizado suporta apenas 'remote' (por enquanto)."
  log_err "Chamada recebida: depl_type='$depl_type'"
  exit 2
fi

if [ "$#" -lt 1 ]; then
  usage
fi

instance_info_file="$1"
shift || true

if [[ ! -f "$instance_info_file" ]]; then
  if [[ -f "$deploy_dir/$instance_info_file" ]]; then
    instance_info_file="$deploy_dir/$instance_info_file"
  elif [[ -f "$deploy_dir/scripts/$instance_info_file" ]]; then
    instance_info_file="$deploy_dir/scripts/$instance_info_file"
  else
    log_err "Arquivo de instance-info não encontrado: $instance_info_file"
    exit 1
  fi
fi

log_info "Using instance-info file: $instance_info_file"

if [ "$#" -lt 1 ]; then
  usage
fi

new_experiment=false
config_generator_script=""
exp_data_dir=""

if [ "${1:-}" = "new" ]; then
  new_experiment=true
  shift || true

  idx=0
  while :; do
    candidate=$(printf "%s/remote-%04d" "$deployment_data_root" "$idx")
    if [ ! -d "$candidate" ]; then
      exp_data_dir="$candidate"
      break
    fi
    idx=$((idx + 1))
  done

  config_generator_script="${1:-scripts/experiment-configuration/generate-config.sh}"
  log_info "Novo experimento. Diretório escolhido: $exp_data_dir"
else
  exp_data_dir="$1"
  shift || true

  if [[ "$exp_data_dir" != /* ]]; then
    exp_data_dir="$deployment_data_root/$exp_data_dir"
  fi

  if [ ! -d "$exp_data_dir" ]; then
    log_err "Diretório de experimento não existe: $exp_data_dir"
    exit 1
  fi

  log_info "Usando experimento existente: $exp_data_dir"
fi

log_info "Garantidos diretórios locais em $exp_data_dir:"
mkdir -p "$exp_data_dir/logs" "$exp_data_dir/_debug"
log_info "  - logs/"
log_info "  - _debug/"
if [ -d "$exp_data_dir/config" ] || $new_experiment; then
  mkdir -p "$exp_data_dir/config"
  log_info "  - config/ (se aplicável)"
fi

ensure_local_binaries

log_sep "[INIT] Gerando config/deployment para o novo experimento"

if $new_experiment; then
  if [[ ! -x "$config_generator_script" ]]; then
    if [[ -x "$deploy_dir/$config_generator_script" ]]; then
      config_generator_script="$deploy_dir/$config_generator_script"
    fi
  fi

  if [[ ! -x "$config_generator_script" ]]; then
    log_err "Config generator não encontrado ou não executável: $config_generator_script"
    exit 1
  fi

  log_info "Config generator: $config_generator_script"
  log_info "exp_data_dir    : $exp_data_dir"

  "$config_generator_script" "$exp_data_dir" \
    | tee "$exp_data_dir/logs/config-generator.log"

  if [ ! -f "$exp_data_dir/$csv_filename" ] || [ ! -f "$exp_data_dir/$dpl_filename" ]; then
    log_err "Config generator não gerou $csv_filename e/ou $dpl_filename em $exp_data_dir"
    exit 1
  fi

  log_info "OK: gerados $csv_filename e $dpl_filename"
else
  if [ ! -f "$exp_data_dir/$csv_filename" ] || [ ! -f "$exp_data_dir/$dpl_filename" ]; then
    log_err "Experimento existente sem $csv_filename/$dpl_filename: $exp_data_dir"
    exit 1
  fi
fi

if $init_only; then
  log_warn "init-only -> parando após gerar config."
  exit 0
fi

log_sep "[REMOTE] Deploy remoto + start"
export exp_data_dir
export instance_info_file

# Este script faz reset remoto + start master/slaves
bash "$deploy_dir/scripts/deploy-remote.sh"

