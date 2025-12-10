#!/bin/bash

# scripts/fetch-results.sh (VERSÃO EMULAB / MULTIPAXOS)
#
# Uso:
#   scripts/fetch-results.sh <master_ip> <exp_dir>
#
# Exemplo:
#   scripts/fetch-results.sh 172.20.150.1 deployment-data/remote-0000
#
# O que faz:
#   1) TENTA baixar os .tar.gz do master (padrão ISS)
#   2) SE NÃO HOUVER .tar.gz, faz FALLBACK:
#        - lê scripts/instance-info
#        - para cada linha com "slave", faz rsync de:
#            ${remote_user}@<ip_ctrl>:${remote_work_dir}/experiment-output
#          para:
#            <exp_dir>/experiment-output
#
set -euo pipefail

source scripts/global-vars.sh

master_ip="$1"
exp_dir="$2"

if [[ -z "$master_ip" || -z "$exp_dir" ]]; then
  echo "Uso: $0 <master_ip> <exp_dir>"
  exit 1
fi

mkdir -p "$exp_dir/experiment-output"

echo "[INFO] Iniciando fetch de resultados do master $master_ip para $exp_dir"
echo

# --------------------------------------------------------------------
# 1) Tenta baixar .tar.gz (padrão ISS).
# --------------------------------------------------------------------

tar_glob="experiment-output-*.tar.gz"
found_tar=false

echo "[INFO] Tentando baixar arquivos $tar_glob do master (padrão ISS)..."

rsync -rtz --progress -e "ssh $ssh_options" \
  "${remote_user}@${master_ip}:${remote_work_dir}/$tar_glob" \
  "$exp_dir/" && found_tar=true || true

if $found_tar; then
  echo "[INFO] Arquivos .tar.gz encontrados. Descompactando..."
  mkdir -p "$exp_dir/experiment-output"
  for f in "$exp_dir"/$tar_glob; do
    echo "  - Extraindo $f"
    tar -xzf "$f" -C "$exp_dir/experiment-output"
  done
  echo "[INFO] Extração concluída."
  exit 0
fi

echo "[INFO] Nenhum $tar_glob encontrado no master. Partindo para fallback (rsync de cada slave)."
echo

# --------------------------------------------------------------------
# 2) FALLBACK: rsync direto do experiment-output de cada slave.
# --------------------------------------------------------------------

instance_info_file="scripts/instance-info"

if [[ ! -f "$instance_info_file" ]]; then
  echo "[ERRO] Arquivo $instance_info_file não encontrado; não sei de onde puxar os resultados."
  exit 1
fi

echo "[INFO] Lendo lista de slaves em $instance_info_file..."
echo

while read -r line; do
  tag=$(echo "$line" | awk '{print $1}')
  ctrl_ip=$(echo "$line" | awk '{print $2}')

  # Linhas de comentário ou vazias
  [[ -z "$tag" ]] && continue
  [[ "$tag" == \#* ]] && continue

  # Só interessa slaves
  if [[ "$tag" != "slave" ]]; then
    continue
  fi

  echo "[INFO] Slave detectado:"
  echo "      tag    = $tag"
  echo "      ctrl_ip= $ctrl_ip"
  echo

  echo "    rsync ${remote_user}@${ctrl_ip}:${remote_work_dir}/experiment-output -> ${exp_dir}/"

  rsync --progress -rtz -e "ssh $ssh_options" \
    "${remote_user}@${ctrl_ip}:${remote_work_dir}/experiment-output" \
    "$exp_dir/" || true

  echo
done < "$instance_info_file"

echo "[INFO] Fetch de resultados via fallback concluído."
echo
echo "Próximos passos sugeridos (no node-0):"
echo "  1) Analisar cada experimento:"
echo "       for e in 0000 0001 0002 0003; do"
echo "         scripts/analyze/analyze.sh \\"
echo "           $exp_dir/experiment-output/\$e"
echo "       done"
echo
echo "  2) Gerar resumo CSV final:"
echo "       scripts/analyze/summarize.sh \\"
echo "         $exp_dir/$csv_filename \\"
echo "         $exp_dir/experiment-output \\"
echo "         > $exp_dir/$result_summary_file"
echo

