#!/usr/bin/env bash
set -euo pipefail

ts() { date +"%Y-%m-%d %H:%M:%S%z"; }
log(){ echo "[stubborn-scp][$(ts)] $*"; }

# Uso esperado:
#   remoto simples:
#     stubborn-scp.sh <tentativas> <src> <dst>
#   remoto/local com chave:
#     stubborn-scp.sh <tentativas> -i <ssh_key_file> <src> <dst>

if [[ $# -lt 3 ]]; then
  log "Uso: $0 <tentativas> [-i chave] <src> <dst>"
  exit 1
fi

retries="$1"
shift

ssh_extra=()
if [[ "${1:-}" == "-i" ]]; then
  # modo com chave: stubborn-scp.sh <tentativas> -i chave src dst
  if [[ $# -lt 4 ]]; then
    log "Uso: $0 <tentativas> -i <chave> <src> <dst>"
    exit 1
  fi
  ssh_extra=(-i "$2")
  shift 2
fi

src="$1"
dest="$2"

dest_dir="$(dirname "$dest")"
log "Iniciando stubborn-scp"
log "  src      = ${src}"
log "  dest     = ${dest}"
log "  dest_dir = ${dest_dir}"

# Garante diretório de destino no nó local (slave)
if [[ -d "$dest_dir" ]]; then
  log "Dest dir já existe localmente."
else
  log "Dest dir NÃO existe; tentando criar: $dest_dir"
  if mkdir -p "$dest_dir"; then
    log "Dest dir criado com sucesso."
  else
    log "ERRO: mkdir -p $dest_dir falhou."
  fi
fi

i=0
while (( i < retries )); do
  i=$((i+1))
  log "Tentativa $i de $retries: scp ${src} -> ${dest}"
  if scp "${ssh_extra[@]}" "$src" "$dest"; then
    log "SCP OK (tentativa $i)."
    exit 0
  else
    rc=$?
    log "SCP falhou (tentativa $i), código de saída = $rc."
    sleep 1
  fi
done

log "SCP falhou após $retries tentativas."
exit 2

