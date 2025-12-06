#!/bin/bash

# scripts/start-remote-slaves.sh
#
# Uso (do deploy-remote.sh):
#   scripts/start-remote-slaves.sh \
#       <exp_data_dir> <tag> <num_slaves> <master_ip> <instance_info_file>
#
# Onde:
#   tag               = "peers" ou "1client"
#   num_slaves        = quantos nós dessa tag queremos iniciar
#   master_ip         = IP do master (para o start-slave.sh remoto)
#   instance_info_file= arquivo scripts/instance-info

# Carrega variáveis globais (ssh_options, remote_work_dir, remote_bin_dir, etc.)
source scripts/global-vars.sh

# Garante que filhos morram se este script sair
trap "$trap_exit_command" EXIT

exp_data_dir="$1"
tag="$2"
num_slaves="$3"
master_ip="$4"
instance_info_file="$5"

# Diretórios locais (deployment e repo)
this_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
deployment_dir="$(cd "${this_dir}/.." && pwd)"
repo_dir="$(cd "${deployment_dir}/.." && pwd)"

echo "[INFO  ][$(date +"%F %T")] ==== [start-remote-slaves] Diretórios detectados ====="
echo "[INFO  ][$(date +"%F %T")]   this_dir       = ${this_dir}"
echo "[INFO  ][$(date +"%F %T")]   deployment_dir = ${deployment_dir}"
echo "[INFO  ][$(date +"%F %T")]   repo_dir       = ${repo_dir}"
echo "[INFO  ][$(date +"%F %T")]   remote_user    = ${remote_user}"
echo "[INFO  ][$(date +"%F %T")]   remote_gopath  = ${remote_gopath}"
echo "[INFO  ][$(date +"%F %T")]   remote_bin_dir = ${remote_bin_dir}"
echo "[INFO  ][$(date +"%F %T")]   remote_work_dir= ${remote_work_dir}"
echo "[INFO  ][$(date +"%F %T")]   remote_exp_dir = ${remote_exp_dir}"
echo "[INFO  ][$(date +"%F %T")] "

if [ ! -f "${instance_info_file}" ]; then
  echo "[ERROR ][$(date +"%F %T")] Arquivo de instance info não encontrado: ${instance_info_file}"
  exit 1
fi

# Diretório local dos binários (no node-0)
# Se GOBIN estiver setado, usa ele; senão, usa $GOPATH/bin; se não tiver, cai pro remote_gopath/bin
if [ -n "${GOBIN:-}" ]; then
  local_bin_dir="${GOBIN}"
elif [ -n "${GOPATH:-}" ]; then
  local_bin_dir="${GOPATH}/bin"
else
  local_bin_dir="${remote_gopath}/bin"
fi

echo "[INFO  ][$(date +"%F %T")] ==== [start-remote-slaves] Distribuindo scripts/binários aos slaves ===="

# ---------------------------------------------------------------------------
# 1) DISTRIBUIÇÃO DE SCRIPTS E BINÁRIOS
# ---------------------------------------------------------------------------
started=0
while read -r instance_id ctrl_ip data_ip role itag; do
  # pula linhas vazias ou comentários
  [ -z "${instance_id}" ] && continue
  case "${instance_id}" in
    \#*) continue ;;
  esac

  # só slaves com a tag desejada (peers ou 1client)
  if [ "${role}" != "slave" ] || [ "${itag}" != "${tag}" ]; then
    continue
  fi

  echo "[INFO  ][$(date +"%F %T")] ---------------------------------------------------------------------"
  echo "[INFO  ][$(date +"%F %T")]   [REMOTO] Garantindo ambiente em ${ctrl_ip}"
  echo "[INFO  ][$(date +"%F %T")]            instance_id = ${instance_id}"
  echo "[INFO  ][$(date +"%F %T")]            tag         = ${itag}"
  echo "[INFO  ][$(date +"%F %T")] ---------------------------------------------------------------------"

  # 1.1) Garante diretórios remotos
  ssh ${ssh_options} "${remote_user}@${ctrl_ip}" " \
    mkdir -p '${remote_work_dir}/scripts' '${remote_bin_dir}' '${remote_exp_dir}' 2>/dev/null || true \
  " >/dev/null 2>&1 || true

  # 1.2) Copia scripts básicos (para o nó remoto)
  scp ${ssh_options} \
    "${this_dir}/global-vars.sh" \
    "${this_dir}/remote-machine-status.sh" \
    "${this_dir}/stubborn-scp.sh" \
    "${remote_user}@${ctrl_ip}:${remote_work_dir}/scripts/" >/dev/null 2>&1 || true

  # 1.3) Copia start-slave.sh (para raiz de trabalho remota)
  scp ${ssh_options} \
    "${this_dir}/start-slave.sh" \
    "${remote_user}@${ctrl_ip}:${remote_work_dir}/start-slave.sh" >/dev/null 2>&1 || true

  # 1.4) Copia binários (desc/ord)
  scp ${ssh_options} \
    "${local_bin_dir}/discoverymaster" \
    "${local_bin_dir}/discoveryslave" \
    "${local_bin_dir}/orderingpeer" \
    "${local_bin_dir}/orderingclient" \
    "${remote_user}@${ctrl_ip}:${remote_bin_dir}/" >/dev/null 2>&1 || true

  echo "[INFO  ][$(date +"%F %T")]     [REMOTO] OK: ambiente garantido em ${ctrl_ip}."

  started=$((started + 1))
  [ "${started}" -ge "${num_slaves}" ] && break

  # Evita abrir SSH demais de uma vez
  sleep 0.1
done < "${instance_info_file}"

echo "[INFO  ][$(date +"%F %T")] ==== [start-remote-slaves] Distribuição concluída. ===="
echo "[INFO  ][$(date +"%F %T")] "

# ---------------------------------------------------------------------------
# 2) DISPARO DOS SLAVES (start-slave.sh remoto)
# ---------------------------------------------------------------------------
echo "[INFO  ][$(date +"%F %T")] ==== [start-remote-slaves] Disparando slaves da tag '${tag}' ===="

started=0
while read -r instance_id ctrl_ip data_ip role itag; do
  [ -z "${instance_id}" ] && continue
  case "${instance_id}" in
    \#*) continue ;;
  esac

  if [ "${role}" != "slave" ] || [ "${itag}" != "${tag}" ]; then
    continue
  fi

  echo "[INFO  ][$(date +"%F %T")]   [DEPLOY] Iniciando slave em ${ctrl_ip} (instance_id=${instance_id}, tag=${itag})"

  ssh ${ssh_options} "${remote_user}@${ctrl_ip}" " \
    cd '${remote_work_dir}' && \
    chmod +x start-slave.sh scripts/*.sh 2>/dev/null || true && \
    ./start-slave.sh '${tag}' '${master_ip}' '${ctrl_ip}' '${data_ip}' \
      >> '${remote_work_dir}/start-slave-${tag}.log' 2>&1 & \
  " >/dev/null 2>&1 || true

  started=$((started + 1))
  [ "${started}" -ge "${num_slaves}" ] && break

  sleep 0.1
done < "${instance_info_file}"

echo "[INFO  ][$(date +"%F %T")] "
echo "[INFO  ][$(date +"%F %T")] ==== [start-remote-slaves] Todos os slaves disparados. ===="
echo "[INFO  ][$(date +"%F %T")] ==== [start-remote-slaves] FIM ==========================================="
wait

