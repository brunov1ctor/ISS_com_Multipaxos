#!/usr/bin/env bash
#
# start-remote-slaves.sh — versão corrigida e automatizada
#
# Agora com:
#   ✔ Compilação automática dos binários no node-0
#   ✔ Verificação segura dos binários compilados
#   ✔ Distribuição completa para todos os slaves
#   ✔ Inclusão do diretório scripts/ (inclui stubborn-scp.sh)
#   ✔ Permissões remotas corrigidas
#   ✔ SSH/SCP sem prompt de "yes" (StrictHostKeyChecking=no)
#
# Não usamos -e para evitar abortar se um ssh falhar em um nó.
#
set -uo pipefail

# Opções comuns de SSH/SCP para NÃO pedir confirmação de host
SSH_OPTS="-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ServerAliveInterval=60"

exp_data_dir="$1"
tag="$2"
n="$3"
master_ip="$4"
shift 4

orig_pwd="$(pwd)"

echo "====================================================================="
echo "=== [start-remote-slaves] INÍCIO ===================================="
echo "  exp_data_dir = ${exp_data_dir}"
echo "  tag          = ${tag}"
echo "  n            = ${n}"
echo "  master_ip    = ${master_ip}"
echo "  args rest    = $*"
echo "====================================================================="
echo

# ----------------------------------------------------------------------
# Caminhos locais e remotos
# ----------------------------------------------------------------------
remote_gopath="/users/Bruno/go"
remote_bin_dir="${remote_gopath}/bin"
remote_work_dir="/users/Bruno/iss"
remote_logs_dir="/users/Bruno/iss-logs"

# Repo e diretório de deploy
local_repo="/tmp/ISS_com_Multipaxos/mirbft"
# orig_pwd deve ser /tmp/ISS_com_Multipaxos/mirbft/deployment

local_bin_dir="/users/Bruno/go/bin"

local_discoverymaster="${local_bin_dir}/discoverymaster"
local_discoveryslave="${local_bin_dir}/discoveryslave"
local_orderingpeer="${local_bin_dir}/orderingpeer"
local_orderingclient="${local_bin_dir}/orderingclient"

# Estes são relativos ao diretório de deployment (orig_pwd)
local_start_slave_script="scripts/start-slave.sh"
local_scripts_dir="scripts"   # inclui stubborn-scp.sh

# ----------------------------------------------------------------------
# Auto-Compilação dos binários
# ----------------------------------------------------------------------
echo
echo "==== [start-remote-slaves] (LOCAL) Compilando binários ====="
echo "  Repositório: ${local_repo}"

cd "${local_repo}" || {
  echo "  [LOCAL] ERRO: diretório ${local_repo} não encontrado!"
  exit 1
}

export GOPATH=/users/Bruno/go
export GOBIN=/users/Bruno/go/bin
export PATH=$GOBIN:/usr/local/go/bin:$PATH

echo "  Executando: go install ./cmd/..."
if ! go install ./cmd/... ; then
  echo "  [LOCAL] ERRO: falha ao compilar binários!"
  exit 1
fi

echo "  [LOCAL] Compilação concluída."
echo

# Volta para o diretório original de deployment
cd "${orig_pwd}" || {
  echo "  [LOCAL] ERRO: não foi possível voltar para ${orig_pwd}"
  exit 1
}

# ----------------------------------------------------------------------
# Verificação dos binários compilados
# ----------------------------------------------------------------------
echo "==== [start-remote-slaves] (LOCAL) Verificando binários em ${local_bin_dir} ===="
echo "  remote_gopath = ${remote_gopath}"
echo "  local_bin_dir = ${local_bin_dir}"

for bin in "${local_discoverymaster}" "${local_discoveryslave}" "${local_orderingpeer}" "${local_orderingclient}"; do
  if [[ ! -x "${bin}" ]]; then
    echo "  [LOCAL] ERRO: binário não encontrado após compilação: ${bin}"
    exit 1
  else
    echo "  [LOCAL] OK: ${bin}"
  fi
done

# Verificar start-slave.sh (agora relativo ao diretório de deployment)
if [[ ! -f "${local_start_slave_script}" ]]; then
  echo "  [LOCAL] ERRO: script ${local_start_slave_script} não encontrado!"
  echo "         (cwd atual: $(pwd))"
  exit 1
fi

# Verificar stubborn-scp.sh
if [[ ! -f "${local_scripts_dir}/stubborn-scp.sh" ]]; then
  echo "  [LOCAL] ERRO: scripts/stubborn-scp.sh não encontrado!"
  exit 1
fi

echo "==== [start-remote-slaves] Binários + scripts verificados. ===="
echo

