#!/usr/bin/env bash
set -euo pipefail

SRC="$1"
DST="$2"
MAX_RETRIES="${3:-10}"
SCP_OPTS=(-q -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR)

echo "[scp] Enviando '${SRC}' -> '${DST}' ..."

attempt=1
while true; do
  if scp "${SCP_OPTS[@]}" "$SRC" "$DST"; then
    echo "[scp] OK (tentativa $attempt)."
    exit 0
  fi

  if (( attempt >= MAX_RETRIES )); then
    echo "[scp] Falhou após $attempt tentativas."
    exit 1
  fi

  attempt=$((attempt + 1))
  sleep 1
  echo "[scp] Tentativa $attempt..."
done

