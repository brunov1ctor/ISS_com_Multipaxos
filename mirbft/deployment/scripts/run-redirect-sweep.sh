#!/usr/bin/env bash
set -euo pipefail

# Dispara o sweep de redirecionamento de carga (baseline "all" + cada grupo pinado) via SSH
# no no "1client" -- roda de onde o deploy.sh foi feito (ex. node-0), DEPOIS que o cluster ja
# esta de pe (waitForManualCommands ativo). Cada rodada grava seu .trc com outPrefix unico
# dentro de remote_experiment_output_dir no proprio cliente -- fetch-results.sh ja puxa essa
# pasta de volta no final do deploy (qualquer slave com tag != "peers"), sem nada extra aqui.
#
# Os peers ficam de pe a campanha toda (um .trc continuo por peer, todas as rodadas
# misturadas) -- por isso NAO use scripts/analyze/analyze.sh direto no exp_dir inteiro depois.
# Use scripts/analyze/analyze-redirect.sh, que isola cada rodada antes de analisar.

usage() {
  cat >&2 <<EOF
Uso: $0 <instance-info> [opcoes]
  --groups "1 2 3 4"    grupos pinados a testar (default: "1 2 3 4"; "all" sempre entra)
  --reps N              repeticoes por ponto (default: 7)
  --duration SEGUNDOS   duracao de cada rodada (default: 30)
  --api-size N          tamanho do conjunto semeado (default: 10000)
  --write PCT           --write do overload (default: 0)
  --delete PCT          --delete do overload (default: 0)
  --remote-user USER    (default: \$USER)
  --out-sub DIR         subpasta dentro de experiment-output no cliente (default: redirect)
EOF
  exit 1
}

[ "$#" -ge 1 ] || usage
instance_info_file="$1"; shift

groups="1 2 3 4"
reps=7
duration=30
api_size=10000
write_pct=0
delete_pct=0
remote_user="${USER:-user}"
out_sub="redirect"

while [ "$#" -gt 0 ]; do
  case "$1" in
    --groups) groups="$2"; shift 2 ;;
    --reps) reps="$2"; shift 2 ;;
    --duration) duration="$2"; shift 2 ;;
    --api-size) api_size="$2"; shift 2 ;;
    --write) write_pct="$2"; shift 2 ;;
    --delete) delete_pct="$2"; shift 2 ;;
    --remote-user) remote_user="$2"; shift 2 ;;
    --out-sub) out_sub="$2"; shift 2 ;;
    *) usage ;;
  esac
done

[ -f "$instance_info_file" ] || { echo "instance-info nao encontrado: $instance_info_file" >&2; exit 1; }

client_ip="$(awk '$4=="slave" && $5!="peers"{print $2; exit}' "$instance_info_file")"
[ -n "$client_ip" ] || { echo "nao achei no 1client (tag != peers) em $instance_info_file" >&2; exit 1; }

remote_work_dir="/tmp/iss-${remote_user}"
remote_base_dir="/users/${remote_user}/iss"
remote_bin_dir="${remote_work_dir}/bin"
remote_out_dir="${remote_work_dir}/experiment-output/${out_sub}"
# ServerAlive* detecta conexao morta/travada (ex. rede caiu no meio do dreno) e fecha em vez
# de ficar pendurado pra sempre -- sem isso, uma rodada travada nunca retorna e o sweep nao
# avanca nem com o timeout abaixo (o timeout mata o processo local do ssh, mas so o
# keepalive detecta a conexao morta de verdade).
ssh_opts="-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=10 -o ServerAliveInterval=10 -o ServerAliveCountMax=3"
# Teto por rodada: duracao pedida + folga generosa (semeadura + dreno + handshake). Uma
# rodada que passar disso e considerada travada -- morta e registrada como falha, sem
# abortar o sweep inteiro (ver run_point abaixo, que nunca propaga erro pra cima).
point_timeout=$((duration + 180))

echo "[sweep] cliente: ${remote_user}@${client_ip}"
echo "[sweep] destino dos .trc: ${remote_out_dir}"
ssh $ssh_opts "${remote_user}@${client_ip}" "mkdir -p '${remote_out_dir}'"

failures=0

run_point() {
  local target="$1" rep="$2"
  local prefix="${remote_out_dir}/${target}-rep${rep}"
  echo "[sweep] target=${target} rep=${rep}/${reps}"
  # cd pro BASE_DIR: tls-data/config.yml sao relativos a ele (mesma convencao do exec-start
  # dos peers). PATH precisa apontar pro bin do deploy -- SSH nao-interativo nao carrega
  # .bashrc, "orderingclient" nao resolve sem isso.
  if ! timeout "$point_timeout" ssh $ssh_opts "${remote_user}@${client_ip}" \
    "cd '${remote_base_dir}' && PATH='${remote_bin_dir}:/usr/bin:/bin' orderingclient overload ${target} ${duration} --api ${api_size} --write ${write_pct} --delete ${delete_pct} '${prefix}'"
  then
    echo "[sweep] FALHOU (timeout ${point_timeout}s ou erro): target=${target} rep=${rep}/${reps}"
    failures=$((failures + 1))
  fi
}

for rep in $(seq 1 "$reps"); do
  run_point all "$rep"
done
for g in $groups; do
  for rep in $(seq 1 "$reps"); do
    run_point "$g" "$rep"
  done
done

n_groups=$(echo $groups | wc -w)
total=$((reps * (1 + n_groups)))
echo "[sweep] feito: ${total} rodadas tentadas (${failures} falharam) em ${client_ip}:${remote_out_dir}"
