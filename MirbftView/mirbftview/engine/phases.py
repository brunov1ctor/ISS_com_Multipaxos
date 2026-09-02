"""Máquina de estados — executa cada fase do protocolo gerando mensagens."""

import hashlib
import random
import string

from mirbftview.protocol.types import (
    Phase, MsgType, Message, RequestInfo,
)
from mirbftview.protocol.batch import PendingRequest
from mirbftview.protocol.delivery import DeliveryEntry
from mirbftview.engine.state import SimState


def phase_client_send(st: SimState):
    """Gera nova request e envia ao proxy."""
    client = random.choice(st.clients)
    st.client_sn += 1

    payload = ''.join(random.choices(string.ascii_uppercase + string.digits, k=12))
    payload_hash = hashlib.sha256(payload.encode()).hexdigest()[:16]

    is_cross = st.scenarios.get('cross_group', False) or random.random() < st.cross_op_pct
    if is_cross:
        touched = [1, 2]
        st.gsn += 1
        gsn = st.gsn
    else:
        touched = [random.choice([g.id for g in st.groups if g.id != 0])]
        gsn = 0

    # Grupo primario (onde a request sera proposta via Paxos)
    group_id = touched[0]
    group = st.groups[group_id] if group_id < len(st.groups) else st.groups[1]
    quorum = len(group.members) // 2 + 1

    # Lider da PROXIMA posicao do log deste grupo (multipaxosinstance.go:606:
    # i.leader = i.members[i.sn % n] — fixo pelo SN, nao muda com epoch/policy).
    leader = st.epoch_mgr.leader_for_group(group_id)

    # Bucket assignment (interno, para visualizacao do painel de buckets)
    bucket_id = st.epoch_mgr.get_request_bucket(client.id, st.client_sn)

    # Proxy = qualquer no que recebe do cliente (no real, e o no gRPC)
    proxy_node = random.choice(group.members)

    st.current_request = RequestInfo(
        client_id=client.id,
        client_sn=st.client_sn,
        payload=payload,
        payload_hash=payload_hash,
        bucket_id=bucket_id,
        group_id=group_id,
        gsn=gsn,
        sn=0,  # será atribuído quando o batch for proposto (phase_batch_cut)
        leader=leader,
        ballot=0,
        quorum=quorum,
        is_cross_group=is_cross,
        touched_groups=touched,
        proxy_node=proxy_node,
    )

    # Registra proxy
    st.proxy.register_request(client.id, st.client_sn, proxy_node)

    st.phase = Phase.CLIENT_SEND
    st.log_event(Phase.CLIENT_SEND, "Cliente envia request",
                 f"{client.name} -> Node {proxy_node} (proxy)\n"
                 f"Payload: \"{payload}\" | Hash: {payload_hash[:8]}\n"
                 f"Grupo: G{group_id} membros={group.members}\n"
                 f"Lider do grupo: Node {leader}\n"
                 f"Cross-group: {'Sim -> GSN=' + str(gsn) if is_cross else 'Nao'}",
                 "orange")

    st.info_text = (
        f"🔶 Um cliente enviou um pedido!\n\n"
        f"Imagine que {client.name} é um usuário enviando\n"
        f"uma transação para o sistema.\n\n"
        f"O pedido vai para o Node {proxy_node} (o 'carteiro'\n"
        f"que recebe e encaminha ao responsável).\n\n"
        f"{'⚡ Este pedido envolve MÚLTIPLOS grupos — precisa de coordenação extra.' if is_cross else '→ Pedido simples, envolve apenas 1 grupo.'}\n\n"
        f"─── Detalhes técnicos ───\n"
        f"Payload: \"{payload}\" | Hash: {payload_hash[:8]}\n"
        f"Grupo G{group_id} membros={group.members} | Líder: Node {leader}"
    )

    st.messages = [Message(
        MsgType.CLIENT_REQUEST, client.id, proxy_node,
        from_is_client=True,
        label=f"REQ h={payload_hash[:8]}",
        detail=f"Payload=\"{payload}\"",
        sn=0,
        batch_digest="",
    )]


