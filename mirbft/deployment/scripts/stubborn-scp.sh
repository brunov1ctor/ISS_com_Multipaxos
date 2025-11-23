#!/bin/bash

# Uso:
#   stubborn-scp.sh <tentativas> [opções-do-scp...] <SRC> <DEST>
#
# Exemplo:
#   stubborn-scp.sh 10 -i ~/.ssh/id_rsa arquivo foo@10.0.0.1:/tmp/arquivo

if [ $# < 3 ]; then
  echo "Uso: $0 <tentativas> [opções-do-scp...] <SRC> <DEST>" >&2
  exit 1
fi

retries="$1"
shift

# Vamos montar as opções extras de forma filtrada
extra_opts=()

while [ "$#" -gt 2 ]; do
  arg="$1"

  # Caso especial: "-o X=Y" separado em 2 argumentos
  if [ "$arg" = "-o" ] && [ "$#" -gt 3 ]; then
    next="$2"

    case "$next" in
      StrictHostKeyChecking=*|UserKnownHostsFile=*|BatchMode=* )
        # Ignora essa opção e seu valor
        shift 2
        continue
        ;;
    esac
  fi

  # Caso "-oX=Y" em um único argumento
  case "$arg" in
    -oStrictHostKeyChecking=*|-oUserKnownHostsFile=*|-oBatchMode=* )
      shift
      continue
      ;;
  esac

  extra_opts+=("$arg")
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

