#!/bin/bash

# ============================================================================
# Deploy script for ISS/MultiPaxos experiments (local + remote)
# Adaptado para ambiente de laboratório (Bruno / Emulab-like)
# ============================================================================

set -euo pipefail

# ----------------------------------------------------------------------------
# Funções auxiliares de log
# ----------------------------------------------------------------------------
timestamp() {
  date +"%Y-%m-%d %H:%M:%S-%z"
}

log_info() {
  echo "[INFO ][$(timestamp)] $*"
}

log_warn() {
  echo "[WARN  ][$(timestamp)] $*" >&2
}

log_err() {
  echo "[ERROR ][$(timestamp)] $*" >&2
}

log_sep() {
  echo
  echo "=================================================="
  echo "$1"
  echo "=================================================="
}

# ----------------------------------------------------------------------------
# Uso
# ----------------------------------------------------------------------------
usage() {
  cat <<EOF
Uso: $0 PATH INSTANCE_INFO [new|reuse] [config_generator_script]

  PATH                : "local" ou "remote"
  INSTANCE_INFO       : arquivo com descrição das instâncias (scripts/instance-info)
  new|reuse           : "new" cria novo experimento; "reuse" reaproveita diretório
  config_generator_script (opcional):
                       - script que gera configs de experimento
                       - default: scripts/experiment-configuration/generate-config.sh

Exemplos:
  $0 local  scripts/instance-info new
  $0 remote scripts/instance-info new scripts/experiment-configuration/generate-config.sh
  $0 remote scripts/instance-info reuse
EOF
}

# ----------------------------------------------------------------------------
# Funções de utilidade
# ----------------------------------------------------------------------------

ensure_dir() {
  local d="$1"
  if [ ! -d "$d" ]; then
    mkdir -p "$d"
  fi
}

# Retorna próximo id de experimento (0000, 0001, ...)
next_experiment_id() {
  local root="$1"
  if [ ! -d "$root" ]; then
    echo "0000"
    return
  fi

  local last
  last=$(ls -1 "$root" 2>/dev/null | grep -E '^[0-9]{4}$' | sort | tail -n 1 || true)
  if [ -z "$last" ]; then
    echo "0000"
    return
  fi

  local n=$((10#$last + 1))
  printf "%04d" "$n"
}

# ----------------------------------------------------------------------------
# Início do script
# ----------------------------------------------------------------------------

if [ "$#" -lt 2 ]; then
  usage
  exit 1
fi

path="$1"               # local ou remote
instance_info_file="$2" # scripts/instance-info

if [ ! -f "$instance_info_file" ]; then
  log_err "Arquivo de instance-info não encontrado: $instance_info_file"
  exit 1
fi

mode="${3:-new}"   # new ou reuse
shift 3 || true     # avança os 3 primeiros argumentos, se existirem

# ----------------------------------------------------------------------------
# Diretórios base
# ----------------------------------------------------------------------------
script_dir="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
root_dir="$( cd "$script_dir/.." && pwd )"

deployment_dir="$script_dir"
data_root="$deployment_dir/deployment-data"

ensure_dir "$data_root"

log_info "Using instance-info file: $instance_info_file"

# ----------------------------------------------------------------------------
# Decide se é experimento novo ou reutilizado
# ----------------------------------------------------------------------------
case "$mode" in
  new)
    exp_id="$(next_experiment_id "$data_root")"
    exp_dir="$data_root/remote-$exp_id"
    log_info "Novo experimento. Diretório escolhido: $exp_dir"
    ;;
  reuse)
    # Reaproveita último experimento existente
    last_dir=$(ls -1 "$data_root" 2>/dev/null | sort | tail -n 1 || true)
    if [ -z "$last_dir" ]; then
      log_err "Nenhum experimento existente em $data_root para reaproveitar."
      exit 1
    fi
    exp_dir="$data_root/$last_dir"
    log_info "Reutilizando experimento existente: $exp_dir"
    ;;
  *)
    log_err "Modo inválido: $mode (use new ou reuse)"
    usage
    exit 1
    ;;