def phase_bucket_assign(st: SimState):
    """Request chega ao proxy → atribuída ao bucket → encaminhada a TODOS os membros."""
    req = st.current_request
    bucket_id = req.bucket_id
    leader = req.leader

    # Adiciona ao bucket visual
    label = f"sn={req.client_sn} h={req.payload_hash[:8]}"
    if bucket_id < len(st.bucket_contents):
        st.bucket_contents[bucket_id].append(label)

    # Incrementa batch fill counter
    st.batch_fill[bucket_id] = st.batch_fill.get(bucket_id, 0) + 1

    # Emite evento visual: request entrou no bucket
    st.visual_events.append({"type": "bucket_in", "bucket": bucket_id, "leader": leader, "ttl": 18})

    # Adiciona ao BatchCutter
    pending = PendingRequest(
        client_id=req.client_id,
        client_sn=req.client_sn,
        payload_hash=req.payload_hash,
        is_cross_group=req.is_cross_group,
        gsn=req.gsn,
        touched_groups=req.touched_groups,
    )
    cutter = st.batch_cutters.get(bucket_id)
    if cutter:
        cutter.add_request(pending)

    group = st.groups[req.group_id] if req.group_id < len(st.groups) else st.groups[1]
    next_sn = st.epoch_mgr.next_sn_for(req.group_id)

    st.phase = Phase.BUCKET_ASSIGN
    st.log_event(Phase.BUCKET_ASSIGN, "Bucket Assignment",
                 f"Request -> Bucket {bucket_id}\n"
                 f"  Formula request->bucket: (clID={req.client_id} + clSN={req.client_sn}) % {st.num_buckets}\n"
                 f"  Bucket pertence sempre ao grupo G{req.group_id} (fixo, de groups.yml)\n"
                 f"  Lider da proxima posicao (SN={next_sn}) do log de G{req.group_id}: Node {leader}\n"
                 f"  (members[sn % n] — roda por SN, nao muda com epoca)",
                 "gold")

    st.info_text = (
        f"🪣 Classificando o pedido numa 'caixa de entrada'\n\n"
        f"O sistema tem {st.num_buckets} caixas (buckets) POR GRUPO.\n"
        f"Um bucket pertence sempre ao mesmo grupo: a caixa em si\n"
        f"nunca muda de dono, só serve para agrupar pedidos antes\n"
        f"de cortar o batch.\n\n"
        f"📡 O proxy envia a request para TODOS os membros\n"
        f"do grupo G{req.group_id}: {group.members}\n"
        f"(via sendToGroup/GsnReqForward — garante que todos\n"
        f"tenham a request no bucket local antes do ACCEPT)\n\n"
        f"─── Como a fila é escolhida? ───\n"
        f"Fórmula: (cliente + nº do pedido) mod {st.num_buckets}\n"
        f"= ({req.client_id} + {req.client_sn}) mod {st.num_buckets} = fila {bucket_id}\n\n"
        f"─── Quem lidera a próxima posição do log deste grupo? ───\n"
        f"Node {leader} (SN={next_sn} mod {len(group.members)} membros = "
        f"posição {next_sn % len(group.members)} da lista {group.members})"
    )

    # Mensagem visual: proxy encaminha para TODOS os membros do grupo
    # (conforme sendToGroup em multipaxosmulticastorderer.go)
    # Cada membro faz AddDirectToBucket localmente.
    st.messages = []
    src = req.proxy_node
    for nid in group.members:
        if nid == src:
            continue
        st.messages.append(Message(
            MsgType.CLIENT_REQUEST, src, nid,
            label=f"FWD req h={req.payload_hash[:6]}",
            detail=f"GsnReqForward group=G{req.group_id}",
        ))
    if not st.messages:
        st.messages = [Message(
            MsgType.CLIENT_REQUEST, leader, leader,
            label=f"AddDirectToBucket B{bucket_id}",
            detail=f"local insert",
        )]


