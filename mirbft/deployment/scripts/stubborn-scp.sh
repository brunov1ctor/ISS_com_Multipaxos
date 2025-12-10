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
  echo "[stubborn-scp] $*" >&2
}

usage() {
  cat >&2 <<EOF
Uso:
  $0 <retries> -i host:/caminho/remoto /caminho/local
  $0 <retries> origem destino

Em que:
  - <retries> é o número de tentativas
  - <origem> e <destino> são caminhos locais ou host:path

Exemplos:
  $0 10 -i user@host:/tmp/foo ./foo
  $0 10 ./foo user@host:/tmp/foo
EOF
  exit 1
}

if [[ $# -lt 3 ]]; then
  usage
fi

retries="$1"
shift

if ! [[ "$retries" =~ ^[0-9]+$ ]]; then
  echo "Primeiro argumento deve ser número de tentativas." >&2
  usage
fi

mode=""
if [[ "$1" == "-i" ]]; then
  mode="pull"
  shift
else
  mode="auto"
fi

src="$1"
dst="$2"

if [[ "$mode" == "pull" ]]; then
  # Modo compatível antigo: remoto->local
  if [[ "$src" != *:* ]]; then
    echo "No modo -i, a origem deve ser host:/caminho/remoto" >&2
    exit 1
  fi
else
  # Modo auto: um lado tem host:
  if [[ "$src" == *:* && "$dst" == *:* ]]; then
    echo "Exatamente um lado deve conter host:path" >&2
    exit 1
  fi
  if [[ "$src" != *:* && "$dst" != *:* ]]; then
    echo "Um dos lados deve conter host:path" >&2
    exit 1
  fi
fi

attempt=1
status=1

while (( attempt <= retries )); do
  log "Tentativa $attempt de $retries..."

  if [[ "$mode" == "pull" ]]; then
    # src: host:/caminho  dst: local
    host="${src%%:*}"
    rpath="${src#*:}"

    if ssh ${ssh_options:-} "$host" "test -f '$rpath'"; then
      ssh ${ssh_options:-} "$host" "cat '$rpath'" > "$dst" && {
        log "Cópia remoto->local concluída com sucesso."
        exit 0
      }
    else
      log "Arquivo remoto '$rpath' não existe em $host."
      status=1
    fi
  else
    # Modo auto (um lado é remoto)
    if [[ "$src" == *:* ]]; then
      # remoto -> local
      host="${src%%:*}"
      rpath="${src#*:}"

      if ssh ${ssh_options:-} "$host" "test -f '$rpath'"; then
        ssh ${ssh_options:-} "$host" "cat '$rpath'" > "$dst" && {
          log "Cópia remoto->local concluída com sucesso."
          exit 0
        }
      else
        log "Arquivo remoto '$rpath' não existe em $host."
        status=1
      fi
    else
      # local -> remoto
      host="${dst%%:*}"
      rpath="${dst#*:}"

      if [[ ! -f "$src" && ! -d "$src" ]]; then
        log "Origem local '$src' não existe."
        status=1
      else
        # Se for diretório, empacota via tar; se for arquivo, envia direto.
        if [[ -d "$src" ]]; then
          tar czf - -C "$(dirname "$src")" "$(basename "$src")" | \
            ssh ${ssh_options:-} "$host" "mkdir -p \"\$(dirname '$rpath')\" && tar xzf - -C \"\$(dirname '$rpath')\""
        else
          ssh ${ssh_options:-} "$host" "mkdir -p \"\$(dirname '$rpath')\" && cat > '$rpath'" < "$src"
        fi
        log "Cópia local->remoto concluída com sucesso."
        exit 0
      fi
    fi
  fi

  status=$?
  log "Falha na cópia (status $status). Tentando novamente..."
  sleep 1
  attempt=$((attempt + 1))
done

log "Desisti após $retries tentativas. Último status: $status"
exit "$status"

