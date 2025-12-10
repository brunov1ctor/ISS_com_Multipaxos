#!/bin/bash
#
# start-remote-slaves.sh
#
# Versão para ambiente remoto (Emulab / instance-info / binários já instalados).
#
# Uso:
#   scripts/start-remote-slaves.sh <exp_data_dir> <ignored_num> <tag> <instance_info_file>
#   Ex.: scripts/start-remote-slaves.sh deployment-data/remote-0000 5 peers   scripts/instance-info
#        scripts/start-remote-slaves.sh deployment-data/remote-0000 1 1client scripts/instance-info
#
# Este script:
#   - resolve o exp_data_dir de forma robusta (deployment/ vs repo root)
#   - detecta a tag (peers, 1client, etc.)
#   - detecta o arquivo instance-info
#   - garante diretórios remotos e copia scripts/binários
#   - dispara start-slave.sh em cada máquina com a tag desejada.
#

set -euo pipefail

# ---------------------------------------------------------------------------
# 0) Normalizar opções de SSH (sem chave fixa, sem spam)
# ---------------------------------------------------------------------------

ssh_options="-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR"

# ---------------------------------------------------------------------------
# 1) Parsing de argumentos
# ---------------------------------------------------------------------------

if [[ $# -ne 4 ]]; then
  echo "Uso: $0 <exp_data_dir> <ignored_num> <tag> <instance_info_file>" >&2
  exit 1
fi

exp_data_dir_arg="$1"
ignored_num="$2"      # mantido por compatibilidade, não é usado
wanted_tag="$3"
instance_info_arg="$4"

# ---------------------------------------------------------------------------
# 2) Diretórios locais básicos
# ---------------------------------------------------------------------------

this_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
deployment_dir="$(cd "${this_dir}/.." && pwd)"
repo_dir="$(cd "${deployment_dir}/.." && pwd)"

# Carrega variáveis globais se existir
if [[ -f "${this_dir}/global-vars.sh" ]]; then
  # shellcheck source=/dev/null
  source "${this_dir}/global-vars.sh"
fi

# Valores padrão para ambiente remoto (podem ser sobrescritos por global-vars.sh
# ou por variáveis de ambiente).
remote_user="${remote_user:-${DEPL_REMOTE_USER:-$USER}}"
remote_gopath="${remote_gopath:-/users/${remote_user}/go}"
remote_bin_dir="${remote_bin_dir:-${remote_gopath}/bin}"
remote_work_dir="${remote_work_dir:-/users/${remote_user}/iss}"
remote_exp_dir="${remote_exp_dir:-${remote_work_dir}/current-deployment-data}"

# ---------------------------------------------------------------------------
# 3) Resolve exp_data_dir e instance-info
# ---------------------------------------------------------------------------

resolve_exp_dir_slaves() {
  local exp_arg="$1"

  # Caminho absoluto?
  if [[ "$exp_arg" = /* ]]; then
    if [[ -d "$exp_arg" ]]; then
      echo "$exp_arg"
      return 0
    else
      return 1
    fi
  fi

  # Relativo ao diretório deployment
  local cand1="${deployment_dir}/${exp_arg}"
  # Relativo ao repositório (deployment/<exp_arg>)
  local cand2="${repo_dir}/${exp_arg}"

  if [[ -d "$cand1" ]]; then
    echo "$cand1"
    return 0
  fi
  if [[ -d "$cand2" ]]; then
    echo "$cand2"
    return 0
  fi

  return 1
}

resolve_instance_info() {
  local info_arg="$1"

  # Absoluto?
  if [[ "$info_arg" = /* && -f "$info_arg" ]]; then
    echo "$info_arg"
    return 0
  fi

  local cand1="${deployment_dir}/${info_arg}"
  local cand2="${repo_dir}/${info_arg}"

  if [[ -f "$cand1" ]]; then
    echo "$cand1"
    return 0
  fi
  if [[ -f "$cand2" ]]; then
    echo "$cand2"
    return 0
  fi

  # "Como está" (relativo ao CWD)
  if [[ -f "$info_arg" ]]; then
    echo "$info_arg"
    return 0
  fi

  return 1
}

if ! exp_data_dir="$(resolve_exp_dir_slaves "$exp_data_dir_arg")"; then
  echo "[ERRO  ][$(date +"%Y-%m-%d %H:%M:%S")] Diretório de experimento não encontrado para '$exp_data_dir_arg'." >&2
  exit 1
fi

if ! instance_info_file="$(resolve_instance_info "$instance_info_arg")"; then
  echo "[ERRO  ][$(date +"%Y-%m-%d %H:%M:%S")] Arquivo instance-info não encontrado para '$instance_info_arg'." >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# 4) Logs iniciais de contexto
# ---------------------------------------------------------------------------

echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] ==== [start-remote-slaves] Diretórios detectados ====="
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   this_dir           = ${this_dir}"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   deployment_dir     = ${deployment_dir}"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   repo_dir           = ${repo_dir}"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   exp_data_dir       = ${exp_data_dir}"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   instance_info_file = ${instance_info_file}"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   wanted_tag        = ${wanted_tag}"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   remote_user        = ${remote_user}"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   remote_gopath      = ${remote_gopath}"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   remote_bin_dir     = ${remote_bin_dir}"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   remote_work_dir    = ${remote_work_dir}"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")]   remote_exp_dir     = ${remote_exp_dir}"
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] "

# ---------------------------------------------------------------------------
# 5) Diretório temporário de scripts a serem enviados
# ---------------------------------------------------------------------------

tmp_scripts_dir="${exp_data_dir}/scripts-temp"
rm -rf "${tmp_scripts_dir}"
mkdir -p "${tmp_scripts_dir}"

cp "${this_dir}/start-slave.sh" "${tmp_scripts_dir}/"
cp "${this_dir}/stubborn-scp.sh" "${tmp_scripts_dir}/" 2>/dev/null || true
cp "${this_dir}/new-experiment-state.sh" "${tmp_scripts_dir}/" 2>/dev/null || true
cp "${this_dir}/global-vars.sh" "${tmp_scripts_dir}/" 2>/dev/null || true

# ---------------------------------------------------------------------------
# 6) Descobre master_ip (para repassar ao start-slave.sh)
# ---------------------------------------------------------------------------

master_ip=""

while read -r iid ctrl_ip data_ip role tag; do
  [[ -z "${iid}" ]] && continue
  [[ "${iid}" =~ ^# ]] && continue

  if [[ "${role}" == "master" || "${tag}" == "master" || "${iid}" == "master" ]]; then
    master_ip="${ctrl_ip}"
    break
  fi
done < "${instance_info_file}"

if [[ -z "${master_ip}" ]]; then
  echo "[ERRO  ][$(date +"%Y-%m-%d %H:%M:%S")] Não foi possível determinar master_ip a partir de '${instance_info_file}'." >&2
  exit 1
fi

echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] [start-remote-slaves] master_ip detectado = ${master_ip}"

# ---------------------------------------------------------------------------
# 7) Função para iniciar um slave para uma linha do instance-info
# ---------------------------------------------------------------------------

scp_retries="${SCP_RETRIES:-10}"

start_instance_line() {
  local line="$1"

  # Campos: instance_id ctrl_ip data_ip role tag
  local instance_id ctrl_ip data_ip role tag
  read -r instance_id ctrl_ip data_ip role tag <<< "$line"

  # Ignora linhas vazias ou comentários
  if [[ -z "${instance_id}" ]]; then
    return 0
  fi
  if [[ "${instance_id}" =~ ^# ]]; then
    return 0
  fi

  # Filtra pela tag desejada
  if [[ "${tag}" != "${wanted_tag}" ]]; then
    return 0
  fi

  echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] [start-remote-slaves] Iniciando slave '${instance_id}' (${tag}) em ${ctrl_ip}..."

  # Garante diretórios básicos no remoto
  ssh ${ssh_options} "${remote_user}@${ctrl_ip}" "mkdir -p '${remote_work_dir}/scripts' '${remote_work_dir}/logs' '${remote_exp_dir}' '${remote_bin_dir}'" || true

  # Copia scripts auxiliares (start-slave, stubborn-scp, etc.)
  bash "${this_dir}/stubborn-scp.sh" \
    "${scp_retries}" \
    "${tmp_scripts_dir}/" \
    "${remote_user}@${ctrl_ip}:${remote_work_dir}/scripts/"

  # Copia binários se existirem localmente
  local local_bin_dir="${LOCAL_BIN_DIR:-${remote_bin_dir}}"
  for bin in discoverymaster discoveryslave orderingpeer orderingclient; do
    if [[ -x "${local_bin_dir}/${bin}" ]]; then
      bash "${this_dir}/stubborn-scp.sh" \
        "${scp_retries}" \
        "${local_bin_dir}/${bin}" \
        "${remote_user}@${ctrl_ip}:${remote_bin_dir}/"
    fi
  done

  # Recupera IP público/privado dessa linha
  local public_ip="${ctrl_ip}"
  local private_ip="${data_ip}"

  # Dispara o start-slave.sh no host remoto em background
  ssh ${ssh_options} "${remote_user}@${ctrl_ip}" "
    cd '${remote_work_dir}/scripts' && \
    nohup ./start-slave.sh \
      '${tag}' \
      '${master_ip}' \
      '${public_ip}' \
      '${private_ip}' \
      > '${remote_work_dir}/logs/slave-${instance_id}.log' 2>&1 &
  " >/dev/null 2>&1 || true
}

# ---------------------------------------------------------------------------
# 8) Percorre o instance-info e inicia os slaves com a tag desejada
# ---------------------------------------------------------------------------

while IFS= read -r line; do
  start_instance_line "$line"
done < "${instance_info_file}"

echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] "
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] ==== [start-remote-slaves] Todos os slaves disparados. ===="
echo "[INFO  ][$(date +"%Y-%m-%d %H:%M:%S")] ==== [start-remote-slaves] FIM ==========================================="

