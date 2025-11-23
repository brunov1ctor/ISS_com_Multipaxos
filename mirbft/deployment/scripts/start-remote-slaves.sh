#!/bin/bash

###############################################################################
# start-remote-slaves.sh
#
# Inicia slaves remotos (peers / clientes) a partir das informações já
# "achatadas" que o deploy.sh passa na linha de comando.
#
# Além de apenas disparar os slaves, este script agora também garante que,
# para cada nó remoto listado, os binários Go necessários
# (discoverymaster, discoveryslave, orderingpeer, orderingclient)
# estejam presentes em /users/$USER/go/bin do respectivo nó.
###############################################################################

set -e

# Carrega variáveis globais (ssh_options, etc.)
. "$(dirname "$0")/global-vars.sh"

if [ $# -lt 4 ]; then
    echo "Uso: $0 <exp_data_dir> <tag> <n> <master_ip> [skip <k> <tag>] <instance list...>" >&2
    exit 1
fi

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
echo

# Diretório local dos binários Go (no node-0).
local_gopath="${GOPATH:-/users/$USER/go}"
local_bin_dir="${local_gopath}/bin"

echo "==== [start-remote-slaves] (LOCAL) Verificando binários em ${local_bin_dir} ===="
echo "  remote_gopath = /users/$USER/go"
echo "  local_bin_dir = ${local_bin_dir}"

BINARIES=(discoverymaster discoveryslave orderingpeer orderingclient)

for bin in "${BINARIES[@]}"; do
    if [ ! -x "${local_bin_dir}/${bin}" ]; then
        echo "  [LOCAL] ERRO  : ${local_bin_dir}/${bin} não existe ou não é executável."
        echo "                  Compile os binários primeiro (go install ...) e tente novamente."
        exit 1
    else
        echo "  [LOCAL] OK     : ${local_bin_dir}/${bin}"
    fi
done
echo "==== [start-remote-slaves] Binários locais OK. ===="
echo

# Tratamento opcional de 'skip <k> <tag>'
skip=0
skip_tag=""
if [ "$1" = "skip" ]; then
    skip="$2"
    skip_tag="$3"
    echo "==== [start-remote-slaves] Encontrado parâmetro 'skip': skip=$skip skip_tag=$skip_tag ===="
    shift 3
fi

# Guarda todos os argumentos restantes (lista de instâncias) em um array
nodes_args=("$@")

###############################################################################
# Função: garante que os binários existam no nó remoto.
###############################################################################
ensure_remote_bins() {
    local ip="$1"
    local user_home="/users/$USER"
    local remote_gopath="${GOPATH:-${user_home}/go}"
    local remote_bin_dir="${remote_gopath}/bin"

    echo "---------------------------------------------------------------------"
    echo "  [REMOTO] Garantindo binários em ${ip}"
    echo "           remote_gopath = ${remote_gopath}"
    echo "           remote_bin_dir = ${remote_bin_dir}"
    echo "---------------------------------------------------------------------"

    # Garante diretório remoto
    ssh $ssh_options "${ip}" "mkdir -p '${remote_bin_dir}'" >/dev/null 2>&1 || {
        echo "    [REMOTO:${ip}] ERRO ao criar diretório ${remote_bin_dir}"
        return 1
    }

    for bin in "${BINARIES[@]}"; do
        local local_path="${local_bin_dir}/${bin}"
        # Verifica remoto; se já existe e é executável, não copia.
        ssh $ssh_options "${ip}" "test -x '${remote_bin_dir}/${bin}'" >/dev/null 2>&1
        if [ $? -eq 0 ]; then
            echo "    [REMOTO-${bin}] Já existe em ${ip}:${remote_bin_dir}/${bin}"
            continue
        fi

        echo "    [REMOTO:${ip}] Copiando binário '${bin}' para ${remote_bin_dir}/ ..."
        scp $ssh_options "${local_path}" "${ip}:${remote_bin_dir}/" >/dev/null 2>&1 || {
            echo "    [REMOTO-${bin}] ERRO ao copiar para ${ip}"
            continue
        }

        # Confere permissão de execução
        ssh $ssh_options "${ip}" "chmod +x '${remote_bin_dir}/${bin}'" >/dev/null 2>&1 || true
        echo "    [REMOTO-${bin}] OK: ${ip}:${remote_bin_dir}/${bin}"
    done
}

###############################################################################
# Primeiro passo: distribuir/garantir binários em TODOS os slaves listados.
# A lista de instâncias vem nos argumentos, em grupos de 5:
#   instance_id public_ip private_ip role tag
###############################################################################
echo "==== [start-remote-slaves] (REMOTO) Garantindo binários em todos os slaves listados ===="

i=0
total=${#nodes_args[@]}
while [ $i -lt $total ]; do
    instance_id="${nodes_args[$i]}"
    public_slave_ip="${nodes_args[$i+1]}"
    private_slave_ip="${nodes_args[$i+2]}"
    slave_role="${nodes_args[$i+3]}"
    slave_tag_i="${nodes_args[$i+4]}"

    echo "---------------------------------------------------------------------"
    echo "  [REMOTO] Nó ${instance_id} (${public_slave_ip} / ${private_slave_ip})"
    echo "           role=${slave_role} tag=${slave_tag_i}"
    echo "---------------------------------------------------------------------"

    # Só precisa garantir binários nos nós que são 'slave' (não no master).
    if [ "${slave_role}" = "slave" ]; then
        ensure_remote_bins "${public_slave_ip}"
    else
        echo "    [REMOTO] role='${slave_role}' (provavelmente master); ignorando distribuição de binários."
    fi

    i=$((i + 5))
done

echo "==== [start-remote-slaves] Distribuição/garantia de binários concluída. ===="
echo

###############################################################################
# Segundo passo: seguir lógica original de 'skip' e disparo dos slaves.
###############################################################################
echo "==== [start-remote-slaves] Processando 'skip' nos argumentos extras ===="
echo "  args atuais: $*"
echo
echo "  [SKIP] Valor final de 'skip' para tag '${tag}': ${skip}"
echo "==== [start-remote-slaves] Iniciando loop pelos nós (n = ${n}) ===="
echo

# Restaura $@ a partir de nodes_args para o loop principal
set -- "${nodes_args[@]}"

# Loop principal (igual ao original, mas com mais logs)
while [ $# -ge 5 ] && [ "$n" -gt 0 ]; do
    instance_id="$1"
    public_slave_ip="$2"
    private_slave_ip="$3"
    slave_role="$4"
    slave_tag="$5"
    shift 5

    echo "---------------------------------------------------------------------"
    echo "  [LOOP] instance_id=${instance_id}"
    echo "         public_slave_ip=${public_slave_ip}"
    echo "         private_slave_ip=${private_slave_ip}"
    echo "         slave_role=${slave_role}"
    echo "         slave_tag=${slave_tag}"
    echo "         tag alvo=${tag}, skip atual=${skip}, n restante=${n}"
    echo "---------------------------------------------------------------------"

    if [ "$slave_tag" != "$tag" ]; then
        echo "  [LOOP] slave_tag='${slave_tag}' não bate com a tag alvo='${tag}'; ignorando esse nó."
        continue
    fi

    if [ "$skip" -gt 0 ]; then
        echo "  [LOOP] skip=${skip} > 0 e slave_tag='${slave_tag}' == tag alvo; ignorando e decrementando skip."
        skip=$((skip - 1))
        continue
    fi

    echo "  [DEPLOY] Vai iniciar slave em ${public_slave_ip} (${instance_id}), tag=${slave_tag}"
    echo "  [DEPLOY] Log remoto será gravado em: ${exp_data_dir}/ssh-${slave_tag}-${public_slave_ip}.log"

    scripts/start-slave.sh "$slave_tag" "$master_ip" "$public_slave_ip" "$private_slave_ip" > "${exp_data_dir}/ssh-${slave_tag}-${public_slave_ip}.log" 2>&1 &
    echo "  [DEPLOY] start-slave.sh disparado em background (PID=$!)."
    echo "  [DEPLOY] Aguardando pequena pausa para não sobrecarregar o SSH."
    echo
    sleep 0.2

    n=$((n - 1))
    echo "  [DEPLOY] n restante agora = ${n}"
    echo
done

echo "==== [start-remote-slaves] Todos os SSHs disparados. Chamando 'wait' para aguardar término dos comandos locais. ===="
wait || true
echo "==== [start-remote-slaves] FIM ==========================================="
echo "====================================================================="

