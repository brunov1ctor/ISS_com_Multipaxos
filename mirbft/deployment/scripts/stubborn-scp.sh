#!/usr/bin/env bash
set -euo pipefail

MAX_RETRIES="${1:-5}"
SRC="${2:-}"
DST="${3:-}"

if [[ -z "${SRC}" || -z "${DST}" ]]; then
  echo "[stubborn-scp] Uso: $0 <tentativas> <src> <dst>" >&2
  exit 2
fi

attempt=1
last_status=1

while (( attempt <= MAX_RETRIES )); do
  echo "[stubborn-scp] Tentativa ${attempt} de ${MAX_RETRIES}..."
  # Opções para evitar qualquer prompt de host key
  if scp -o StrictHostKeyChecking=no \
         -o UserKnownHostsFile=/dev/null \
         -o LogLevel=ERROR \
         "${SRC}" "${DST}"; then
    echo "[stubborn-scp] Cópia local->remoto concluída com sucesso."
    exit 0
  else
    last_status=$?
    echo "[stubborn-scp] Falha na tentativa ${attempt} (status ${last_status})."
  fi

  attempt=$((attempt + 1))
  sleep 1
done

echo "[stubborn-scp] Desisti após ${MAX_RETRIES} tentativas. Último status: ${last_status}" >&2
exit "${last_status}"