# ----------------------------------------------------------------------
# Função para preparar cada slave
# ----------------------------------------------------------------------
ensure_remote_slave() {
  local instance_id="$1"
  local public_ip="$2"
  local private_ip="$3"
  local role="$4"
  local slave_tag="$5"

  if [[ "${role}" != "slave" ]]; then
    echo "    [REMOTO] role='${role}' (não é slave); ignorando."
    return
  fi

  echo "---------------------------------------------------------------------"
  echo "  [REMOTO] Garantindo ambiente em ${public_ip}"
  echo "           instance_id = ${instance_id}"
  echo "           tag         = ${slave_tag}"
  echo "---------------------------------------------------------------------"

  # Criar diretórios remotos
  ssh $SSH_OPTS "Bruno@${public_ip}" "
    mkdir -p '${remote_bin_dir}' '${remote_work_dir}' '${remote_logs_dir}'
  " >/dev/null 2>&1

  # Copiar binários
  scp $SSH_OPTS \
    "${local_discoverymaster}" \
    "${local_discoveryslave}" \
    "${local_orderingpeer}" \
    "${local_orderingclient}" \
    "Bruno@${public_ip}:${remote_bin_dir}/" >/dev/null 2>&1

  # Copiar start-slave.sh
  scp $SSH_OPTS \
    "${local_start_slave_script}" \
    "Bruno@${public_ip}:${remote_work_dir}/start-slave.sh" >/dev/null 2>&1

  # Copiar diretório scripts/
  scp -r $SSH_OPTS \
    "${local_scripts_dir}" \
    "Bruno@${public_ip}:${remote_work_dir}/" >/dev/null 2>&1

  # Permissões
  ssh $SSH_OPTS "Bruno@${public_ip}" "
    chmod +x '${remote_bin_dir}/discoverymaster' \
             '${remote_bin_dir}/discoveryslave' \
             '${remote_bin_dir}/orderingpeer' \
             '${remote_bin_dir}/orderingclient' \
             '${remote_work_dir}/start-slave.sh' \
             '${remote_work_dir}/scripts/'*.sh || true
  " >/dev/null 2>&1

  echo "    [REMOTO] OK: ambiente garantido em ${public_ip}."
}

# ----------------------------------------------------------------------
# Distribuição
# ----------------------------------------------------------------------
echo "==== [start-remote-slaves] (REMOTO) Garantindo binários e scripts ===="

rest=("$@")
idx=0
total=${#rest[@]}

while (( idx < total )); do
  key="${rest[$idx]}"

  if [[ "${key}" == "skip" ]]; then
    ((idx+=3))
    continue
  fi

  if (( idx + 4 >= total )); then
    echo "  [REMOTO-AVISO] Argumentos insuficientes."
    break
  fi

  ensure_remote_slave \
    "${rest[$idx]}"       \
    "${rest[$((idx+1))]}" \
    "${rest[$((idx+2))]}" \
    "${rest[$((idx+3))]}" \
    "${rest[$((idx+4))]}"

  ((idx+=5))
done

echo "==== [start-remote-slaves] Distribuição concluída. ===="
echo

# ----------------------------------------------------------------------
# Disparar slaves da tag desejada
# ----------------------------------------------------------------------
echo "==== [start-remote-slaves] Iniciando slaves da TAG '${tag}' (n = ${n}) ===="

started=0
idx=0

while (( idx < total )); do
  key="${rest[$idx]}"

  if [[ "${key}" == "skip" ]]; then
    ((idx+=3))
    continue
  fi

  if (( idx + 4 >= total )); then break; fi

  instance_id="${rest[$idx]}"
  public_ip="${rest[$((idx+1))]}"
  private_ip="${rest[$((idx+2))]}"
  role="${rest[$((idx+3))]}"
  slave_tag="${rest[$((idx+4))]}"

  if [[ "${role}" != "slave" || "${slave_tag}" != "${tag}" ]]; then
    ((idx+=5))
    continue
  fi

  if (( started >= n )); then
    ((idx+=5))
    continue
  fi

  log_file="${exp_data_dir}/ssh-${tag}-${public_ip}.log"
  echo "  [DEPLOY] Iniciando slave em ${public_ip}"

  (
    ssh $SSH_OPTS "Bruno@${public_ip}" "
      cd '${remote_work_dir}' || exit 1
      nohup ./start-slave.sh \
        '${tag}' '${master_ip}' '${public_ip}' '${private_ip}' \
        > '${remote_logs_dir}/start-slave-${tag}.log' 2>&1 &
    "
  ) > "${log_file}" 2>&1 &

  started=$((started + 1))
  sleep 0.2

  ((idx+=5))
done

echo
echo "==== [start-remote-slaves] Todos os slaves disparados. ===="
wait || true
echo "==== [start-remote-slaves] FIM ==========================================="
echo "====================================================================="

