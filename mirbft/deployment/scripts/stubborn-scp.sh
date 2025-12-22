#!/usr/bin/env bash
set -euo pipefail

MAX_RETRIES="${1:?MAX_RETRIES ausente}"
SRC="${2:?SRC ausente}"
DST="${3:?DST ausente}"

attempt=1

# Se o destino tiver "user@host:...", tratamos como remoto.
is_remote_dest=0
if [[ "$DST" == *:* ]]; then
  is_remote_dest=1
fi

while [[ $attempt -le $MAX_RETRIES ]]; do
  # Destino local: garante diretório pai
  if [[ $is_remote_dest -eq 0 ]]; then
    mkdir -p "$(dirname "$DST")"
  fi

  # Rodar scp e capturar status REAL
  set +e
  scp -q -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null "$SRC" "$DST"
  status=$?
  set -e

  if [[ $status -eq 0 ]]; then
    exit 0
  fi

  echo "[scp] Retry ${attempt}/${MAX_RETRIES} (status ${status})" >&2
  attempt=$((attempt+1))
  sleep 0.3
done

echo "[scp] FALHA: não foi possível copiar '${SRC}' -> '${DST}' após ${MAX_RETRIES} tentativas." >&2
exit 1

