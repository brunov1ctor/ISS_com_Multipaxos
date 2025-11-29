#!/bin/bash

# scripts/fetch-results.sh (VERSÃO SIMPLIFICADA PARA EMULAB)
#
# Uso:
#   scripts/fetch-results.sh <master_ip> <exp_dir>
#
# Exemplo:
#   scripts/fetch-results.sh 172.20.150.1 deployment-data/remote-0000
#
# O que faz:
#   - NÃO espera mais por master-ready nem por status DONE/ANALYZED.
#   - Apenas baixa:
#       * raw-results (tar.gz) do master
#       * scripts/, queries/ e master-log
#   - Depois você roda a análise localmente:
#       scripts/analyze/extract-successful.sh ...
#       scripts/analyze/summarize.sh ...
#
# Motivo:
#   No setup atual, ninguém cria $remote_ready_file nem atualiza
#   $remote_status_file para DONE/ANALYZED, então o script original
#   ficava preso em loops infinitos. Esta versão assume que você
#   só roda fetch-results.sh DEPOIS que o experimento terminou no master.

set -euo pipefail

source scripts/global-vars.sh

# Mata filhos ao sair
trap "$trap_exit_command" EXIT

if [ $# -lt 2 ]; then
  echo "Uso: $0 <master_ip> <exp_dir>"
  exit 1
fi

master_ip="$1"
exp_dir="$2"
raw_results="$exp_dir/raw-results"

echo "=== [fetch-results] INÍCIO ==="
echo "  master_ip   = $master_ip"
echo "  exp_dir     = $exp_dir"
echo "  raw_results = $raw_results"
echo "  remote_exp_dir    = $remote_exp_dir"
echo "  remote_log_archs  = $remote_log_archives"
echo

# 1) Cria diretório de resultados crus
mkdir -p "$raw_results"

echo "[fetch-results] Baixando arquivos de log (raw-results) do master..."
echo "  rsync: $master_ip:$remote_exp_dir/raw-results/$remote_log_archives -> $raw_results"
rsync --progress -rtz -e "ssh $ssh_options" \
  "$master_ip:$remote_exp_dir/raw-results/$remote_log_archives" \
  "$raw_results" || echo "[fetch-results] Aviso: nenhum $remote_log_archives encontrado (talvez experimento não tenha gerado logs ainda?)."

echo
echo "[fetch-results] Listando raw-results em $raw_results:"
ls -lh "$raw_results" || echo "[fetch-results] (sem arquivos em $raw_results)"
echo

# 2) Baixa scripts, queries e master-log do master
echo "[fetch-results] Baixando scripts/, queries/ e master-log do master..."

rsync --progress -rtz -e "ssh $ssh_options" \
  "$master_ip:scripts" \
  "$exp_dir/" || echo "[fetch-results] Aviso: não foi possível baixar scripts/"

rsync --progress -rtz -e "ssh $ssh_options" \
  "$master_ip:queries" \
  "$exp_dir/" || echo "[fetch-results] Aviso: não foi possível baixar queries/"

rsync --progress -rtz -e "ssh $ssh_options" \
  "$master_ip:$remote_master_log" \
  "$exp_dir/$local_master_log" || echo "[fetch-results] Aviso: não foi possível baixar master-log."

echo
echo "=== [fetch-results] FIM (download concluído) ==="
echo
echo "Próximos passos sugeridos (no node-0):"
echo "  1) Extrair/analisar resultados:"
echo "       scripts/analyze/extract-successful.sh \\"
echo "         $exp_dir \\"
echo "         analyze \\"
echo "         $analysis_query_params -d"
echo
echo "  2) Gerar resumo CSV final:"
echo "       scripts/analyze/summarize.sh \\"
echo "         $exp_dir/$csv_filename \\"
echo "         $exp_dir/experiment-output \\"
echo "         > $exp_dir/$result_summary_file"
echo

