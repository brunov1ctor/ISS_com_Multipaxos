#!/bin/bash
#
# Wrapper "teimoso" para scp: tenta N vezes antes de desistir.
#
# Uso:
#   stubborn-scp.sh <tentativas> [opções scp...] <origem> <destino>
#
# Exemplo:
#   stubborn-scp.sh 10 -i ~/.ssh/id_rsa arquivo.txt user@host:/caminho/
#

if [ $# -lt 3 ]; then
  echo "Uso: $0 <tentativas> [opções scp...] <origem> <destino>" >&2
  exit 1
fi

max_tries="$1"
shift

# Separar origem e destino (últimos dois argumentos)
if [ $# -lt 2 ]; then
  echo "Erro: argumentos insuficientes para scp." >&2
  exit 1
fi

src="${@: -2:1}"
dst="${@: -1:1}"
scp_opts=("${@:1:$(($#-2))}")

try=1
status=1

timestamp() {
  date +"%Y-%m-%d %H:%M:%S"
}

while [ "$try" -le "$max_tries" ]; do
  echo "[$(timestamp)] Tentativa $try/$max_tries: scp '${src}' '${dst}'"
  scp "${scp_opts[@]}" "$src" "$dst"
  status=$?
  if [ $status -eq 0 ]; then
    echo "[$(timestamp)] scp concluído com sucesso."
    exit 0
  fi
  echo "[$(timestamp)] scp falhou com status $status. Tentando novamente..." >&2
  try=$((try+1))
  sleep 1
done

echo "[$(timestamp)] Desisti após $max_tries tentativas. Último status: $status" >&2
exit $status