def phase_batch_cut(st: SimState):
    """Líder corta o batch (CutBatch) — acumula até batch_visual_size."""
    req = st.current_request
    cutter = st.batch_cutters.get(req.bucket_id)
    fill = st.batch_fill.get(req.bucket_id, 1)

    batch_digest = hashlib.sha256(
        f"{req.payload_hash}:{req.bucket_id}:{req.group_id}".encode()
    ).hexdigest()[:12]
    req.batch_digest = batch_digest
    req.batch_requests = fill  # batch real: todas as requests acumuladas

    # Remove todas as requests do bucket visual
    if req.bucket_id < len(st.bucket_contents):
        bucket = st.bucket_contents[req.bucket_id]
        for _ in range(min(fill, len(bucket))):
            bucket.pop(0)

    # Reset batch fill (batch cortado)
    st.batch_fill[req.bucket_id] = 0

    # Emite evento visual: batch cortado + thought bubble
    st.visual_events.append({"type": "batch_cut", "bucket": req.bucket_id, "ttl": 20})
    cut_reason_label = "cross_op" if req.is_cross_group else "size/timeout"
    st.bucket_bubbles[req.bucket_id] = {
        "text": f"CutBatch()\nreason={cut_reason_label}\nreqs={fill} digest={batch_digest[:6]}",
        "color": "green", "ttl": 30,
    }

    # Remove do cutter
    if cutter:
        cutter.force_cut()

    # Consome a proxima posicao do log deste grupo — e so aqui, no corte real
    # do batch, que o SN e de fato usado (leader_for_group ja tinha "espiado"
    # este mesmo SN antes, em phase_client_send, sem consumi-lo).
    req.sn = st.epoch_mgr.next_sn_for(req.group_id)
    st.epoch_mgr.advance(req.group_id)

    cut_reason = "cross_op_immediate" if req.is_cross_group else "size/timeout"

    st.phase = Phase.BATCH_CUT
    st.log_event(Phase.BATCH_CUT, "CutBatch",
                 f"Lider Node {req.leader} corta batch do Bucket {req.bucket_id}\n"
                 f"Motivo: {cut_reason}\n"
                 f"Requests no batch: {req.batch_requests}\n"
                 f"Batch digest: {batch_digest}\n"
                 f"SN atribuido: {req.sn} (log do grupo G{req.group_id})",
                 "gold")

    st.info_text = (
        f"✂️ Empacotando pedidos para votação\n\n"
        f"O líder (Node {req.leader}) junta pedidos num 'pacote'\n"
        f"(batch) antes de pedir aprovação ao grupo.\n\n"
        f"{'⚡ Como envolve múltiplos grupos, empacotou IMEDIATAMENTE.' if req.is_cross_group else '📦 Empacotou quando acumulou pedidos suficientes (ou deu timeout).'}\n\n"
        f"Agora o pacote recebe um número de ordem (SN={req.sn})\n"
        f"e vai para votação no grupo.\n\n"
        f"─── Detalhes técnicos ───\n"
        f"Batch: {req.batch_requests} reqs | Digest: {batch_digest}\n"
        f"Grupo G{req.group_id} | Log contíguo (sem segmentos intercalados)"
    )

    st.messages = [Message(
        MsgType.CLIENT_REQUEST, req.leader, req.leader,
        label=f"CutBatch d={batch_digest[:6]}",
        detail=f"size={req.batch_requests} reason={cut_reason}",
    )]


def phase_gsn_assign(st: SimState):
    """Sequenciador atribui GSN para cross-group ops."""
    req = st.current_request
    seq_leader = min(st.groups[0].members) if st.groups[0].members else 0

    # Registra META stream publication
    st.meta_stream.append({
        "gsn": req.gsn,
        "groups": list(req.touched_groups),
        "published_by": seq_leader,
    })
    if len(st.meta_stream) > 20:
        st.meta_stream = st.meta_stream[-20:]

    # Anuncia a existencia do GSN a cada grupo tocado JA AQUI, antes do
    # commit em si — espelha RegisterMetadata/groupGSNQueue reais, que
    # populam a fila de um grupo assim que o META chega, independente de
    # quando aquele grupo especifico termina seu proprio consenso. E isso
    # que faz o ADeliver poder de fato bloquear em ordem, sem depender de
    # que os GSNs sejam inteiros consecutivos por grupo.
    for gid in req.touched_groups:
        st.delivery.register_touch(req.gsn, gid)

    st.phase = Phase.GSN_ASSIGN
    st.log_event(Phase.GSN_ASSIGN, "GSN Atribuido",
                 f"GSN={req.gsn} | Sequenciador: Node {seq_leader}\n"
                 f"Grupos tocados: {req.touched_groups}\n"
                 f"META_STREAM broadcast -> garante ordem global",
                 "purple")

    st.info_text = (
        f"🔢 Numeração global para pedidos multi-grupo\n\n"
        f"Este pedido afeta mais de um grupo!\n"
        f"Para evitar conflitos, um coordenador (Node {seq_leader})\n"
        f"atribui um número de ordem GLOBAL: GSN={req.gsn}\n\n"
        f"É como numerar senhas num hospital que tem\n"
        f"vários guichês — garante que todos atendam\n"
        f"na mesma ordem.\n\n"
        f"Grupos envolvidos: {req.touched_groups}"
    )

    st.messages = []
    for nid in st.groups[0].members:
        if nid != seq_leader:
            st.messages.append(Message(
                MsgType.META_STREAM, seq_leader, nid,
                label=f"META gsn={req.gsn}",
                detail=f"groups={req.touched_groups}",
            ))


