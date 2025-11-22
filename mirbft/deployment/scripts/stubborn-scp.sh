#!/bin/bash
#
# stubborn-scp.sh
#
# Wrapper simples em volta do scp com tentativas de repetição.
#
# Uso esperado pelas master-commands:
#   stubborn-scp.sh <tentativas> -i <SRC> <DST>
# onde:
#   - <tentativas> é um inteiro (por ex.: 10)
#   - "-i" é só um marcador (não é a opção -i do scp), então ignoramos
#   - <SRC> e <DST> são caminhos padrão do scp, podendo ser:
#       172.19.135.1:experiment-config/config-0000.yml  config/config.yml
#     ou
#       experiment-output-0000-slave-__id__.tar.gz  172.19.135.1:current-deployment-data/raw-results/
#

if [ "$#" -lt 3 ]; then
  echo "Uso: $0 <tentativas> [-i] <SRC> <DST>" >&2
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

# Ignora o "-i" "fake" usado pelas master-commands
if [ "$1" = "-i" ]; then
  shift
fi

if [ "$#" -lt 2 ]; then
  echo "Uso: $0 <tentativas> [-i] <SRC> <DST>" >&2
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
