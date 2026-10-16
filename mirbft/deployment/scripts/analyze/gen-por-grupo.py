#!/usr/bin/env python3
"""Gera queries/por-grupo.sql pro número de grupos de dados do groups.yml em uso.

Uso:
  python3 gen-por-grupo.py --num-groups 4 --out ../../deployment/queries/por-grupo.sql
  (ou sem --out: imprime no stdout)
"""
import argparse
import sys

HEADER = """-- Throughput/batch-size por grupo de dados, via seqNr % numGroups (idx denso,
-- conferido em orderer/multipaxosgroup.go:GetDataGroupIndex). groupId = idx + 1 assume
-- groups.yml sem buraco na numeracao.
--
-- GERADO por scripts/analyze/gen-por-grupo.py -- nao editar a mao, regenerar ao mudar o
-- numero de grupos de dados do groups.yml:
--   python3 scripts/analyze/gen-por-grupo.py --num-groups <N> --out queries/por-grupo.sql
--
-- Um bloco por grupo/metrica (nao GROUP BY) porque summarize.sh exige um escalar por .val.
"""

FOOTER = """-- request.clSn nao carrega grupo/SN -- latencia por grupo so e direta em rodadas
-- pinadas (overload <grupo> ... --api), onde a rodada inteira ja e de um grupo so.
"""


def commit_rate_block(idx: int, num_groups: int) -> str:
    group_id = idx + 1
    return (
        f"-- export commit-rate-trunc-g{group_id}.val\n"
        f"SELECT count(*) / ((max(ts) - min(ts))/1000000.0)\n"
        f"FROM protocol_truncated\n"
        f"WHERE event = 'COMMIT' AND seqNr % {num_groups} = {idx}\n"
        f"-- (rate[batches/sec])\n"
    )


def batch_size_block(idx: int, num_groups: int) -> str:
    group_id = idx + 1
    return (
        f"-- export batch-size-avg-trunc-g{group_id}.val\n"
        f"SELECT avg(val)\n"
        f"FROM protocol_truncated\n"
        f"WHERE event = 'PROPOSE' AND seqNr % {num_groups} = {idx}\n"
        f"-- (avg-batch-size[req])\n"
    )


def ncommit_block(idx: int, num_groups: int) -> str:
    group_id = idx + 1
    return (
        f"-- export ncommit-trunc-g{group_id}.val\n"
        f"SELECT count(*)\n"
        f"FROM protocol_truncated\n"
        f"WHERE event = 'COMMIT' AND seqNr % {num_groups} = {idx}\n"
        f"-- (count[batches])\n"
    )


def generate(num_groups: int) -> str:
    parts = [HEADER]
    for idx in range(num_groups):
        parts.append(commit_rate_block(idx, num_groups))
    for idx in range(num_groups):
        parts.append(batch_size_block(idx, num_groups))
    for idx in range(num_groups):
        parts.append(ncommit_block(idx, num_groups))
    parts.append(FOOTER)
    return "\n".join(parts)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--num-groups", type=int, required=True,
                         help="Numero de grupos de dados (SEM o grupo 0/GSN), conforme groups.yml")
    parser.add_argument("--out", type=str, default=None,
                         help="Arquivo de saida (default: stdout)")
    args = parser.parse_args()

    if args.num_groups < 1:
        print("--num-groups precisa ser >= 1", file=sys.stderr)
        sys.exit(1)

    sql = generate(args.num_groups)
    if args.out:
        with open(args.out, "w") as f:
            f.write(sql)
        print(f"Escrito {args.out} para {args.num_groups} grupos de dados.", file=sys.stderr)
    else:
        print(sql)


if __name__ == "__main__":
    main()
