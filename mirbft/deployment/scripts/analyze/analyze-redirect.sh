#!/usr/bin/env bash
set -euo pipefail

# Analisa cada rodada do sweep de redirecionamento isoladamente. Os peers ficam de pe a
# campanha inteira (um .trc continuo por peer, todas as rodadas do sweep misturadas) --
# analyze.sh direto no exp_dir inteiro calcularia a janela de truncagem (aggregates.sql,
# baseada nos timestamps da tabela request) sobre as 35 rodadas juntas, o que nao faz
# sentido. Em vez disso, pra cada .trc de cliente (uma rodada = um outPrefix unico de
# run-redirect-sweep.sh), monta um diretorio isolado com symlinks (so aquele .trc + os .trc
# continuos dos peers) e roda analyze.sh nele -- a janela de truncagem fica correta,
# calculada so a partir dos timestamps REQ_SEND/REQ_FINISHED daquela rodada.

usage() {
  echo "Uso: $0 <exp_dir (com experiment-output/ ja buscado por fetch-results.sh)> [out_sub=redirect]" >&2
  exit 1
}

[ "$#" -ge 1 ] || usage
exp_root="$1"
out_sub="${2:-redirect}"

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
deploy_dir="$(cd "$script_dir/../.." && pwd)"

exp_output_dir="$(find "$exp_root" -maxdepth 2 -type d -name "experiment-output" | head -1)"
[ -n "$exp_output_dir" ] || { echo "nao achei experiment-output/ em $exp_root" >&2; exit 1; }

redirect_dir="$(find "$exp_output_dir" -type d -name "$out_sub" | head -1)"
[ -n "$redirect_dir" ] || { echo "nao achei pasta '$out_sub' em $exp_output_dir" >&2; exit 1; }

mapfile -t peer_trc_files < <(find "$exp_output_dir" -path "*/slave-*/peer.trc" -not -path "*/$out_sub/*")
[ "${#peer_trc_files[@]}" -gt 0 ] || { echo "nao achei nenhum peer.trc em $exp_output_dir" >&2; exit 1; }
echo "[analyze-redirect] ${#peer_trc_files[@]} peer.trc encontrados"

metrics="commit-rate-trunc-g1 commit-rate-trunc-g2 commit-rate-trunc-g3 commit-rate-trunc-g4 ncommit-trunc-g1 ncommit-trunc-g2 ncommit-trunc-g3 ncommit-trunc-g4 throughput-trunc latency-avg-trunc nreq-trunc"

result_csv="$exp_root/redirect-result.csv"
echo "point,rep,$(echo $metrics | tr ' ' ',')" > "$result_csv"

cd "$deploy_dir"
for client_trc in "$redirect_dir"/*.trc; do
  [ -e "$client_trc" ] || continue
  base="$(basename "$client_trc" .trc)"               # ex.: g1-rep3-000
  point_rep="$(echo "$base" | sed -E 's/-[0-9]+$//')"  # g1-rep3
  point="$(echo "$point_rep" | sed -E 's/-rep[0-9]+$//')"
  rep="$(echo "$point_rep" | sed -E 's/^.*-rep//')"

  tmp_dir="$(mktemp -d)"
  mkdir -p "$tmp_dir/slave-client"
  ln -s "$(realpath "$client_trc")" "$tmp_dir/slave-client/$base.trc"
  i=0
  for p in "${peer_trc_files[@]}"; do
    mkdir -p "$tmp_dir/slave-peer$i"
    ln -s "$(realpath "$p")" "$tmp_dir/slave-peer$i/peer.trc"
    i=$((i + 1))
  done

  echo "[analyze-redirect] $point rep=$rep"
  scripts/analyze/analyze.sh -d "$tmp_dir" -f -q queries/aggregates.sql -q queries/por-grupo.sql >/dev/null

  row="$point,$rep"
  for metric in $metrics; do
    val_file="$tmp_dir/$metric.val"
    val="$([ -f "$val_file" ] && tr -d '\r\n' < "$val_file" || true)"
    row="$row,$val"
  done
  echo "$row" >> "$result_csv"

  rm -rf "$tmp_dir"
done

echo "[analyze-redirect] OK: $result_csv"
