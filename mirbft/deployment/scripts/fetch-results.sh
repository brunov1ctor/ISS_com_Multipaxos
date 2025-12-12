#!/bin/bash
set -euo pipefail

source scripts/global-vars.sh

master_ip="${1:-${MASTER_IP:-}}"
exp_dir="${2:-${exp_data_dir:-}}"

if [[ -z "${master_ip}" && -n "${instance_info_file:-}" && -f "${instance_info_file}" ]]; then
  master_ip="$(awk 'NF>=4 && $4=="master" {print $2; exit}' "${instance_info_file}" 2>/dev/null || true)"
fi

if [[ -z "${master_ip}" || -z "${exp_dir}" ]]; then
  echo "ERRO: fetch-results.sh precisa de master_ip e exp_dir." >&2
  echo "  master_ip='${master_ip:-}' exp_dir='${exp_dir:-}' instance_info_file='${instance_info_file:-}'" >&2
  exit 1
fi

mkdir -p "$exp_dir/experiment-output"

echo "[INFO] Iniciando fetch de resultados do master ${master_ip} para ${exp_dir}"
echo "[INFO] remote_user=${remote_user} remote_work_dir=${remote_work_dir}"
echo

# --------------------------------------------------------------------
# 0) Diagnóstico rápido no master (não falha o script)
# --------------------------------------------------------------------
echo "[INFO] Diagnóstico no master: listando dirs e procurando outputs..."
ssh $ssh_options "${remote_user}@${master_ip}" "
  set -e;
  echo '--- /users/Bruno/iss ---';
  ls -la '${remote_work_dir}' || true;
  echo '--- logs ---';
  ls -la '${remote_work_dir}/logs' 2>/dev/null || true;
  echo '--- find experiment-output* (depth 4) ---';
  find '${remote_work_dir}' -maxdepth 4 -type d -name 'experiment-output' -o -type f -name 'experiment-output-*.tar.gz' 2>/dev/null | head -n 80 || true;
" </dev/null || true
echo

# --------------------------------------------------------------------
# 1) Busca .tar.gz em múltiplos paths prováveis no master
# --------------------------------------------------------------------
tar_paths=(
  "${remote_work_dir}/experiment-output-*.tar.gz"
  "${remote_work_dir}/current-deployment-data/experiment-output-*.tar.gz"
  "${remote_work_dir}/current-deployment-data/**/experiment-output-*.tar.gz"
)

found_tar=false
for pat in "${tar_paths[@]}"; do
  echo "[INFO] Tentando baixar tar(s) do master: ${pat}"
  if rsync -rtz --progress -e "ssh $ssh_options" \
      "${remote_user}@${master_ip}:${pat}" \
      "$exp_dir/" ; then
    found_tar=true
  else
    echo "[WARN] Não encontrei nesse path: ${pat}"
  fi
  echo
done

# --------------------------------------------------------------------
# 2) Fallback: tenta baixar diretórios experiment-output de master e slaves
# --------------------------------------------------------------------
dir_paths=(
  "${remote_work_dir}/experiment-output/"
  "${remote_work_dir}/current-deployment-data/experiment-output/"
  "${remote_work_dir}/current-deployment-data/**/experiment-output/"
)

if [[ "$found_tar" != "true" ]]; then
  echo "[INFO] Nenhum tar encontrado. Fallback: tentando rsync de diretórios experiment-output..."
  echo

  echo "[INFO] Tentando no master primeiro..."
  for dpat in "${dir_paths[@]}"; do
    echo "[INFO] - master dir: ${dpat}"
    rsync -rtz --progress -e "ssh $ssh_options" \
      "${remote_user}@${master_ip}:${dpat}" \
      "$exp_dir/experiment-output/" || true
  done
  echo

  if [[ -z "${instance_info_file:-}" || ! -f "${instance_info_file:-}" ]]; then
    echo "[ERRO] instance_info_file não definido/encontrado; não dá para varrer slaves." >&2
    exit 1
  fi

  while read -r instance_id ctrl_ip data_ip role tag; do
    [[ -z "${instance_id:-}" ]] && continue
    [[ "${instance_id:-}" =~ ^# ]] && continue
    [[ "${role:-}" != "slave" ]] && continue

    echo "[INFO] - slave ${instance_id} (${tag}) @ ${ctrl_ip}: tentando dirs..."
    for dpat in "${dir_paths[@]}"; do
      rsync -rtz --progress -e "ssh $ssh_options" \
        "${remote_user}@${ctrl_ip}:${dpat}" \
        "$exp_dir/experiment-output/" || true
    done
    echo
  done < "${instance_info_file}"
fi

echo "[INFO] fetch-results finalizado."

