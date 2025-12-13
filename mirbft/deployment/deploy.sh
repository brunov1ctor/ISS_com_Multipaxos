#!/bin/bash

# --------------------------------------------------------------------
# Carrega variáveis globais (deployment_data_root, csv_filename, etc.)
# --------------------------------------------------------------------
# shellcheck source=/dev/null
source scripts/global-vars.sh

# Safety: se por algum motivo deployment_data_root veio vazio (env/export),
# evita gerar exp_data_dir como "/remote-0000".
if [[ -z "${deployment_data_root:-}" ]]; then
  deploy_dir="$(cd "$(dirname "$0")" && pwd)"
  deployment_data_root="${deploy_dir}/deployment-data"
fi

# Kill all children of this script when exiting
trap "$trap_exit_command" EXIT

# --------------------------------------------------------------------
# Helpers: logs
# --------------------------------------------------------------------
log_sep() {
  echo
  echo "=================================================="
  echo "$1"
  echo "=================================================="
}
log_info() { echo "[INFO] $*"; }
log_warn() { echo "[WARN] $*"; }
log_err()  { echo "[ERRO] $*" >&2; }

# --------------------------------------------------------------------
# Preflight: garante que os binários existem localmente
# --------------------------------------------------------------------
ensure_local_binaries() {
  if [ "${DEPLOY_SKIP_BUILD:-0}" = "1" ]; then
    log_warn "DEPLOY_SKIP_BUILD=1 -> pulando build automático."
    return 0
  fi

  deploy_dir="$(cd "$(dirname "$0")" && pwd)"
  repo_dir="$(cd "$deploy_dir/.." && pwd)"

  if ! command -v go >/dev/null 2>&1; then
    log_err "go não encontrado no PATH. Não dá para compilar binários automaticamente."
    return 1
  fi

  local_bin_dir="${GOBIN:-}"
  if [ -z "$local_bin_dir" ]; then
    local_bin_dir="$(go env GOBIN 2>/dev/null)"
  fi
  if [ -z "$local_bin_dir" ]; then
    gp="$(go env GOPATH 2>/dev/null)"
    if [ -n "$gp" ]; then
      local_bin_dir="${gp%/}/bin"
    fi
  fi
  if [ -z "$local_bin_dir" ]; then
    local_bin_dir="$HOME/go/bin"
  fi

  req_bins="discoverymaster discoveryslave orderingpeer orderingclient"

  log_sep "[BUILD] Preflight: garantindo binários locais"
  log_info "repo_dir      = $repo_dir"
  log_info "local_bin_dir = $local_bin_dir"
  log_info "go version    = $(go version 2>/dev/null || true)"
  echo

  missing=""
  for b in $req_bins; do
    if [ ! -x "$local_bin_dir/$b" ]; then
      missing="$missing $b"
    fi
  done

  if [ -z "$missing" ]; then
    log_info "OK: todos os binários necessários já existem."
    return 0
  fi

  log_warn "Binários faltando:${missing}"
  log_info "Compilando apenas os que faltam via 'go install'..."

  cd "$repo_dir" || { log_err "não consegui entrar em $repo_dir"; return 1; }

  for b in $missing; do
    if [ ! -d "./cmd/$b" ]; then
      log_err "diretório ./cmd/$b não existe. Não sei compilar '$b'."
      return 1
    fi

    log_info "go install ./cmd/$b"
    if ! go install "./cmd/$b"; then
      log_err "Falha ao compilar $b. Rode 'go install ./cmd/$b' manualmente para ver o erro completo."
      return 1
    fi
  done

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

# --------------------------------------------------------------------
# Trata flag de inicialização apenas (-i / --init-only)
# --------------------------------------------------------------------
if [ "${1:-}" = "-i" ] || [ "${1:-}" = "--init-only" ]; then
  init_only=true
  shift
else
  init_only=false
fi

# --------------------------------------------------------------------
# Suporte ao modo "new":
#   ./deploy.sh remote scripts/instance-info new scripts/experiment-configuration/generate-config.sh
# --------------------------------------------------------------------
if [ "${1:-}" = "remote" ] && [ "${3:-}" = "new" ]; then
  depl_type="$1"
  instance_info_file="$2"
  config_gen_script="${4:-scripts/experiment-configuration/generate-config.sh}"

  exp_index=0
  while :; do
    candidate=$(printf "%s/remote-%04d" "$deployment_data_root" "$exp_index")
    if [ ! -d "$candidate" ]; then
      exp_data_dir="$candidate"
      break
    fi
    exp_index=$((exp_index + 1))
  done

  mkdir -p "$exp_data_dir"

  echo "Using experiment data directory: $exp_data_dir"
  "$config_gen_script" "$exp_data_dir" 0
  if [ $? -ne 0 ]; then
    echo "ERROR: $config_gen_script falhou ao gerar configurações em $exp_data_dir"
    exit 1
  fi

  set -- "$depl_type" "$instance_info_file" "$exp_data_dir"
fi

# --------------------------------------------------------------------
# Inicializa o deployment
# --------------------------------------------------------------------
# shellcheck source=/dev/null
source scripts/initialize-deployment.sh

if $init_only; then
  echo "Init only. Experiment directory: $exp_data_dir"
  exit 0
fi

# --------------------------------------------------------------------
# PRE-FLIGHT BUILD (ANTES de qualquer deploy)
# --------------------------------------------------------------------
if ! ensure_local_binaries; then
  log_err "Preflight de build falhou. Abortando deploy."
  exit 1
fi

# --------------------------------------------------------------------
# Deploy (local / cloud / remote)
# --------------------------------------------------------------------
if [ "$depl_type" = "local" ]; then
  # shellcheck source=/dev/null
  source scripts/deploy-local.sh
elif [ "$depl_type" = "cloud" ]; then
  # shellcheck source=/dev/null
  source scripts/deploy-cloud.sh
elif [ "$depl_type" = "remote" ]; then
  # shellcheck source=/dev/null
  source scripts/deploy-remote.sh
else
  >&2 echo "$0: unknown deployment type: $depl_type (allowed values: local, cloud, remote)"
  exit 2
fi

# --------------------------------------------------------------------
# Validação estrutural ANTES do summary (evita 'summary enganoso')
# --------------------------------------------------------------------
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

# --------------------------------------------------------------------
# Geração do resumo dos resultados
# --------------------------------------------------------------------
echo "Generating result summary."
scripts/analyze/summarize.sh \
  "$exp_data_dir/$csv_filename" \
  "$exp_data_dir/experiment-output" \
  | tee "$exp_data_dir/$result_summary_file"

echo "Done. Experiment data directory: $exp_data_dir"