def phase_prepare(st: SimState):
    """PREPARE — Fase 1a do MultiPaxos."""
    req = st.current_request
    group = st.groups[req.group_id] if req.group_id < len(st.groups) else st.groups[1]

    st.phase = Phase.PREPARE
    st.log_event(Phase.PREPARE, "PREPARE (Fase 1a)",
                 f"Lider Node {req.leader} | SN={req.sn} | Ballot={req.ballot}\n"
                 f"Grupo G{req.group_id} membros={group.members}\n"
                 f"Quorum: {req.quorum}/{len(group.members)}",
                 "phase_prepare")

    st.info_text = (
        f"📋 PREPARE (Fase 1a) — O líder pede permissão\n\n"
        f"Node {req.leader} envia MPxPrepare ao grupo:\n"
        f"\"Aceitem-me como coordenador para SN={req.sn}\n"
        f"com ballot={req.ballot}\"\n\n"
        f"Precisa que a MAIORIA concorde ({req.quorum} de\n"
        f"{len(group.members)} membros) — isso é o 'quorum'.\n\n"
        f"⚡ No MultiPaxos steady-state, esta fase só\n"
        f"acontece UMA VEZ (em SetMembers). Depois, o líder\n"
        f"pula direto para ACCEPT (ProposeIfDue).\n"
        f"Só repete PREPARE se houver timeout (novo ballot).\n\n"
        f"─── Detalhes (MPxPrepare) ───\n"
        f"Ballot={req.ballot} | GroupId={req.group_id} | Membros: {group.members}"
    )

    st.messages = [Message(
        MsgType.PREPARE, req.leader, nid,
        label=f"PREPARE b={req.ballot} g={req.group_id}",
        detail=f"sn={req.sn} ballot={req.ballot} groupId={req.group_id}",
        sn=req.sn,
        ballot=req.ballot,
        batch_digest=req.batch_digest,
    ) for nid in group.members if nid != req.leader]


def phase_promise(st: SimState):
    """PROMISE — Fase 1b."""
    req = st.current_request
    group = st.groups[req.group_id] if req.group_id < len(st.groups) else st.groups[1]
    req.promises_received = len(group.members)

    st.phase = Phase.PROMISE
    st.log_event(Phase.PROMISE, "PROMISE (Fase 1b)",
                 f"Promises: {req.promises_received}/{req.quorum} QUORUM\n"
                 f"Ballot={req.ballot} aceito",
                 "phase_promise")

    st.info_text = (
        f"🤝 PROMISE (Fase 1b) — O grupo concorda\n\n"
        f"Os membros responderam com MPxPromise:\n"
        f"\"Prometemos não aceitar ballot < {req.ballot}.\"\n\n"
        f"Respostas: {req.promises_received} de {req.quorum} necessárias ✓\n\n"
        f"Quando quorum atingido, líder marca prepared=true\n"
        f"e chama ProposeIfDue() → envia ACCEPT.\n\n"
        f"─── Detalhes (MPxPromise) ───\n"
        f"Ballot={req.ballot} | Ok=true | GroupId={req.group_id}\n"
        f"Members={group.members}"
    )

    st.messages = [Message(
        MsgType.PROMISE, nid, req.leader,
        label=f"PROMISE b={req.ballot} ok",
        detail=f"ballot={req.ballot} groupId={req.group_id}",
        sn=req.sn,
        ballot=req.ballot,
    ) for nid in group.members if nid != req.leader]


