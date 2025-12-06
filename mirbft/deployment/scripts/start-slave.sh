#!/bin/bash
#
# scripts/start-slave.sh
#
# Este script é chamado pelo start-remote-slaves.sh via SSH,
# já NO PRÓPRIO SLAVE (node-1..node-6 no Emulab).
# Ele:
#   - garante PATH, diretórios base e status;
#   - TENTA esperar o master ficar READY (master-ready no master),
#     mas NÃO TRAVA eternamente se o arquivo não aparecer;
#   - dispara o discoveryslave em background.
#
# Uso:
#   ./start-slave.sh <tag> <master_ip> <public_ip> <private_ip>

set -euo pipefail

source scripts/global-vars.sh

tag="$1"
master_ip="$2"
public_ip="$3"
private_ip="$4"

echo "[start-slave][INFO ] hostname=$(hostname) tag=$tag master_ip=$master_ip public_ip=$public_ip private_ip=$private_ip"

# Garante PATH corretinho no slave (bin do Go + bin local do repo)
export PATH="$remote_gopath/bin:$remote_work_dir/bin:$PATH"

# Arquivo de status remoto (opcional, mas já deixamos como RUNNING)
mkdir -p "$(dirname "$remote_status_file")"
echo "RUNNING" > "$remote_status_file"

# Garante diretório de trabalho e raiz de experiment-output
mkdir -p "$remote_work_dir"
cd "$remote_work_dir"
mkdir -p "$remote_work_dir/experiment-output"

###############################################################################
# Tentativa de esperar o master-ready, mas com TIMEOUT para não travar
###############################################################################

echo "[start-slave][INFO ] Verificando READY do master em $master_ip ($remote_ready_file)..."

max_loops=6  # 6 * machine_status_poll_period (tipicamente 5s) = ~30s
loop=0
master_ready=false

while [ $loop -lt $max_loops ]; do
  if ssh $ssh_options -q -o "ConnectTimeout=10" "Bruno@${master_ip}" "test -f '$remote_ready_file'"; then
    echo "[start-slave][INFO ] Master READY detectado após $((loop * machine_status_poll_period)) segundos."
    master_ready=true
    break
  fi

  echo "[start-slave][INFO ] Master ainda não está READY. Aguardando $machine_status_poll_period segundos... (tentativa $((loop+1))/$max_loops)"
  sleep "$machine_status_poll_period"
  loop=$((loop+1))
done

if [ "$master_ready" = false ]; then
  echo "[start-slave][WARN ] master-ready NÃO foi detectado após $((max_loops * machine_status_poll_period)) segundos."
  echo "[start-slave][WARN ] Prosseguindo mesmo assim e iniciando discoveryslave."
fi

###############################################################################
# Iniciar discoveryslave
###############################################################################

echo "[start-slave][INFO ] Iniciando discoveryslave..."

slave_cmd="
  ulimit -Sn $open_files_limit &&
  cd '$remote_work_dir' &&
  discoveryslave '$tag' '$master_ip:$master_port' '$public_ip' '$private_ip'
"

echo "[start-slave][INFO ] Comando: $slave_cmd"

# Roda em background no próprio slave, logando em um arquivo
nohup bash -c "$slave_cmd" > \"$remote_work_dir/slave-$tag.log\" 2>&1 &

echo "[start-slave][INFO ] discoveryslave para tag=$tag disparado em background."

