#!/bin/bash

# scripts/fetch-results.sh (VERSÃO EMULAB / MULTIPAXOS)
#
# Uso (execução direta):
#   scripts/fetch-results.sh <master_ip> <exp_dir>
#
# Uso (quando chamado via source pelo deploy.sh):
#   - usa MASTER_IP (se existir)
#   - senão tenta inferir master_ip do instance_info_file
#   - usa exp_data_dir como exp_dir (se existir)
#
set -euo pipefail

source scripts/global-vars.sh

master_ip="${1:-${MASTER_IP:-}}"
exp_dir="${2:-${exp_data_dir:-}}"

# Se não veio master_ip, tenta descobrir via instance-info (linha com role=master)
if [[ -z "${master_ip}" && -n "${instance_info_file:-}" && -f "${instance_info_file}" ]]; then
  master_ip="$(awk 'NF>=4 && $4=="master" {print $2; exit}' "${instance_info_file}" 2>/dev/null || true)"
fi

if [[ -z "${master_ip}" || -z "${exp_dir}" ]]; then
  echo "ERRO: fetch-results.sh precisa de master_ip e exp_dir." >&2
  echo "  - master_ip (arg1 ou MASTER_IP) = '${master_ip:-}'" >&2
  echo "  - exp_dir   (arg2 ou exp_data_dir) = '${exp_dir:-}'" >&2
  echo "Uso (execução direta): $0 <master_ip> <exp_dir>" >&2
  exit 1
fi

mkdir -p "$exp_dir/experiment-output"

echo "[INFO] Iniciando fetch de resultados do master ${master_ip} para ${exp_dir}"
echo "[INFO] remote_user=${remote_user} remote_work_dir=${remote_work_dir}"
echo

# --------------------------------------------------------------------
# 1) Tenta baixar .tar.gz (padrão ISS).
# --------------------------------------------------------------------

tar_glob="experiment-output-*.tar.gz"
found_tar=false

echo "[INFO] Tentando baixar arquivos $tar_glob do master (padrão ISS)..."

if rsync -rtz --progress -e "ssh $ssh_options" \
  "${remote_user}@${master_ip}:${remote_work_dir}/$tar_glob" \
  "$exp_dir/"; then
  found_tar=true
else
  echo "[WARN] Não foi possível baixar $tar_glob do master (pode não existir ainda)."
fi

echo

# --------------------------------------------------------------------
# 2) Fallback: copia experiment-output direto de cada slave via rsync
# --------------------------------------------------------------------

if [[ "$found_tar" != "true" ]]; then
  echo "[INFO] Fallback: baixando ${remote_work_dir}/experiment-output de cada slave via rsync..."
  echo "[INFO] instance_info_file=${instance_info_file:-<none>}"
  echo

  if [[ -z "${instance_info_file:-}" || ! -f "${instance_info_file:-}" ]]; then
    echo "[ERRO] instance_info_file não definido ou não encontrado; não dá para fazer fallback." >&2
    exit 1
  fi

  while read -r instance_id ctrl_ip data_ip role tag; do
    [[ -z "${instance_id:-}" ]] && continue
    [[ "${instance_id:-}" =~ ^# ]] && continue

    if [[ "${role:-}" != "slave" ]]; then
      continue
    fi

    echo "[INFO] - slave ${instance_id} (${tag}) @ ${ctrl_ip}: rsync experiment-output ..."
    rsync -rtz --progress -e "ssh $ssh_options" \
      "${remote_user}@${ctrl_ip}:${remote_work_dir}/experiment-output/" \
      "$exp_dir/experiment-output/" || true
  done < "${instance_info_file}"

  echo
  echo "[INFO] Fallback concluído."
fi

echo "[INFO] fetch-results finalizado."