esac

# ----------------------------------------------------------------------------
# Preparar estrutura básica do experimento
# ----------------------------------------------------------------------------
ensure_dir "$exp_dir"
ensure_dir "$exp_dir/logs"
ensure_dir "$exp_dir/_debug"

log_info "Garantidos diretórios locais em $exp_dir:"
log_info "  - raiz do experimento (config/ ficará a cargo do generate-config.sh)"
log_info "  - logs/"
log_info "  - _debug/"

# ----------------------------------------------------------------------------
# Preflight de binários (local)
# ----------------------------------------------------------------------------
log_sep "[BUILD] Preflight: garantindo binários locais"

# Diretório de binários do Go do usuário
local_bin_dir="${GOBIN:-$HOME/go/bin}"

# Binários necessários
bins=(
  "discoverymaster"
  "discoveryslave"
  "orderingpeer"
  "orderingclient"
)

missing=()
for b in "${bins[@]}"; do
  if [ ! -x "$local_bin_dir/$b" ]; then
    missing+=("$b")
  fi
done

if [ "${#missing[@]}" -gt 0 ]; then
  log_warn "Alguns binários não foram encontrados em $local_bin_dir:"
  for m in "${missing[@]}"; do
    echo "  - $m"
  done
  echo
  log_info "Tentando compilar os binários faltantes (go build ./cmd/...)"
  (
    cd "$root_dir"
    if ! go build ./cmd/...; then
      log_err "Falha ao compilar os binários. Verifique o ambiente Go."
      exit 1
    fi
  )
  echo

  # Revalida
  missing=()
  for b in "${bins[@]}"; do
    if [ ! -x "$local_bin_dir/$b" ]; then
      missing+=("$b")
    fi
  done

  if [ "${#missing[@]}" -gt 0 ]; then
    log_err "Ainda faltando binários após tentativa de build:"
    for m in "${missing[@]}"; do
      echo "  - $m"
    done
    exit 1
  fi
fi

log_info "Todos os binários necessários já existem em $local_bin_dir."

# ----------------------------------------------------------------------------
# Geração de configuração de experimento (quando modo=new)
# ----------------------------------------------------------------------------
config_generator_script="${1:-scripts/experiment-configuration/generate-config.sh}"
exp_id_offset="${2:-0}"

if [ "$mode" = "new" ]; then
  log_sep "[CONFIG] Gerando configurações de experimento"
  log_info "Script de configuração : $config_generator_script"
  log_info "Diretório do experimento: $exp_dir"
  log_info "exp_id_offset           : ${exp_id_offset}"

  if [ ! -x "$config_generator_script" ]; then
    log_err "Script gerador de config não é executável ou não existe: $config_generator_script"
    exit 1
  fi

  if ! "$config_generator_script" "$exp_dir" "$exp_id_offset"; then
    log_err "Falha ao gerar configs com $config_generator_script"
    exit 1
  fi

  echo
else
  log_info "Modo reuse: mantendo configurações já existentes em $exp_dir"
fi

