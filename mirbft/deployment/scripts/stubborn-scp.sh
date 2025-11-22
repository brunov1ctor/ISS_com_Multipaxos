#!/bin/bash
#
# stubborn-scp.sh
#
# Wrapper simples em volta do scp com tentativas de repetição.
#
# Uso esperado pelas master-commands:
#   stubborn-scp.sh <tentativas> -i <ALGUMA_COISA> <SRC> <DST>
#
# Neste ambiente, o "-i" e o argumento seguinte não são usados de verdade
# (às vezes nem vêm definidos), então os ignoramos e usamos apenas SRC/DST.

if [ "$#" -lt 3 ]; then
  echo "Uso: $0 <tentativas> [-i QUALQUER_COISA] <SRC> <DST>" >&2
  exit 1
fi

retries="$1"
shift

# Confere se o primeiro argumento é numérico
case "$retries" in
  ''|*[!0-9]*)
    echo "Primeiro argumento (tentativas) precisa ser um número inteiro. Recebi: '$retries'" >&2
    exit 1
    ;;
esac

# Se o próximo argumento for "-i", ignoramos ele e o argumento seguinte
if [ "$1" = "-i" ]; then
  shift          # tira o "-i"
  if [ "$#" -ge 1 ]; then
    shift        # joga fora o "ssh_key_file" (ou o que vier no lugar)
  fi
fi

if [ "$#" -lt 2 ]; then
  echo "Uso: $0 <tentativas> [-i QUALQUER_COISA] <SRC> <DST>" >&2
  exit 1
fi

src="$1"
dst="$2"

attempt=1
while [ "$attempt" -le "$retries" ]; do
  echo "[$(date '+%Y-%m-%d %H:%M:%S')] Tentativa $attempt/$retries: scp '$src' '$dst'"
  scp "$src" "$dst"
  status=$?

  if [ "$status" -eq 0 ]; then
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] scp concluído com sucesso."
    exit 0
  fi

  echo "[$(date '+%Y-%m-%d %H:%M:%S')] scp falhou com status $status. Tentando novamente..." >&2
  attempt=$((attempt + 1))
  sleep 1
done

echo "[$(date '+%Y-%m-%d %H:%M:%S')] Desisti após $retries tentativas. Último status: $status" >&2
exit "$status"

