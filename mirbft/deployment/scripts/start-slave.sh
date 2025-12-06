#!/bin/bash
#
# scripts/start-slave.sh
#
# Este script é chamado pelo start-remote-slaves.sh via SSH,
# já NO PRÓPRIO SLAVE (node-1..node-6 no Emulab).
# Ele:
#   - garante PATH, diretórios base e status;
#   - espera o master ficar READY (master-ready no master);
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

echo "[start-slave][INFO ] Aguardando READY do master em $master_ip ($remote_ready_file)..."

# Espera o master criar o arquivo master-ready
while ! ssh $ssh_options -q -o "ConnectTimeout=10" "Bruno@${master_ip}" "test -f '$remote_ready_file'"; do
  echo "[start-slave][INFO ] Master ainda não está READY. Aguardando $machine_status_poll_period segundos..."
  sleep "$machine_status_poll_period"
done

echo "[start-slave][INFO ] Master READY detectado. Iniciando discoveryslave..."

# Comando do slave: usa parâmetros padrão do teu ambiente.
slave_cmd="
  ulimit -Sn $open_files_limit &&
  cd '$remote_work_dir' &&
  discoveryslave '$tag' '$master_ip:$master_port' '$public_ip' '$private_ip'
"

echo "[start-slave][INFO ] Comando: $slave_cmd"

# Roda em background no próprio slave, logando em um arquivo
nohup bash -c "$slave_cmd" > "$remote_work_dir/slave-$tag.log" 2>&1 &

echo "[start-slave][INFO ] discoveryslave para tag=$tag disparado em background."