# ----------------------------------------------------------------------------
# Preparar experiment-config/ (onde o master espera encontrar os .yml)
# ----------------------------------------------------------------------------
# Se existirem config-*.yml na raiz de exp_dir ou em exp_dir/config, copiamos
if compgen -G "$exp_dir/config-*.yml" > /dev/null || compgen -G "$exp_dir/config/config-*.yml" > /dev/null; then
  log_sep "[CONFIG] Preparando experiment-config/ para deploy remoto"
  rm -rf "$exp_dir/experiment-config"
  mkdir -p "$exp_dir/experiment-config"

  # Copia configs tanto da raiz quanto de config/ (sem despejar tudo no log)
  cp "$exp_dir"/config-*.yml "$exp_dir/experiment-config/" 2>/dev/null || true
  cp "$exp_dir"/config/config-*.yml "$exp_dir/experiment-config/" 2>/dev/null || true

  cfg_count=$(ls -1 "$exp_dir/experiment-config"/*.yml 2>/dev/null | wc -l | tr -d ' ')
  log_info "Configs prontos em $exp_dir/experiment-config (${cfg_count:-0} arquivos .yml)."
  echo
else
  log_warn "Nenhum config-*.yml encontrado em $exp_dir nem em $exp_dir/config."
fi

# ----------------------------------------------------------------------------
# A partir daqui: branch local x remoto
# ----------------------------------------------------------------------------

case "$path" in
  local)
    # ========================================================================
    # DEPLOY LOCAL (apenas uma máquina)
    # ========================================================================
    log_sep "[DEPLOY] Iniciando deploy LOCAL"
    log_info "Exp dir        : $exp_dir"
    log_info "Instance-info  : $instance_info_file"
    log_info "Data root      : $data_root"

    # Aqui você pode plugar a lógica específica de deploy local,
    # por exemplo, scripts/local-deploy.sh etc.
    log_warn "(Deploy local ainda não implementado neste script; use remote.)"
    ;;

  remote)
    # ========================================================================
    # DEPLOY REMOTO (Emulab / cluster)
    # ========================================================================
    log_sep "[DEPLOY] Iniciando deploy remoto"
    log_info "Exp dir        : $exp_dir"
    log_info "Instance-info  : $instance_info_file"
    log_info "Data root      : $data_root"

    # Master é o primeiro com tag "master" ou, na ausência, o primeiro da lista
    master_ip=$(awk '!/^#/ && NF>=2 && ($3=="master" || NR==1) {print $2; exit}' "$instance_info_file")
    if [ -z "$master_ip" ]; then
      log_err "Não foi possível detectar o master_ip a partir de $instance_info_file"
      exit 1
    fi
    log_info "Master: $master_ip (instance-info: $instance_info_file)"

    # Gera template de comandos para o master + peers/clients
    log_info "Gerando master-commands-template.cmd..."
    master_cmd_template="$exp_dir/master-commands-template.cmd"

    cat > "$master_cmd_template" <<EOF
-1 1 1client cloud-machine-templates/small-machine-fra05.cmt
-1 4 peers cloud-machine-templates/small-machine-fra05.cmt
EOF

    log_info "Gerando master-commands.cmd..."
    master_cmd="$exp_dir/master-commands.cmd"

    cat > "$master_cmd" <<EOF
# Este arquivo será patchado por start-master.sh para usar caminhos corretos
# e garantir que config/config.yml exista antes de subir os processos.
$(cat "$master_cmd_template")
EOF

    log_info "master-commands.cmd: $master_cmd"

    # Reset remoto + start master + start slaves
    log_info "Resetando estado nas máquinas remotas..."
    "$deployment_dir/scripts/deploy-remote.sh" \
      "$exp_dir" \
      "$instance_info_file" \
      "$data_root" \
      "$master_ip" || {
        log_err "deploy-remote.sh falhou."
        exit 1
      }

    log_sep "[VERIFY] Checando se existem resultados reais"
    if [ -d "$exp_dir/experiment-output" ] && [ "$(find "$exp_dir/experiment-output" -mindepth 1 -maxdepth 1 -type d | wc -l)" -gt 0 ]; then
      log_info "OK: experiment-output contém dados ($(ls -1 "$exp_dir/experiment-output" | wc -l) dirs)."
    else
      log_warn "Nenhum dado encontrado em $exp_dir/experiment-output."
    fi

    log_sep "[SUMMARY] Gerando result-summary.csv"
    "$deployment_dir/scripts/generate-summary.sh" "$exp_dir" || {
      log_err "Falha ao gerar resumo de resultados."
      exit 1
    }

    echo
    echo "Done. Experiment data directory: $exp_dir"
    ;;

  *)
    log_err "PATH inválido: $path (use local ou remote)"
    usage
    exit 1
    ;;
esac