def phase_accept(st: SimState):
    """ACCEPT — Fase 2a (proposta do batch)."""
    req = st.current_request
    group = st.groups[req.group_id] if req.group_id < len(st.groups) else st.groups[1]

    st.phase = Phase.ACCEPT
    st.log_event(Phase.ACCEPT, "ACCEPT (Fase 2a)",
                 f"Lider propoe batch digest={req.batch_digest}\n"
                 f"SN={req.sn} | Ballot={req.ballot} | G{req.group_id}",
                 "phase_accept")

    st.info_text = (
        f"📦 ACCEPT (Fase 2a) — O líder propõe o batch\n\n"
        f"Node {req.leader} envia MPxAccept com o batch:\n"
        f"\"Registrem o pacote '{req.batch_digest[:6]}...'\n"
        f"na posição SN={req.sn} com ballot={req.ballot}.\"\n\n"
        f"No código real (ProposeIfDue), o líder faz\n"
        f"CutBatch do BucketGroup e envia ACCEPT direto\n"
        f"(sem PREPARE repetido — steady state).\n\n"
        f"─── Detalhes (MPxAccept) ───\n"
        f"Ballot={req.ballot} | Batch.digest={req.batch_digest}\n"
        f"GroupId={req.group_id} | SN={req.sn}"
    )

    st.messages = [Message(
        MsgType.ACCEPT, req.leader, nid,
        label=f"ACCEPT sn={req.sn} b={req.ballot}",
        detail=f"batch={req.batch_digest[:8]} groupId={req.group_id}",
        sn=req.sn,
        ballot=req.ballot,
        batch_digest=req.batch_digest,
    ) for nid in group.members]


def phase_accepted(st: SimState):
    """ACCEPTED — Fase 2b."""
    req = st.current_request
    group = st.groups[req.group_id] if req.group_id < len(st.groups) else st.groups[1]
    req.accepted_received = len(group.members)

    st.phase = Phase.ACCEPTED
    st.log_event(Phase.ACCEPTED, "ACCEPTED (Fase 2b)",
                 f"Accepted: {req.accepted_received}/{req.quorum} QUORUM\n"
                 f"SN={req.sn} confirmado",
                 "phase_accepted")

    st.info_text = (
        f"✅ ACCEPTED (Fase 2b) — Quorum aceita!\n\n"
        f"Membros responderam com MPxAccepted:\n"
        f"{req.accepted_received} de {req.quorum} necessários ✓\n\n"
        f"Cada membro gravou: acceptedBallot={req.ballot},\n"
        f"lastDigest={req.batch_digest[:8]}\n\n"
        f"No código real (onAccepted), quando acceptCount\n"
        f">= quorum, o líder emite COMMIT e chama\n"
        f"onCommit → deliverCommit.\n\n"
        f"─── Detalhes (MPxAccepted) ───\n"
        f"Ballot={req.ballot} | Ok=true | GroupId={req.group_id}"
    )

    st.messages = [Message(
        MsgType.ACCEPTED, nid, req.leader,
        label=f"ACCEPTED b={req.ballot} ok",
        detail=f"sn={req.sn} groupId={req.group_id}",
        sn=req.sn,
        ballot=req.ballot,
        batch_digest=req.batch_digest,
    ) for nid in group.members if nid != req.leader]


