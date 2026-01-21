#!/usr/bin/env bash
set -euo pipefail

# scripts/analyze/analyze.sh
#
# Padronizado (Opção A): SEM summarize.py.
# Este script:
#   1) carrega traces (*.trc) -> <exp_dir>/trace.db
#   2) executa queries SQL -> gera *.val dentro de <exp_dir>
#
# A sumarização/consolidação em CSV deve ser feita por:
#   scripts/analyze/summarize.sh <deployment.csv> <experiment-output-root> > result-summary.csv
#
# Uso típico:
#   scripts/analyze/analyze.sh -d path/to/experiment-output/0000 -q queries/aggregates.sql -q queries/histograms.sql
#
# Compatível com analyze-continuously.sh (aceita -d, -q, -f, etc.)

dbfile="trace.db"
exp_dir=""
queries=()

# flags legadas (mantidas pra compatibilidade; não são necessárias aqui)
peer_bin=""
client_bin=""
force=false

usage() {
  cat <<'USAGE'
Usage:
  scripts/analyze/analyze.sh -d EXP_DIR [-q QUERY.sql ...] [--db trace.db] [-f]

Required:
  -d EXP_DIR            Diretório do experimento (ex.: .../experiment-output/0000)

Optional:
  -q QUERY.sql          Query SQL (pode repetir -q várias vezes)
  --db FILE             Nome do arquivo sqlite gerado dentro de EXP_DIR (default: trace.db)
  -f, --force           Recria o DB (remove EXP_DIR/trace.db antes)
  -b PEER_BIN           (legado) ignorado
  -c CLIENT_BIN         (legado) ignorado
  -h, --help            Ajuda

Notes:
  - Este script gera *.val em EXP_DIR via diretivas '-- export' presentes nos .sql.
  - Para consolidar resultados em CSV, use summarize.sh (não é feito aqui).
USAGE
}

# parse args
while [[ $# -gt 0 ]]; do
  case "$1" in
    -d) exp_dir="${2:-}"; shift 2 ;;
    --db) dbfile="${2:-}"; shift 2 ;;
    -q) queries+=("${2:-}"); shift 2 ;;
    -b) peer_bin="${2:-}"; shift 2 ;;      # legado, ignorado
    -c) client_bin="${2:-}"; shift 2 ;;    # legado, ignorado
    -f|--force) force=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *)
      # permite passar EXP_DIR sem -d como primeiro argumento
      if [[ -z "$exp_dir" && -d "$1" ]]; then
        exp_dir="$1"; shift
      else
        echo "Unknown arg: $1" >&2
        usage >&2
        exit 2
      fi
      ;;
  esac
done

if [[ -z "$exp_dir" || ! -d "$exp_dir" ]]; then
  echo "EXP_DIR inválido (use -d)." >&2
  usage >&2
  exit 1
fi

# descobrir queries padrão se nenhuma foi passada:
# pega analysis_query_params de scripts/global-vars.sh (contexto mirbft/deployment)
if [[ ${#queries[@]} -eq 0 ]]; then
  # shellcheck source=/dev/null
  if [[ -f "scripts/global-vars.sh" ]]; then
    source scripts/global-vars.sh
    if [[ -n "${analysis_query_params:-}" ]]; then
      # analysis_query_params é algo como: "-q queries/aggregates.sql -q queries/histograms.sql"
      # transforma em array de caminhos
      while read -r tok; do
        [[ "$tok" == "-q" || -z "$tok" ]] && continue
        queries+=("$tok")
      done < <(echo "$analysis_query_params" | tr ' ' '\n')
    fi
  fi
fi

if [[ ${#queries[@]} -eq 0 ]]; then
  echo "[warn] Nenhuma query foi informada (-q) e não foi possível obter defaults via scripts/global-vars.sh." >&2
  echo "       Rode assim, por exemplo:" >&2
  echo "       scripts/analyze/analyze.sh -d \"$exp_dir\" -q queries/aggregates.sql -q queries/histograms.sql" >&2
  exit 1
fi

# localizar traces
shopt -s nullglob
trc_files=("$exp_dir"/slave-*/*.trc "$exp_dir"/*.trc)
shopt -u nullglob

if [[ ${#trc_files[@]} -eq 0 ]]; then
  echo "[warn] Nenhum arquivo .trc encontrado em $exp_dir (nem em slave-*). Nada para analisar."
  exit 0
fi

dbpath="$exp_dir/$dbfile"
if $force; then
  rm -f "$dbpath"
fi

echo "[analyze] EXP_DIR=$exp_dir"
echo "[analyze] DB=$dbpath"
echo "[analyze] traces=${#trc_files[@]}"

# 1) Carrega traces no sqlite
echo "[analyze] loading traces -> sqlite"
python3 scripts/analyze/load-logs.py "$dbpath" "${trc_files[@]}"

# 2) Executa queries e exporta *.val em exp_dir
echo "[analyze] running queries -> exporting *.val"
for q in "${queries[@]}"; do
  if [[ ! -f "$q" ]]; then
    echo "[warn] query não encontrada: $q (pulando)" >&2
    continue
  fi
  python3 scripts/analyze/run-queries.py "$dbpath" "$q" "$exp_dir" >/dev/null
done

val_count=$(ls -1 "$exp_dir"/*.val 2>/dev/null | wc -l | tr -d ' ')
echo "[analyze] done: ${val_count} *.val generated in $exp_dir"

