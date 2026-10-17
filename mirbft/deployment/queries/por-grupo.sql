-- Throughput/batch-size por grupo de dados, via seqNr % numGroups (idx denso,
-- conferido em orderer/multipaxosgroup.go:GetDataGroupIndex). groupId = idx + 1 assume
-- groups.yml sem buraco na numeracao.
--
-- GERADO por scripts/analyze/gen-por-grupo.py -- nao editar a mao, regenerar ao mudar o
-- numero de grupos de dados do groups.yml:
--   python3 scripts/analyze/gen-por-grupo.py --num-groups <N> --out queries/por-grupo.sql
--
-- Um bloco por grupo/metrica (nao GROUP BY) porque summarize.sh exige um escalar por .val.
--
-- CREATE TABLE protocol_truncated proprio (copiado de aggregates.sql): cada arquivo passado
-- em "-q" ao run-queries.py roda numa transacao PROPRIA, com rollback no final -- a tabela
-- que aggregates.sql cria nao sobrevive pra esta execucao separada (descoberto rodando de
-- verdade: "no such table: protocol_truncated"). Por isso este arquivo precisa criar a dele
-- mesmo, nao pode depender de rodar sempre junto com aggregates.sql na mesma invocacao.
CREATE TABLE protocol_truncated as
SELECT *
FROM protocol
WHERE
  ts - 5000000 >= (SELECT max(t)
    FROM (SELECT min(ts) as t
      FROM request
      WHERE event = 'REQ_FINISHED'
      GROUP BY nodeId))
  AND ts + 5000000 <= (SELECT min(t)
    FROM (SELECT max(ts) as t
      FROM request
      WHERE event = 'REQ_SEND'
      GROUP BY nodeId))

-- export commit-rate-trunc-g1.val
SELECT count(*) / ((max(ts) - min(ts))/1000000.0)
FROM protocol_truncated
WHERE event = 'COMMIT' AND seqNr % 4 = 0
-- (rate[batches/sec])

-- export commit-rate-trunc-g2.val
SELECT count(*) / ((max(ts) - min(ts))/1000000.0)
FROM protocol_truncated
WHERE event = 'COMMIT' AND seqNr % 4 = 1
-- (rate[batches/sec])

-- export commit-rate-trunc-g3.val
SELECT count(*) / ((max(ts) - min(ts))/1000000.0)
FROM protocol_truncated
WHERE event = 'COMMIT' AND seqNr % 4 = 2
-- (rate[batches/sec])

-- export commit-rate-trunc-g4.val
SELECT count(*) / ((max(ts) - min(ts))/1000000.0)
FROM protocol_truncated
WHERE event = 'COMMIT' AND seqNr % 4 = 3
-- (rate[batches/sec])

-- export batch-size-avg-trunc-g1.val
SELECT avg(val)
FROM protocol_truncated
WHERE event = 'PROPOSE' AND seqNr % 4 = 0
-- (avg-batch-size[req])

-- export batch-size-avg-trunc-g2.val
SELECT avg(val)
FROM protocol_truncated
WHERE event = 'PROPOSE' AND seqNr % 4 = 1
-- (avg-batch-size[req])

-- export batch-size-avg-trunc-g3.val
SELECT avg(val)
FROM protocol_truncated
WHERE event = 'PROPOSE' AND seqNr % 4 = 2
-- (avg-batch-size[req])

-- export batch-size-avg-trunc-g4.val
SELECT avg(val)
FROM protocol_truncated
WHERE event = 'PROPOSE' AND seqNr % 4 = 3
-- (avg-batch-size[req])

-- export ncommit-trunc-g1.val
SELECT count(*)
FROM protocol_truncated
WHERE event = 'COMMIT' AND seqNr % 4 = 0
-- (count[batches])

-- export ncommit-trunc-g2.val
SELECT count(*)
FROM protocol_truncated
WHERE event = 'COMMIT' AND seqNr % 4 = 1
-- (count[batches])

-- export ncommit-trunc-g3.val
SELECT count(*)
FROM protocol_truncated
WHERE event = 'COMMIT' AND seqNr % 4 = 2
-- (count[batches])

-- export ncommit-trunc-g4.val
SELECT count(*)
FROM protocol_truncated
WHERE event = 'COMMIT' AND seqNr % 4 = 3
-- (count[batches])

-- request.clSn nao carrega grupo/SN -- latencia por grupo so e direta em rodadas
-- pinadas (overload <grupo> ... --api), onde a rodada inteira ja e de um grupo so.
