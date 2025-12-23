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

  # Diretório do repositório (mirbft/)
  local repo_dir
  repo_dir="$(cd "$deploy_dir/.." && pwd)"

  if ! command -v go >/dev/null 2>&1; then
    log_err "go não encontrado no PATH. Não dá para compilar binários automaticamente."
    return 1
  fi

  # Onde o go install joga os binários
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

# 1º argumento após 'remote' = instance-info
instance_info_file="$1"
shift || true

# Resolve instance-info em relação ao diretório de deployment, se precisar
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

# 2º argumento: "new" ou diretório do experimento
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

########################################
# Criação de diretórios locais do experimento
########################################

if $new_experiment; then
  mkdir -p "$exp_data_dir"
else
  mkdir -p "$exp_data_dir/config"
fi

mkdir -p "$exp_data_dir/logs" "$exp_data_dir/_debug"

log_info "Garantidos diretórios locais em $exp_data_dir:"
if $new_experiment; then
  log_info "  - raiz do experimento (config/ ficará a cargo do generate-config.sh)"
else
  log_info "  - config/ (já existente ou criado agora)"
fi
log_info "  - logs/"
log_info "  - _debug/"

########################################
# Preflight de build
########################################

log_sep "[BUILD] Preflight: garantindo binários locais"
if ! ensure_local_binaries; then
  log_err "Preflight de build falhou. Abortando deploy."
  exit 1
fi

########################################
# Geração de configurações (apenas para 'new')
########################################

if $new_experiment; then
  if [ -n "$config_generator_script" ]; then
    if [[ "$config_generator_script" != /* ]]; then
      config_generator_script="$deploy_dir/$config_generator_script"
    fi

    if [ ! -x "$config_generator_script" ]; then
      log_err "Script de geração de config não é executável ou não existe: $config_generator_script"
      exit 1
    fi

    log_sep "[CONFIG] Gerando configurações de experimento"
    log_info "Script: $config_generator_script"
    log_info "Exp dir: $exp_data_dir"
    log_info "exp_id_offset: 0"

    if ! "$config_generator_script" "$exp_data_dir" 0; then
      log_err "Geração de config falhou para $exp_data_dir"
      exit 1
    fi
  else
    log_warn "Novo experimento, mas nenhum script de config informado. Assumindo que configs já existem."
  fi
fi

########################################
# Preparar experiment-config/ para o start-master.sh
########################################

if [ -d "$exp_data_dir/config" ] || ls "$exp_data_dir"/config-*.yml >/dev/null 2>&1; then
  log_sep "[CONFIG] Preparando experiment-config/ para deploy remoto"
  rm -rf "$exp_data_dir/experiment-config"
  mkdir -p "$exp_data_dir/experiment-config"

  cp "$exp_data_dir"/config-*.yml "$exp_data_dir/experiment-config/" 2>/dev/null || true
  cp "$exp_data_dir"/config/config-*.yml "$exp_data_dir/experiment-config/" 2>/dev/null || true

  log_info "Configs copiados para $exp_data_dir/experiment-config:"
  ls "$exp_data_dir/experiment-config" || true
else
  log_warn "Nenhum config-*.yml encontrado em $exp_data_dir ou $exp_data_dir/config; experiment-config/ não foi montado."
fi

if $init_only; then
  log_info "Init only solicitado. Diretório do experimento: $exp_data_dir"
  exit 0
fi

########################################
# Deploy remoto (EXECUTAR, NÃO SOURCEAR)
########################################

log_sep "[DEPLOY] Iniciando deploy remoto"
log_info "Exp dir        : $exp_data_dir"
log_info "Instance-info  : $instance_info_file"
log_info "Data root      : $deployment_data_root"

export exp_data_dir
export instance_info_file
export deployment_data_root
export dpl_filename
export csv_filename

# Executa como subprocesso (permite deploy-remote.sh usar exit sem matar deploy.sh)
if ! bash "$deploy_dir/scripts/deploy-remote.sh"; then
  rc=$?
  log_err "scripts/deploy-remote.sh falhou (rc=$rc)."
  exit "$rc"
fi

########################################
# Verificação de resultados reais
########################################

log_sep "[VERIFY] Checando se existem resultados reais"

if [[ ! -d "$exp_data_dir/experiment-output" ]]; then
  log_err "Sem experiment-output em $exp_data_dir. Deploy não gerou métricas reais."
  exit 9
fi

cnt="$(find "$exp_data_dir/experiment-output" -mindepth 2 -maxdepth 2 -type d 2>/dev/null | wc -l | tr -d ' ')"

if [[ "$cnt" == "0" ]]; then
  log_err "experiment-output existe mas está vazio. Deploy inválido (sem métricas)."
  log_err "Dica: veja $exp_data_dir/result-fetching.log e $exp_data_dir/_debug/master-diag.txt (se existirem)."
  exit 10
fi

log_info "OK: experiment-output contém dados ($cnt dirs)."

########################################
# Geração de resumo dos resultados
########################################

log_sep "[SUMMARY] Gerando result-summary.csv"

result_summary_path="$exp_data_dir/$result_summary_file"
if ! "$deploy_dir/scripts/analyze/summarize.sh" \
  "$exp_data_dir/$csv_filename" \
  "$exp_data_dir/experiment-output" \
  | tee "$result_summary_path"
then
  log_err "Falha ao gerar resumo em $result_summary_path"
  exit 11
fi

########################################
# Publicar resultados em /users/Bruno/iss/experiment-output
########################################

log_sep "[PUBLISH] Exportando métricas para /users/${USER}/iss/experiment-output"

publish_root="/users/${USER}/iss/experiment-output"
publish_name="$(basename "$exp_data_dir")"     # ex: remote-0000
publish_dir="${publish_root}/${publish_name}"

mkdir -p "$publish_dir"

# Copia o summary e os artefatos para um lugar “fixo”
cp -f "$result_summary_path" "$publish_dir/result-summary.csv"
rsync -a --delete \
  "$exp_data_dir/experiment-output/" \
  "$publish_dir/experiment-output/"

# (Opcional) guardar alguns metadados úteis
cp -f "$exp_data_dir/$csv_filename" "$publish_dir/$csv_filename" 2>/dev/null || true
cp -f "$exp_data_dir/$dpl_filename" "$publish_dir/$dpl_filename" 2>/dev/null || true

# latest -> último experimento
ln -sfn "$publish_dir" "${publish_root}/latest"

log_info "Publicado em: $publish_dir"
log_info "Atalho: ${publish_root}/latest"

echo
echo "Done. Experiment data directory: $exp_data_dir"
exit 0