def phase_commit(st: SimState):
    """COMMIT — consenso atingido."""
    req = st.current_request
    group = st.groups[req.group_id] if req.group_id < len(st.groups) else st.groups[1]

    # Emite evento visual: commit
    st.visual_events.append({"type": "commit", "sn": req.sn, "leader": req.leader, "ttl": 22})

    st.phase = Phase.COMMIT
    st.log_event(Phase.COMMIT, "COMMIT",
                 f"SN={req.sn} committed em G{req.group_id}\n"
                 f"Total: {st.committed}",
                 "phase_commit")

    st.info_text = (
        f"🎉 COMMIT — Decisão final!\n\n"
        f"Líder emite MPxCommit com digest do batch.\n"
        f"Cada membro chama onCommit → deliverCommit:\n"
        f"• Remove batch do bucket (RemoveBatch)\n"
        f"• Anuncia ao announcer (log.Entry)\n"
        f"• Se cross-op: verifica ADeliver (GSN order)\n\n"
        f"Total de decisões: {st.committed}\n\n"
        f"─── Detalhes (MPxCommit) ───\n"
        f"Digest={req.batch_digest[:8]} | GroupId={req.group_id}\n"
        f"SN={req.sn}"
    )

    st.messages = [Message(
        MsgType.COMMIT, req.leader, nid,
        label=f"COMMIT d={req.batch_digest[:6]}",
        detail=f"sn={req.sn} groupId={req.group_id}",
        sn=req.sn,
        ballot=req.ballot,
        batch_digest=req.batch_digest,
    ) for nid in group.members]


def phase_commit_notify(st: SimState):
    """COMMIT_NOTIFY — proxy recebe notificação e responde ao cliente."""
    req = st.current_request
    st.committed += 1
    proxy = req.proxy_node
    client_idx = req.client_id if req.client_id < len(st.clients) else 0
    client = st.clients[client_idx]

    # Proxy notifica cliente
    st.proxy.notify_commit(req.client_id, req.client_sn)

    # Registra no histórico visual (mensagens COMMIT já chegaram ao destino)
    st.commit_history.append({
        "sn": req.sn,
        "leader": req.leader,
        "checkpoint_idx": st.checkpoints_done,
        "hash": req.payload_hash[:6],
        "is_cross": req.is_cross_group,
        "gsn": req.gsn,
        "group": req.group_id,
        "color": req.color,
    })
    if len(st.commit_history) > 60:
        st.commit_history = st.commit_history[-60:]

    st.phase = Phase.COMMIT_NOTIFY
    st.log_event(Phase.COMMIT_NOTIFY, "COMMIT_NOTIFY -> Cliente",
                 f"Proxy Node {proxy} responde a {client.name}\n"
                 f"SN={req.sn} confirmado | Request entregue",
                 "green")

    st.info_text = (
        f"📬 COMMIT_NOTIFY — Proxy responde ao cliente\n\n"
        f"No código real (NotifyProxy), o líder envia\n"
        f"SYSTEM:COMMIT_NOTIFY_BATCH ao proxy original.\n"
        f"O proxy então chama RespondToClient.\n\n"
        f"Node {proxy} → {client.name}: \"Confirmado!\"\n\n"
        f"─── Fluxo CSMR completo ───\n"
        f"Cliente → Proxy → sendToGroup → Bucket →\n"
        f"CutBatch → ACCEPT → ACCEPTED → COMMIT →\n"
        f"NotifyProxy → RespondToClient ✓"
    )

    st.messages = [Message(
        MsgType.COMMIT_NOTIFY, proxy, req.client_id,
        from_is_client=False,
        to_is_client=True,
        label=f"RESPONSE sn={req.sn} ok",
        detail=f"sn={req.sn} committed",
        sn=req.sn,
        batch_digest=req.batch_digest,
    )]


