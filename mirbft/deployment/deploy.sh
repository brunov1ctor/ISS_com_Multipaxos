#!/bin/bash
set -euo pipefail

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

deploy_dir="$(cd "$(dirname "$0")" && pwd)"

# shellcheck source=/dev/null
source "$deploy_dir/scripts/global-vars.sh"

: "${deployment_data_root:="$deploy_dir/deployment-data"}"
: "${dpl_filename:=deployment.dpl}"
: "${csv_filename:=deployment.csv}"
: "${result_summary_file:=result-summary.csv}"

# Normaliza deployment_data_root (evita “remote-0000” por cwd confuso)
if command -v realpath >/dev/null 2>&1; then
  deployment_data_root="$(realpath -m "$deployment_data_root")"
else
  deployment_data_root="$(cd "$(dirname "$deployment_data_root")" && pwd)/$(basename "$deployment_data_root")"
fi

ensure_local_binaries() {
  if [ "${DEPLOY_SKIP_BUILD:-0}" = "1" ]; then
    log_warn "DEPLOY_SKIP_BUILD=1 -> pulando build automático."
    return 0
  fi

  if ! command -v go >/dev/null 2>&1; then
    log_err "go não encontrado no PATH."
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
  for b in $req_bins; do
    if [ ! -x "$local_bin_dir/$b" ]; then
      log_err "Binário faltando: $local_bin_dir/$b"
      log_err "Rode: go install ./cmd/$b"
      return 1
    fi
  done

  log_info "Todos os binários necessários já existem em $local_bin_dir."
  return 0
}

usage() {
  cat >&2 <<EOF
Uso:
  $0 remote <instance-info> new [config_generator]
  $0 remote <instance-info> <exp_data_dir>
EOF
  exit 1
}

pick_next_experiment_dir() {
  local root="$1"
  local idx=0
  local candidate=""

  mkdir -p "$root"

  while :; do
    candidate="$(printf "%s/remote-%04d" "$root" "$idx")"

    # Se o diretório do experimento já existe, considera usado e pula.
    # (Não depende de config/, porque o generate-config vai criar do jeito dele.)
    if [ -d "$candidate" ]; then
      idx=$((idx + 1))
      continue
    fi

    mkdir -p "$candidate/logs" "$candidate/_debug"
    echo "$candidate"
    return 0
  done
}

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
  log_err "Este deploy.sh suporta apenas 'remote'."
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

  exp_data_dir="$(pick_next_experiment_dir "$deployment_data_root")"
  config_generator_script="${1:-scripts/experiment-configuration/generate-config.sh}"

  log_info "Novo experimento. Diretório escolhido: $exp_data_dir"
  log_info "Garantidos diretórios locais em $exp_data_dir:"
  log_info "  - logs/"
  log_info "  - _debug/"
  log_info "  - config/ (será criado pelo generate-config.sh)"

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

  mkdir -p "$exp_data_dir/logs" "$exp_data_dir/_debug" "$exp_data_dir/config"

  log_info "Usando experimento existente: $exp_data_dir"
  log_info "Garantidos diretórios locais em $exp_data_dir:"
  log_info "  - logs/"
  log_info "  - _debug/"
  log_info "  - config/"
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

  # IMPORTANTÍSSIMO:
  # Não criar exp_data_dir/config aqui — o generate-config.sh reclama só de existir.
  log_info "Config generator: $config_generator_script"
  log_info "exp_data_dir    : $exp_data_dir"

  "$config_generator_script" "$exp_data_dir" | tee "$exp_data_dir/logs/config-generator.log"

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

bash "$deploy_dir/scripts/deploy-remote.sh"

