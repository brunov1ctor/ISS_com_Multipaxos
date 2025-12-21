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
  if (( attempt == 1 )); then
    echo "[stubborn-scp] Iniciando cópia (máx ${MAX_RETRIES} tentativas)."
    echo "[stubborn-scp]   origem : ${SRC}"
    echo "[stubborn-scp]   destino: ${DST}"
  else
    echo "[stubborn-scp] Nova tentativa ${attempt}/${MAX_RETRIES} para copiar '${SRC}' (último status: ${last_status})."
  fi

  # tentativa de cópia
  if scp "${SRC}" "${DST}"; then
    echo "[stubborn-scp] Sucesso ao copiar '${SRC}' na tentativa ${attempt}/${MAX_RETRIES}."
    exit 0
  else
    last_status=$?
    echo "[stubborn-scp] Falha na tentativa ${attempt}/${MAX_RETRIES} para copiar '${SRC}' (status ${last_status})."
  fi

  attempt=$((attempt + 1))
  sleep 1
done

echo "[stubborn-scp] Desisti de copiar '${SRC}' após ${MAX_RETRIES} tentativas. Último status: ${last_status}" >&2
exit "${last_status}"