def phase_adeliver(st: SimState):
    """ADeliver — verifica se cross-op pode ser entregue (ordem GSN).

    No sistema real, cada grupo tocado por uma cross-op roda seu proprio
    MultiPaxos e chama ADeliver separadamente para aquele mesmo GSN — por
    isso registramos uma entrada de entrega POR GRUPO tocado, nao so no
    grupo "de origem" desta request simulada; caso contrario a fila de
    entrega do outro grupo nunca fecharia a lacuna de ordem e ficaria
    bloqueada para sempre.
    """
    req = st.current_request
    groups = req.touched_groups if req.touched_groups else [req.group_id]
    entries = [
        DeliveryEntry(sn=req.sn, gsn=req.gsn, group_id=gid, batch_digest=req.batch_digest)
        for gid in groups
    ]
    # Guardado na request: se ficar bloqueado agora, tick.py precisa checar
    # depois (quando outro commit de qualquer grupo tocado rodar
    # _try_deliver de novo) se todas essas entradas (entry.delivered) foram
    # liberadas nesse meio tempo — espelha o BufferCommit/drainBuffer real.
    req._delivery_entries = entries
    delivered_any = False
    for entry in entries:
        if st.delivery.register_commit(entry):
            delivered_any = True
    next_expected = st.delivery.get_next_expected_gsn(req.group_id)
    blocked = st.delivery.get_blocked_entries(req.group_id)

    # Enriquece historico com touched_groups
    if delivered_any and st.delivery.delivery_history:
        last = st.delivery.delivery_history[-1]
        if last["sn"] == req.sn:
            last["groups"] = list(groups)

    is_blocked = not all(e.delivered for e in entries) and req.gsn > 0

    st.phase = Phase.ADELIVER
    if is_blocked:
        st.log_event(Phase.ADELIVER, "ADeliver BLOQUEADO",
                     f"GSN={req.gsn} bloqueado em G{req.group_id}\n"
                     f"Esperando GSN={next_expected} ser entregue primeiro\n"
                     f"Bloqueados: {[e.gsn for e in blocked]}",
                     "red")
        st.info_text = (
            f"🔒 Entrega BLOQUEADA — esperando a vez\n\n"
            f"Este pedido (nº global {req.gsn}) não pode ser\n"
            f"entregue ainda porque o pedido nº {next_expected}\n"
            f"ainda não foi processado.\n\n"
            f"É como uma fila: mesmo que seu pedido\n"
            f"fique pronto antes, ele espera a vez\n"
            f"para manter a ordem correta."
        )
    else:
        st.log_event(Phase.ADELIVER, "ADeliver OK",
                     f"{'GSN=' + str(req.gsn) + ' entregue' if req.gsn > 0 else 'Single-group -> entrega imediata'}\n"
                     f"Proximo GSN esperado: {next_expected}",
                     "green")
        st.info_text = (
            f"🔓 Entrega confirmada!\n\n"
            f"{'Pedido multi-grupo nº ' + str(req.gsn) + ' entregue na ordem correta!' if req.gsn > 0 else 'Pedido simples (1 grupo) → entregue imediatamente.'}\n\n"
            f"O sistema garante que pedidos que afetam\n"
            f"múltiplos grupos sejam entregues na mesma\n"
            f"ordem em todos os grupos."
        )

    # Mensagem visual (entrega interna)
    st.messages = [Message(
        MsgType.COMMIT, req.leader, req.leader,
        label="ADeliver" + (" ✓" if not is_blocked else " 🔒"),
        detail=f"gsn={req.gsn}",
    )]


def phase_checkpoint(st: SimState):
    """Checkpoint — broadcast entre todos os nós.

    No MultiPaxosMulticastOrderer, um checkpoint só permite truncar o log
    (descartar entradas antigas); ao contrário do ISS clássico, ele NÃO troca
    líderes nem redistribui buckets — os grupos de dados são estáticos.
    """
    req = st.current_request
    st.last_checkpoint_sn = req.sn
    st.checkpoints_done += 1

    st.phase = Phase.CHECKPOINT
    st.log_event(Phase.CHECKPOINT, "CHECKPOINT",
                 f"SN={req.sn} | Intervalo: {st.checkpoint_interval}\n"
                 f"Broadcast CHECKPOINT -> log pode ser truncado\n"
                 f"(grupos e lideres continuam os mesmos)",
                 "purple")

    st.info_text = (
        f"🏁 CHECKPOINT — Salvando progresso\n\n"
        f"O sistema 'salva o jogo' periodicamente.\n"
        f"Todos os nós confirmam entre si que estão\n"
        f"sincronizados até a decisão nº {req.sn}.\n\n"
        f"Isso permite descartar dados antigos e\n"
        f"ajuda nós que ficaram para trás a se\n"
        f"atualizarem rapidamente.\n\n"
        f"Diferente do ISS clássico: aqui o checkpoint NÃO\n"
        f"troca líderes nem redistribui buckets — os grupos\n"
        f"de dados são fixos.\n\n"
        f"─── Detalhes ───\n"
        f"Checkpoint nº {st.checkpoints_done} | Intervalo: a cada {st.checkpoint_interval} decisões"
    )

    st.messages = []
    for n1 in st.nodes:
        for n2 in st.nodes:
            if n1.id != n2.id and n1.is_alive and n2.is_alive:
                st.messages.append(Message(
                    MsgType.CHECKPOINT, n1.id, n2.id,
                    label="CKPT",
                    detail=f"sn={req.sn}",
                ))


