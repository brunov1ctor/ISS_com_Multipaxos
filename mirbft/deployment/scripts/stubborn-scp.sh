#!/usr/bin/env bash
#
# stubborn-scp.sh - wrapper "teimoso" para copiar arquivos entre
# máquina local e remota usando ssh + cat, com retries.
#
# Formatos aceitos:
#   1) remoto -> local (compatível com forma antiga do ISS):
#        stubborn-scp.sh <tentativas> -i <origem_remota> <destino_local>
#      onde <origem_remota> é host:/caminho/remoto
#
#   2) genérico (novo):
#        stubborn-scp.sh <tentativas> <origem> <destino>
#      onde exatamente UM dos lados contém "host:path".
#
set -euo pipefail

log() {
  echo "[$(date '+%Y-%m-%d %H:%M:%S')] $*" >&2
}

usage() {
  log "Uso esperado: $0 <tentativas> [-i] <origem> <destino>"
  exit 1
}

if [[ $# -lt 3 ]]; then
  usage
fi

retries="$1"
shift

# Aceita (e ignora) um '-i' legado
if [[ "$1" == "-i" ]]; then
  shift
fi

if [[ $# -lt 2 ]]; then
  usage
fi

src="$1"
dst="$2"

# Detecta quem é remoto (host:path)
is_src_remote=false
is_dst_remote=false
[[ "$src" == *:* ]] && is_src_remote=true
[[ "$dst" == *:* ]] && is_dst_remote=true

if $is_src_remote && $is_dst_remote; then
  log "ERRO: tanto origem quanto destino parecem remotos (contêm ':')."
  exit 1
fi

if ! $is_src_remote && ! $is_dst_remote; then
  log "ERRO: nenhum lado parece remoto (nenhum contém ':')."
  exit 1
fi

# Opções de SSH iguais ao restante do deploy (evita prompt de fingerprint)
ssh_base_opts="-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ServerAliveInterval=60"

copy_remote_to_local() {
  local remote="$1"
  local local_path="$2"

  local host="${remote%%:*}"
  local remote_path="${remote#*:}"

  # Garante diretório local
  mkdir -p "$(dirname "$local_path")"

  if ssh $ssh_base_opts "$host" "cat '$remote_path'" > "$local_path"; then
    return 0
  else
    return $?
  fi
}

copy_local_to_remote() {
  local local_path="$1"
  local remote="$2"

  local host="${remote%%:*}"
  local remote_path="${remote#*:}"

  # Garante diretório remoto de destino
  if ! ssh $ssh_base_opts "$host" "mkdir -p \"\$(dirname '$remote_path')\""; then
    return $?
  fi

  if cat "$local_path" | ssh $ssh_base_opts "$host" "cat > '$remote_path'"; then
    return 0
  else
    return $?
  fi
}

attempt=1
status=1

while (( attempt <= retries )); do
  log "Tentativa $attempt/$retries: copiando '$src' -> '$dst'"

  if $is_src_remote && ! $is_dst_remote; then
    if copy_remote_to_local "$src" "$dst"; then
      log "Cópia remoto->local concluída com sucesso."
      exit 0
    fi
  elif ! $is_src_remote && $is_dst_remote; then
    if copy_local_to_remote "$src" "$dst"; then
      log "Cópia local->remoto concluída com sucesso."
      exit 0
    fi
  fi

  status=$?
  log "Falha na cópia (status $status). Tentando novamente..."
  sleep 1
  attempt=$((attempt + 1))
done

log "Desisti após $retries tentativas. Último status: $status"
exit "$status"

