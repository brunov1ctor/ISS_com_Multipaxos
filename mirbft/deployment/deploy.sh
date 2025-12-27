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

# Carrega variáveis globais compartilhadas
# shellcheck source=/dev/null
source "$deploy_dir/scripts/global-vars.sh"

# Raiz dos dados de deployment (onde ficam remote-0000, remote-0001, ...)
: "${deployment_data_root:="$deploy_dir/deployment-data"}"

# Normaliza para path absoluto (evita escolher remote-0000 por "cwd" diferente)
if command -v realpath >/dev/null 2>&1; then
  deployment_data_root="$(realpath -m "$deployment_data_root")"
else
  # fallback simples
  deployment_data_root="$(cd "$(dirname "$deployment_data_root")" && pwd)/$(basename "$deployment_data_root")"
fi

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
        log_err "Falha ao compilar $b."
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
# Util: escolher próximo remote-XXXX livre (sem risco de overwrite)
########################################

pick_next_experiment_dir() {
  local root="$1"
  local idx=0
  local candidate=""

  mkdir -p "$root"

  while :; do
    candidate="$(printf "%s/remote-%04d" "$root" "$idx")"

    # Critério de "ocupado":
    # - se o diretório existe E
    # - se existe config/ (ou qualquer arquivo esperado) => considera usado e pula
    if [ -d "$candidate/config" ] || [ -f "$candidate/$csv_filename" ] || [ -f "$candidate/$dpl_filename" ]; then
      idx=$((idx + 1))
      continue
    fi

    # Se não existe, ou existe mas sem artefatos => escolhe e cria estrutura
    mkdir -p "$candidate/logs" "$candidate/_debug" "$candidate/config"
    echo "$candidate"
    return 0
  done
}

########################################
# Parse de argumentos
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
  log_err "Este deploy.sh suporta apenas 'remote'."
  exit 2
fi

if [ "$#" -lt 1 ]; then
  usage
fi

instance_info_file="$1"
shift || true

# Resolve instance-info relativo ao deploy_dir
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
else
  exp_data_dir="$1"
  shift || true

  # Se veio relativo, resolve dentro do deployment_data_root
  if [[ "$exp_data_dir" != /* ]]; then
    exp_data_dir="$deployment_data_root/$exp_data_dir"
  fi

  if [ ! -d "$exp_data_dir" ]; then
    log_err "Diretório de experimento não existe: $exp_data_dir"
    exit 1
  fi

  mkdir -p "$exp_data_dir/logs" "$exp_data_dir/_debug" "$exp_data_dir/config"
  log_info "Usando experimento existente: $exp_data_dir"
fi

log_info "Garantidos diretórios locais em $exp_data_dir:"
log_info "  - logs/"
log_info "  - _debug/"
log_info "  - config/ (se aplicável)"

ensure_local_binaries

log_sep "[INIT] Gerando config/deployment para o novo experimento"

if $new_experiment; then
  # Resolve config generator relativo ao deploy_dir
  if [[ ! -x "$config_generator_script" ]]; then
    if [[ -x "$deploy_dir/$config_generator_script" ]]; then
      config_generator_script="$deploy_dir/$config_generator_script"
    fi
  fi

  if [[ ! -x "$config_generator_script" ]]; then
    log_err "Config generator não encontrado ou não executável: $config_generator_script"
    exit 1
  fi

  # Segurança extra: se config/ já tem coisas, aborta (não sobrescreve)
  if [ -n "$(ls -A "$exp_data_dir/config" 2>/dev/null || true)" ]; then
    log_err "Diretório já contém config(s): $exp_data_dir/config"
    log_err "Escolha outro exp dir ou rode 'new' para criar um novo automaticamente."
    exit 1
  fi

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

# Faz reset remoto + start master/slaves + coleta resultados
bash "$deploy_dir/scripts/deploy-remote.sh"