def phase_view_change(st: SimState, old_leader: int, new_leader: int, new_ballot: int):
    """View Change — novo ballot proposto após falha do líder."""
    st.phase = Phase.VIEW_CHANGE
    st.log_event(Phase.VIEW_CHANGE, "VIEW CHANGE",
                 f"Lider Node {old_leader} falhou!\n"
                 f"Novo lider: Node {new_leader} | Ballot: {new_ballot}\n"
                 f"Followers enviam VIEW_CHANGE",
                 "red")

    st.info_text = (
        f"⚠️ Líder caiu! Elegendo substituto...\n\n"
        f"Node {old_leader} parou de responder.\n"
        f"Os outros perceberam (timeout) e estão\n"
        f"elegendo um novo coordenador.\n\n"
        f"Novo líder: Node {new_leader}\n\n"
        f"O sistema continua funcionando mesmo com\n"
        f"falhas — isso é tolerância a faltas!\n\n"
        f"─── Detalhes ───\n"
        f"Ballot antigo → novo: {new_ballot}\n"
        f"Pendências serão re-propostas pelo novo líder."
    )

    alive_nodes = [n for n in st.nodes if n.is_alive and n.id != old_leader]
    st.messages = [Message(
        MsgType.VIEW_CHANGE, n.id, new_leader,
        label=f"VIEW_CHANGE b={new_ballot}",
        detail=f"suspect={old_leader}",
        ballot=new_ballot,
        sn=-1,
    ) for n in alive_nodes]


def phase_retransmit(st: SimState):
    """Retransmissão — líder não recebeu quorum, reenvia ACCEPT."""
    req = st.current_request
    group = st.groups[req.group_id] if req.group_id < len(st.groups) else st.groups[1]

    st.phase = Phase.RETRANSMIT
    st.log_event(Phase.RETRANSMIT, "TIMEOUT -> Retransmissao",
                 f"Lider Node {req.leader} nao recebeu quorum de ACCEPTED\n"
                 f"Reenviando ACCEPT para G{req.group_id}",
                 "red")

    st.info_text = (
        f"⏱️ TIMEOUT! Retransmitindo...\n\n"
        f"O líder (Node {req.leader}) esperou uma resposta\n"
        f"mas nem todos responderam a tempo.\n\n"
        f"Isso pode acontecer por:\n"
        f"• Rede lenta entre os nós\n"
        f"• Um nó sobrecarregado\n"
        f"• Mensagem perdida no caminho\n\n"
        f"Solução: o líder REENVIA a proposta.\n"
        f"O protocolo é seguro: reenviar não causa\n"
        f"problemas (mensagens duplicadas são ignoradas).\n\n"
        f"─── Detalhes (mpxInstance.tick) ───\n"
        f"acceptRtxEvery expirou → re-ACCEPT\n"
        f"SN={req.sn} | Ballot={req.ballot}"
    )

    st.messages = [Message(
        MsgType.ACCEPT, req.leader, nid,
        label=f"RE-ACCEPT sn={req.sn}",
        detail=f"retransmit ballot={req.ballot}",
        sn=req.sn,
        ballot=req.ballot,
        batch_digest=req.batch_digest,
    ) for nid in group.members]


def phase_done(st: SimState):
    """Request completa."""
    req = st.current_request
    st.phase = Phase.DONE
    st.log_event(Phase.DONE, "Request completa",
                 f"SN={req.sn} | G{req.group_id} | Committed: {st.committed}",
                 "green")

    st.info_text = (
        f"✨ Pedido processado com sucesso!\n\n"
        f"O ciclo completo terminou:\n"
        f"  Envio → Classificação → Empacotamento →\n"
        f"  Votação → Aprovação → Confirmação ✓\n\n"
        f"Total de decisões: {st.committed}\n\n"
        f"{'Próximo pedido será processado automaticamente...' if not st.step_mode else 'Pressione ⏭ para o próximo pedido.'}"
    )
