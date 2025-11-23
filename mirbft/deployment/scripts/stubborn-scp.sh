#!/bin/bash

# Uso:
#   stubborn-scp.sh <tentativas> [opções-do-scp...] <SRC> <DEST>
#
# Exemplo:
#   stubborn-scp.sh 10 -i ~/.ssh/id_rsa arquivo foo@10.0.0.1:/tmp/arquivo
#
# Este script:
#   - força modo não interativo (sem perguntar "yes/no" de host key)
#   - repete o scp até <tentativas> vezes
#   - imprime logs com data/hora em stdout

if [ $# -lt 3 ]; then
  echo "Uso: $0 <tentativas> [opções-do-scp...] <SRC> <DEST>" >&2
  exit 1
fi

retries="$1"
shift

# Tudo menos os dois últimos argumentos são opções extras pro scp
extra_opts=()
while [ "$#" -gt 2 ]; do
  extra_opts+=("$1")
  shift
done

src="$1"
dst="$2"

# Opções de SSH/SCP para evitar qualquer interação
SSH_OPTS=(
  -o BatchMode=yes
  -o StrictHostKeyChecking=no
  -o UserKnownHostsFile=/dev/null
)

for ((i=1; i<=retries; i++)); do
  ts="$(date '+%Y-%m-%d %H:%M:%S')"
  echo "[$ts] Tentativa $i/$retries: scp '$src' '$dst'"

  scp "${SSH_OPTS[@]}" "${extra_opts[@]}" "$src" "$dst"
  status=$?

  if [ $status -eq 0 ]; then
    echo "[$ts] Sucesso na tentativa $i."
    exit 0
  fi

  echo "[$ts] Falha na tentativa $i (status=$status). Aguardando 3 segundos..."
  sleep 3
done

ts="$(date '+%Y-%m-%d %H:%M:%S')"
echo "[$ts] Todas as $retries tentativas falharam." >&2
exit 1

