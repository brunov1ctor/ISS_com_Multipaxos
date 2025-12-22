#!/usr/bin/env bash
set -euo pipefail

MAX_RETRIES="$1"
SRC="$2"
DST="$3"

attempt=1
status=1

# Log enxuto: só mostra retries (se houver) e erro final.
while [[ $attempt -le $MAX_RETRIES ]]; do
  if scp -q "$SRC" "$DST"; then
    # Sucesso silencioso por arquivo.
    exit 0
  else
    status=$?

    # Só mostra log SE não for a primeira tentativa
    if [[ $attempt -gt 1 ]]; then
      echo "[scp] Retry ${attempt}/${MAX_RETRIES} (status ${status})"
    fi
  fi

  attempt=$((attempt+1))
  sleep 0.3
done

echo "[scp] FALHA: não foi possível enviar '${SRC}' após ${MAX_RETRIES} tentativas." >&2
exit $status

