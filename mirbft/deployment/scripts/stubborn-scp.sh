#!/bin/bash

# Uso:
#   stubborn-scp.sh <tentativas> [opções-do-scp...] <SRC> <DEST>
#
# Exemplo:
#   ./scripts/stubborn-scp.sh 10 deployment-data/.../master-commands.cmd 172.20.3.2:/users/Bruno/iss/master-commands.cmd

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

# Extrai o host da forma HOST:CAMINHO
host="$dst"
host="${host%%:*}"

# Garante que a chave do host está em known_hosts para evitar prompts
if [ -n "$host" ]; then
  mkdir -p "$HOME/.ssh"
  # Se ainda não existe entrada para esse host, adiciona com ssh-keyscan
  if ! ssh-keygen -F "$host" >/dev/null 2>&1; then
    ssh-keyscan -H "$host" >> "$HOME/.ssh/known_hosts" 2>/dev/null || true
  fi
fi

# Opções de SSH/SCP para evitar qualquer interação
SSH_OPTS=(
  -o BatchMode=yes
  -o StrictHostKeyChecking=no
  -o UserKnownHostsFile="$HOME/.ssh/known_hosts"
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

