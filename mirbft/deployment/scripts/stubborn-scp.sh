#!/bin/bash

# Uso: stubborn-scp.sh <tentativas> [opções-do-scp...] <SRC> <DEST>
# Exemplo:
#   ./scripts/stubborn-scp.sh 10 -P 2222 arquivo.txt user@host:/tmp/

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

# Opções de SSH/SCP para evitar interação (mas mantendo known_hosts normal)
SSH_OPTS=(
  -o BatchMode=yes
)

for ((i=1; i<=retries; i++)); do
  ts="$(date '+%Y-%m-%d %H:%M:%S')"
  echo "[$ts] Tentativa $i/$retries: scp '$src' '$dst'"

  scp "${SSH_OPTS[@]}" "${extra_opts[@]}" "$src" "$dst"
  status=$?

  if [ $status -eq 0 ]; then
    echo "[$ts] scp concluído com sucesso."
    exit 0
  fi

  echo "[$ts] scp falhou com status $status. Tentando novamente..."
  sleep 1
done

ts="$(date '+%Y-%m-%d %H:%M:%S')"
echo "[$ts] Desisti após $retries tentativas. Último status: $status" >&2
exit $status

