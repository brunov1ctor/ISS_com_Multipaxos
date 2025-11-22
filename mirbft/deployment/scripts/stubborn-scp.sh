#!/bin/bash
#
# stubborn-scp.sh
#
# Wrapper em volta do scp com tentativas de repetição.
#
# Interface esperada pelas master-commands:
#   stubborn-scp.sh <tentativas> -i <ID_FILE> <SRC> <DST>
# onde:
#   - <tentativas> é um inteiro (ex.: 10)
#   - "-i <ID_FILE>" é repassado para o scp (opção de identidade)
#   - <SRC> e <DST> são caminhos padrão do scp, podendo ser:
#       172.19.135.1:iss/experiment-config/config-0000.yml  config/config.yml
#     ou
#       experiment-output-0000-slave-__id__.tar.gz  172.19.135.1:iss/current-deployment-data/raw-results/
#

if [ "$#" -lt 3 ]; then
  echo "Uso: $0 <tentativas> [-i ID_FILE] <SRC> <DST>" >&2
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

identity=""
# Se vier "-i", tratamos como opção de identidade do scp
if [ "$1" = "-i" ]; then
  shift
  if [ "$#" -lt 3 ]; then
    echo "Uso: $0 <tentativas> [-i ID_FILE] <SRC> <DST>" >&2
    exit 1
  fi
  identity="$1"
  shift
fi

if [ "$#" -lt 2 ]; then
  echo "Uso: $0 <tentativas> [-i ID_FILE] <SRC> <DST>" >&2
  exit 1
fi

src="$1"
dst="$2"

# Monta comando scp (com ou sem -i)
scp_cmd=(scp)
if [ -n "$identity" ]; then
  scp_cmd+=( -i "$identity" )
fi

attempt=1
while [ "$attempt" -le "$retries" ]; do
  echo "[$(date '+%Y-%m-%d %H:%M:%S')] Tentativa $attempt/$retries: ${scp_cmd[*]} '$src' '$dst'"
  "${scp_cmd[@]}" "$src" "$dst"
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

