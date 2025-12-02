#!/bin/bash
#
# If ran on MacOS, the gdate (Linux-style date) command is preferred.
# Prerequisite is coreutils which has gdate and can be installed via homebrew:
#
#   brew install coreutils
#
# But this script should work also without gdate installed, in which case, on
# MacOS, it will use the default date command.
#

set -euo pipefail

function usage {
  cat 1>&2 <<EOF
Usage: $0 [--db NAME] [--no-prefix] [--skip-pre] DIR [DIR...]

Processes ISS trace logs in the given directory(ies) and generates CSV summaries.
EOF
}

# Defaults
skip_pre=false
dbfile="trace.db"
no_prefix=false

# Parse options
while [[ $# -gt 0 ]]; do
  case "$1" in
    --skip-pre)
      skip_pre=true
      shift
      ;;
    --db)
      dbfile="$2"
      shift 2
      ;;
    --no-prefix)
      no_prefix=true
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    -*)
      echo "Unknown option: $1" >&2
      usage
      exit 1
      ;;
    *)
      break
      ;;
  esac
done

if [[ $# -lt 1 ]]; then
  usage
  exit 1
fi

# We expect to be run from mirbft/deployment.
if [[ ! -d scripts/analyze ]]; then
  echo "This script must be run from mirbft/deployment." >&2
  exit 1
fi

# Main loop over experiment directories
while [[ $# -gt 0 ]]; do
  dir="$1"

  if [[ ! -d "$dir" ]]; then
    echo "[warn] experiment directory not found: $dir"
    shift
    continue
  fi

  echo "Analyzing: $dir"

  # If requested, we might want to skip some pre-processing steps (not shown here).
  skipping=false

  # 1) Load logs into SQLite DB
  if [[ "$skipping" == false ]]; then
    echo "  > Loading trace into database..."

    # Using gdate when available (Linux-style date) and falling back to date otherwise.
    # This also works on Mac when coreutils is installed.
    startTimeNs=$(gdate +%s%N 2>/dev/null || date +%s%N) # This is due to a different date command on Mac.

    # ----------------------------------------------------------------------
    # NOVO BLOCO: detecção explícita dos arquivos .trc com logs detalhados
    # ----------------------------------------------------------------------

    # Expand the glob in a safe way; if there is no match we will get an empty array
    # instead de literalmente "slave-*/*.trc", evitando FileNotFoundError no Python.
    shopt -s nullglob
    trc_files=("$dir"/slave-*/*.trc)
    shopt -u nullglob

    if [ ${#trc_files[@]} -eq 0 ]; then
      echo "  [warn] Nenhum arquivo .trc encontrado em $dir/slave-*/."
      echo "         Sem traces não é possível calcular as métricas de throughput/latência."
      echo "         Verifique:"
      echo "           - se o orderingpeer está sendo iniciado com os argumentos de tracing;"
      echo "           - se ele recebeu SIGINT (veja se há logs de 'Started tracing.' em peer.log);"
      echo "           - se o diretório 'experiment-output/.../slave-*/' bate com o master-commands.cmd."
      echo "         (o script seguirá, mas as métricas dependentes das traces ficarão vazias)."
      skipping=true
    else
      echo "  > Encontrados ${#trc_files[@]} arquivos .trc em $dir/slave-*/:"
      for f in "${trc_files[@]}"; do
        echo "      - $f"
      done

      # Chamada original, agora com a lista expandida de arquivos .trc.
      # IMPORTANTE: não colocar aspas ao redor de ${trc_files[@]} no Python,
      # para que cada arquivo vire um argumento separado, como esperado.
      python3 scripts/analyze/load-logs.py "$dir/$dbfile" "${trc_files[@]}"

      endTimeNs=$(gdate +%s%N 2>/dev/null || date +%s%N) # This is due to a different data command on Mac.
      echo "  > Loaded trace into database in $(((endTimeNs - startTimeNs) / 1000000000)) s."
      skipping=false
    fi
  fi

  # 2) Generate CSV summary if we actually loaded something
  if [[ "$skipping" == false ]]; then
    echo "  > Generating CSV summary..."

    # A função do summarize.py continua a mesma; ele vai olhar para o DB gerado.
    python3 scripts/analyze/summarize.py "$dir/$dbfile" > "$dir/summary.csv"

    echo "  > Summary written to $dir/summary.csv"
  fi

  # Opcional: se quiser, podemos extrair campos específicos para um CSV "global" aqui.
  # (mantive o comportamento original de apenas gerar o summary por diretório.)

  shift
done

