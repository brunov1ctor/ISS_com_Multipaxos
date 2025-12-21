#!/usr/bin/env bash
set -euo pipefail

MAX_RETRIES="$1"
SRC="$2"
DST="$3"

attempt=1
status=1

while [[ $attempt -le $MAX_RETRIES ]]; do

  # Log mais limpo e útil
  echo "[scp] Enviando arquivo '${SRC}' -> '${DST}' (tentativa ${attempt}/${MAX_RETRIES})"

  if scp -q "$SRC" "$DST"; then
    echo "[scp] Arquivo '${SRC}' enviado com sucesso."
    exit 0
  else
    status=$?
    echo "[scp] Falha ao enviar '${SRC}' (tentativa ${attempt}/${MAX_RETRIES}, status=${status})."
  fi

  attempt=$((attempt+1))
  sleep 0.3
done

echo "[scp] Erro: falha definitiva após ${MAX_RETRIES} tentativas ao enviar '${SRC}'." >&2
exit $status

