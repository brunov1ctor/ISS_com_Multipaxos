#!/usr/bin/env bash
set -euo pipefail

MAX_RETRIES="$1"
SRC="$2"
DST="$3"

attempt=1
status=1

# Se o destino tiver "user@host:...", tratamos como remoto.
is_remote_dest=0
if [[ "$DST" == *:* ]]; then
  is_remote_dest=1
fi

while [[ $attempt -le $MAX_RETRIES ]]; do
  if [[ $is_remote_dest -eq 0 ]]; then
    # Destino local: garante diretório pai
    dest_dir="$(dirname "$DST")"
    mkdir -p "$dest_dir"
    if scp -q "$SRC" "$DST"; then
      exit 0
    fi
  else
    # Destino remoto (ex: Bruno@172.20.3.2:/users/Bruno/iss/scripts/start-slave.sh)
    if scp -q "$SRC" "$DST"; then
      exit 0
    fi
  fi

  status=$?

  # Só loga retries além da primeira tentativa
  if [[ $attempt -gt 1 ]]; then
    echo "[scp] Retry ${attempt}/${MAX_RETRIES} (status ${status})"
  fi

  attempt=$((attempt+1))
  sleep 0.3
done

echo "[scp] FALHA: não foi possível enviar '${SRC}' -> '${DST}' após ${MAX_RETRIES} tentativas." >&2
exit $status

