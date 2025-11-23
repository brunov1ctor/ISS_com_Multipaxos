#!/bin/bash

set -euo pipefail

exp_data_dir="$1"
tag="$2"
n="$3"
master_ip="$4"
shift 4

echo "====================================================================="
echo "=== [start-remote-slaves] INÍCIO ===================================="
echo "  exp_data_dir = $exp_data_dir"
echo "  tag          = $tag"
echo "  n            = $n"
echo "  master_ip    = $master_ip"
echo "  args rest    = $*"
echo "====================================================================="

# Diretórios locais/remotos dos binários
local_gopath="/users/Bruno/go"
local_bin_dir="$local_gopath/bin"
remote_gopath="/users/Bruno/go"
remote_bin_dir="$remote_gopath/bin"

# Opções de SSH/SCP (para evitar prompts de host key e manter semelhante ao deploy.sh)
ssh_options="-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ServerAliveInterval=60"

echo
echo "==== [start-remote-slaves] (LOCAL) Verificando binários em $local_bin_dir ===="
echo "  remote_gopath = $remote_gopath"
echo "  local_bin_dir = $local_bin_dir"

for bin in discoverymaster discoveryslave orderingpeer orderingclient; do
  if [ -x "$local_bin_dir/$bin" ]; then
    echo "  [LOCAL] OK     : $local_bin_dir/$bin"
  else
    echo "  [LOCAL] ERRO   : $local_bin_dir/$bin não existe ou não é executável!"
    exit 1
  fi
done

echo "==== [start-remote-slaves] Binários locais OK. ===="
echo

###############################################################################
# PARSE DOS ARGUMENTOS RESTANTES
# Ignora blocos 'skip X tag' e monta uma lista de nós:
#   nodes[i] = "instance_id|public_ip|private_ip|role|node_tag"
###############################################################################

nodes=()
args=( "$@" )
i=0
total=${#args[@]}

# OBS: aqui NÃO aplicamos lógica de 'skip' para seleção de nós.
#      Apenas limpamos os tokens 'skip' para conseguir extrair os grupos (5 em 5).
while [ "$i" -lt "$total" ]; do
  if [ "${args[$i]}" = "skip" ]; then
    # pular "skip <num> <tag>"
    if [ "$((i+2))" -ge "$total" ]; then
      echo "WARN: bloco 'skip' incompleto nos argumentos: ${args[*]}" >&2
      break
    fi
    i=$((i+3))
    continue
  fi

  if [ "$((i+4))" -ge "$total" ]; then
    echo "WARN: argumentos restantes não formam um grupo completo de 5 campos a partir de '${args[$i]}'" >&2
    break
  fi

  instance_id="${args[$i]}"
  public_ip="${args[$i+1]}"
  private_ip="${args[$i+2]}"
  role="${args[$i+3]}"
  node_tag="${args[$i+4]}"

  nodes+=( "${instance_id}|${public_ip}|${private_ip}|${role}|${node_tag}" )

  i=$((i+5))
done

###############################################################################
# ETAPA 1: GARANTIR BINÁRIOS EM TODOS OS SLAVES
###############################################################################

echo "==== [start-remote-slaves] (REMOTO) Garantindo binários em todos os slaves listados ===="

for node in "${nodes[@]}"; do
  IFS='|' read -r instance_id public_ip private_ip role node_tag <<< "$node"

  echo "---------------------------------------------------------------------"
  echo "  [REMOTO] Nó $instance_id ($public_ip / $private_ip)"
  echo "           role=$role tag=$node_tag"
  echo "---------------------------------------------------------------------"

  if [ "$role" != "slave" ]; then
    echo "    [REMOTO] role='$role' (provavelmente master); ignorando distribuição de binários."
    continue
  fi

  echo "---------------------------------------------------------------------"
  echo "  [REMOTO] Garantindo binários em $public_ip"
  echo "           remote_gopath = $remote_gopath"
  echo "           remote_bin_dir = $remote_bin_dir"
  echo "---------------------------------------------------------------------"

  # Cria diretório remoto
  ssh $ssh_options "$public_ip" "mkdir -p '$remote_bin_dir'"

  # Copia binários sempre (idempotente, só sobrescreve)
  scp $ssh_options \
    "$local_bin_dir/discoverymaster" \
    "$local_bin_dir/discoveryslave"  \
    "$local_bin_dir/orderingpeer"    \
    "$local_bin_dir/orderingclient"  \
    "$public_ip:$remote_bin_dir/"

  echo "    [REMOTO-discoverymaster] OK em $public_ip:$remote_bin_dir/discoverymaster"
  echo "    [REMOTO-discoveryslave]  OK em $public_ip:$remote_bin_dir/discoveryslave"
  echo "    [REMOTO-orderingpeer]    OK em $public_ip:$remote_bin_dir/orderingpeer"
  echo "    [REMOTO-orderingclient]  OK em $public_ip:$remote_bin_dir/orderingclient"
done

echo "==== [start-remote-slaves] Distribuição/garantia de binários concluída. ===="
echo

###############################################################################
# ETAPA 2: INICIAR APENAS OS NÓS COM A TAG SOLICITADA
# Aqui simplificamos: ignoramos 'skip' e apenas iniciamos nós cujo 'node_tag == tag',
# até atingir 'n' instâncias.
###############################################################################

echo "==== [start-remote-slaves] Iniciando loop pelos nós (n = $n) ===="

started=0
pids=()

for node in "${nodes[@]}"; do
  IFS='|' read -r instance_id public_ip private_ip role node_tag <<< "$node"

  echo "---------------------------------------------------------------------"
  echo "  [LOOP] instance_id=$instance_id"
  echo "         public_slave_ip=$public_ip"
  echo "         private_slave_ip=$private_ip"
  echo "         slave_role=$role"
  echo "         slave_tag=$node_tag"
  echo "         tag alvo=$tag, n restante=$((n - started))"
  echo "---------------------------------------------------------------------"

  # Só queremos nós com a tag correspondente (peers ou 1client, etc.)
  if [ "$node_tag" != "$tag" ]; then
    echo "  [LOOP] slave_tag='$node_tag' não bate com a tag alvo='$tag'; ignorando esse nó."
    continue
  fi

  if [ "$started" -ge "$n" ]; then
    echo "  [LOOP] Já atingimos n=$n nós iniciados para tag='$tag'; ignorando nós extras."
    continue
  fi

  echo "  [DEPLOY] Vai iniciar slave em $public_ip ($instance_id), tag=$tag"
  log_file="$exp_data_dir/ssh-$tag-$public_ip.log"
  echo "  [DEPLOY] Log remoto será gravado em: $log_file"

  # Dispara o script de inicialização do slave no host remoto.
  # Este script está em /users/Bruno/iss/start-slave.sh no remoto.
  ssh $ssh_options "$public_ip" \
    "/users/Bruno/iss/start-slave.sh '$tag' '$master_ip' '$public_ip' '$private_ip'" \
    > "$log_file" 2>&1 &

  pid=$!
  echo "  [DEPLOY] SSH disparado para $public_ip em background (PID=$pid)."
  echo "  [DEPLOY] Aguardando pequena pausa para não sobrecarregar o SSH."
  sleep 0.2

  pids+=( "$pid" )
  started=$((started + 1))
  echo "  [DEPLOY] n restante agora = $((n - started))"
done

echo
echo "==== [start-remote-slaves] Todos os SSHs disparados. Chamando 'wait' para aguardar término dos comandos locais. ===="

for pid in "${pids[@]}"; do
  wait "$pid" || true
done

echo "==== [start-remote-slaves] FIM ==========================================="
echo "====================================================================="

