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
#            Bruno@<ip_ctrl>:/users/Bruno/iss/experiment-output
#          para:
#            <exp_dir>/experiment-output
#   3) No final mostra o que veio e imprime próximos passos
#
# Depois disso:
#   - você roda analyze.sh em cada experimento (0000..0003)
#   - depois summarize.sh para gerar o CSV de métricas.

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

mkdir -p "$raw_results"

############################################
# 1) TENTATIVA PADRÃO: .tar.gz no MASTER  #
############################################
echo "[fetch-results] Tentando baixar arquivos de log (raw-results) do master..."
echo "  rsync: $master_ip:$remote_exp_dir/raw-results/$remote_log_archives -> $raw_results"

set +e
rsync --progress -rtz -e "ssh $ssh_options" \
  "$master_ip:$remote_exp_dir/raw-results/$remote_log_archives" \
  "$raw_results"
rsync_rc=$?
set -e

if [ $rsync_rc -ne 0 ]; then
  echo "[fetch-results] Aviso: não foi possível baixar $remote_log_archives do master (rc=$rsync_rc)."
fi

echo
echo "[fetch-results] Conteúdo de $raw_results:"
ls -lh "$raw_results" 2>/dev/null || echo "(vazio)"
echo

num_archives=$(ls -1 "$raw_results"/experiment-output-*-slave-*.tar.gz 2>/dev/null | wc -l || echo 0)

if [ "$num_archives" -gt 0 ]; then
  echo "[fetch-results] Encontrados $num_archives arquivos .tar.gz no master. (fluxo padrão ISS)"
  echo "=== [fetch-results] FIM (modo .tar.gz/master) ==="
  echo
  exit 0
fi

##########################################################
# 2) FALLBACK: BUSCAR LOGS DIRETO NOS SLAVES (Emulab)    #
##########################################################
echo "[fetch-results] Nenhum experiment-output-*.tar.gz encontrado no master."
echo "[fetch-results] Usando FALLBACK: rsync de experiment-output/ diretamente dos slaves."
echo

instance_info="scripts/instance-info"
if [ ! -f "$instance_info" ]; then
  echo "[fetch-results] ERRO: $instance_info não encontrado. Não sei de onde puxar os slaves."
  exit 1
fi

# Vamos jogar tudo em <exp_dir>/experiment-output
mkdir -p "$exp_dir/experiment-output"

echo "[fetch-results] Lendo slaves de $instance_info..."
while read -r host ctrl_ip exp_ip role tag; do
  # pula linhas vazias ou comentários
  [ -z "${host:-}" ] && continue
  [[ "$host" =~ ^# ]] && continue

  if [ "$role" != "slave" ]; then
    continue
  fi

  echo "  [fallback] Slave: host=$host ctrl_ip=$ctrl_ip role=$role tag=$tag"
  echo "    rsync Bruno@${ctrl_ip}:/users/Bruno/iss/experiment-output -> ${exp_dir}/"
  set +e
  rsync --progress -rtz -e "ssh $ssh_options" \
    "Bruno@${ctrl_ip}:/users/Bruno/iss/experiment-output" \
    "$exp_dir/"
  rc_slave=$?
  set -e

  if [ $rc_slave -ne 0 ]; then
    echo "    [fallback] Aviso: falha ao rsync de $host (rc=$rc_slave)."
  else
    echo "    [fallback] OK: logs copiados de $host."
  fi
  echo
done < "$instance_info"

echo
echo "[fetch-results] Resultado local em $exp_dir/experiment-output:"
ls -R "$exp_dir/experiment-output" 2>/dev/null || echo "(não há experiment-output/ local)"
echo

echo "=== [fetch-results] FIM (modo fallback/slaves) ==="
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

