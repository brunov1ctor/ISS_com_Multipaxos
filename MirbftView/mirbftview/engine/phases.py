"""Máquina de estados — executa cada fase do protocolo gerando mensagens."""

import hashlib
import random
import string

from mirbftview.protocol.types import (
    Phase, MsgType, Message, RequestInfo, SegmentInfo,
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

    # Bucket assignment: request→bucket (fixo, não muda com epoch)
    bucket_id = st.epoch_mgr.get_request_bucket(client.id, st.client_sn)

    # Quem é o líder deste bucket nesta epoch?
    leader = st.epoch_mgr.get_leader_for_bucket(bucket_id)

    # Segmento do líder
    seg = st.epoch_mgr.get_segment_for_leader(leader)
    group_id = touched[0]
    group = st.groups[group_id] if group_id < len(st.groups) else st.groups[1]
    quorum = len(group.members) // 2 + 1

    # Proxy = nó que recebe do cliente (pode ser qualquer nó, aqui usamos o líder)
    proxy_node = leader

    st.current_request = RequestInfo(
        client_id=client.id,
        client_sn=st.client_sn,
        payload=payload,
        payload_hash=payload_hash,
        bucket_id=bucket_id,
        group_id=group_id,
        gsn=gsn,
        sn=0,  # será atribuído quando o batch for proposto
        segment_id=seg.seg_id if seg else -1,
        leader=leader,
        ballot=seg.seg_id if seg else 0,
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
                 f"Bucket: {bucket_id} (formula: (clID+clSN) % {st.num_buckets})\n"
                 f"Lider do bucket: Node {leader} (epoch {st.epoch_mgr.epoch})\n"
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
        f"Bucket: {bucket_id} | Líder: Node {leader} | Seg: {seg.seg_id if seg else '?'}"
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

    # Info sobre bucket assignment REAL
    epoch = st.epoch_mgr.epoch
    assignment = st.epoch_mgr.bucket_assignment
    group = st.groups[req.group_id] if req.group_id < len(st.groups) else st.groups[1]

    st.phase = Phase.BUCKET_ASSIGN
    st.log_event(Phase.BUCKET_ASSIGN, "Bucket Assignment",
                 f"Request -> Bucket {bucket_id}\n"
                 f"  Formula request->bucket: (clID={req.client_id} + clSN={req.client_sn}) % {st.num_buckets}\n"
                 f"  Formula bucket->lider (epoch {epoch}):\n"
                 f"    Passo 1: round-robin com offset epoch entre todos os nos\n"
                 f"    Passo 2: buckets de nao-lideres redistribuidos\n"
                 f"  Bucket {bucket_id} -> Lider Node {leader}\n"
                 f"  Assignment completo: {assignment}",
                 "gold")

    st.info_text = (
        f"🪣 Classificando o pedido numa 'caixa de entrada'\n\n"
        f"O sistema tem {st.num_buckets} caixas (buckets).\n"
        f"Cada caixa tem um responsável (líder).\n\n"
        f"📡 O pedido é enviado para TODOS os membros\n"
        f"do grupo G{req.group_id}: {group.members}\n"
        f"(não só o líder — garante tolerância a falhas)\n\n"
        f"─── Como a fila é escolhida? ───\n"
        f"Fórmula: (cliente + nº do pedido) mod {st.num_buckets}\n"
        f"= ({req.client_id} + {req.client_sn}) mod {st.num_buckets} = fila {bucket_id}\n\n"
        f"─── Quem atende cada fila (epoch {epoch})? ───\n"
        + "\n".join(f"  Node {l}: filas {bs}" for l, bs in sorted(assignment.items()))
    )

    # Mensagem visual: proxy encaminha para TODOS os membros do grupo
    # (conforme sendToGroup no código real do MirBFT)
    st.messages = []
    src = req.proxy_node
    for nid in group.members:
        if nid == src:
            continue
        st.messages.append(Message(
            MsgType.CLIENT_REQUEST, src, nid,
            label=f"FWD B{bucket_id}" if nid != leader else f"FWD→Líder B{bucket_id}",
            detail=f"bucket={bucket_id} group=G{req.group_id}",
        ))
    if not st.messages:
        st.messages = [Message(
            MsgType.CLIENT_REQUEST, leader, leader,
            label=f"Bucket {bucket_id} enfileirado",
            detail=f"Aguardando CutBatch",
        )]


def phase_batch_cut(st: SimState):
    """Líder corta o batch (CutBatch) — acumula até batch_visual_size."""
    req = st.current_request
    cutter = st.batch_cutters.get(req.bucket_id)
    fill = st.batch_fill.get(req.bucket_id, 1)

    batch_digest = hashlib.sha256(
        f"{req.payload_hash}:{req.bucket_id}:{st.epoch_mgr.epoch}".encode()
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

    # Emite evento visual: batch cortado
    st.visual_events.append({"type": "batch_cut", "bucket": req.bucket_id, "ttl": 20})

    # Remove do cutter
    if cutter:
        cutter.force_cut()

    # Atribui SN do segmento
    seg = st.epoch_mgr.get_segment_for_leader(req.leader)
    if seg and seg.sns:
        # Usa o próximo SN disponível no segmento
        req.sn = seg.sns[min(st.committed % seg.sn_length, seg.sn_length - 1)]
    else:
        req.sn = st.committed

    cut_reason = "cross_op_immediate" if req.is_cross_group else "size/timeout"

    st.phase = Phase.BATCH_CUT
    st.log_event(Phase.BATCH_CUT, "CutBatch",
                 f"Lider Node {req.leader} corta batch do Bucket {req.bucket_id}\n"
                 f"Motivo: {cut_reason}\n"
                 f"Requests no batch: {req.batch_requests}\n"
                 f"Batch digest: {batch_digest}\n"
                 f"SN atribuido: {req.sn} (do segmento {req.segment_id})\n"
                 f"Segmento SNs: {seg.sns if seg else '?'}",
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
        f"Segmento {req.segment_id} | Distance={seg.sn_distance if seg else '?'}"
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
        f"📋 PREPARE — O líder pede permissão\n\n"
        f"Node {req.leader} pergunta ao grupo:\n"
        f"\"Posso coordenar a decisão nº {req.sn}?\"\n\n"
        f"Precisa que a MAIORIA concorde ({req.quorum} de\n"
        f"{len(group.members)} membros) — isso é o 'quorum'.\n\n"
        f"Se a maioria disser sim, ele tem autoridade\n"
        f"para propor um valor.\n\n"
        f"─── Detalhes ───\n"
        f"Ballot={req.ballot} | Grupo G{req.group_id} | Membros: {group.members}"
    )

    st.messages = [Message(
        MsgType.PREPARE, req.leader, nid,
        label=f"PREPARE sn={req.sn}",
        detail=f"ballot={req.ballot}",
        sn=req.sn,
        ballot=req.ballot,
        batch_digest=req.batch_digest,
    ) for nid in group.members]


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
        f"🤝 PROMISE — O grupo concorda\n\n"
        f"Os membros responderam: \"Sim, você pode\n"
        f"coordenar! Prometemos não aceitar outro\n"
        f"coordenador com prioridade menor.\"\n\n"
        f"Respostas: {req.promises_received} de {req.quorum} necessárias ✓\n\n"
        f"Analogia: é como uma eleição — a maioria\n"
        f"votou neste líder, então ele tem mandato."
    )

    st.messages = [Message(
        MsgType.PROMISE, nid, req.leader,
        label=f"PROMISE sn={req.sn} b={req.ballot}",
        detail=f"ballot={req.ballot}",
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
        f"📦 ACCEPT — O líder propõe o valor\n\n"
        f"Agora que tem permissão, Node {req.leader} diz:\n"
        f"\"Proponho que o pacote '{req.batch_digest[:6]}...'\n"
        f"seja registrado na posição {req.sn}.\"\n\n"
        f"Todos os membros do grupo recebem a proposta\n"
        f"e vão decidir se aceitam ou não.\n\n"
        f"─── Detalhes ───\n"
        f"Ballot={req.ballot} | Digest={req.batch_digest}"
    )

    st.messages = [Message(
        MsgType.ACCEPT, req.leader, nid,
        label=f"ACCEPT sn={req.sn} b={req.ballot}",
        detail=f"sn={req.sn}",
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
        f"✅ ACCEPTED — Todos concordam!\n\n"
        f"A maioria aceitou a proposta:\n"
        f"{req.accepted_received} de {req.quorum} necessários ✓\n\n"
        f"Cada membro gravou o pacote no seu registro.\n"
        f"Agora é IMPOSSÍVEL que outro valor seja\n"
        f"registrado nesta posição — consenso quase pronto!\n\n"
        f"Próximo: o líder confirma para todos (COMMIT)."
    )

    st.messages = [Message(
        MsgType.ACCEPTED, nid, req.leader,
        label="ACCEPTED ✓",
        detail=f"sn={req.sn}",
        sn=req.sn,
        ballot=req.ballot,
        batch_digest=req.batch_digest,
    ) for nid in group.members if nid != req.leader]


def phase_commit(st: SimState):
    """COMMIT — consenso atingido."""
    req = st.current_request
    group = st.groups[req.group_id] if req.group_id < len(st.groups) else st.groups[1]
    st.committed += 1

    # Emite evento visual: commit
    st.visual_events.append({"type": "commit", "sn": req.sn, "leader": req.leader, "ttl": 22})

    # Registra no histórico visual
    st.commit_history.append({
        "sn": req.sn,
        "leader": req.leader,
        "epoch": st.epoch_mgr.epoch,
        "hash": req.payload_hash[:6],
        "is_cross": req.is_cross_group,
        "gsn": req.gsn,
        "segment": req.segment_id,
        "group": req.group_id,
    })
    if len(st.commit_history) > 60:
        st.commit_history = st.commit_history[-60:]

    st.phase = Phase.COMMIT
    st.log_event(Phase.COMMIT, "COMMIT",
                 f"SN={req.sn} committed em G{req.group_id}\n"
                 f"Total: {st.committed}",
                 "phase_commit")

    st.info_text = (
        f"🎉 COMMIT — Decisão final!\n\n"
        f"O grupo DECIDIU: o pacote foi aceito\n"
        f"permanentemente na posição {req.sn}.\n\n"
        f"Nenhum participante pode voltar atrás.\n"
        f"Total de decisões até agora: {st.committed}\n\n"
        f"Agora falta avisar o cliente que seu\n"
        f"pedido foi processado com sucesso."
    )

    st.messages = [Message(
        MsgType.COMMIT, req.leader, nid,
        label="COMMIT ✓",
        detail=f"sn={req.sn}",
        sn=req.sn,
        ballot=req.ballot,
        batch_digest=req.batch_digest,
    ) for nid in group.members]


def phase_commit_notify(st: SimState):
    """COMMIT_NOTIFY — proxy recebe notificação e responde ao cliente."""
    req = st.current_request
    proxy = req.proxy_node
    client_idx = req.client_id if req.client_id < len(st.clients) else 0
    client = st.clients[client_idx]

    # Proxy notifica cliente
    st.proxy.notify_commit(req.client_id, req.client_sn)

    st.phase = Phase.COMMIT_NOTIFY
    st.log_event(Phase.COMMIT_NOTIFY, "COMMIT_NOTIFY -> Cliente",
                 f"Proxy Node {proxy} responde a {client.name}\n"
                 f"SN={req.sn} confirmado | Request entregue",
                 "green")

    st.info_text = (
        f"📬 Avisando o cliente: \"Seu pedido foi aprovado!\"\n\n"
        f"O 'carteiro' (Node {proxy}) segurou a resposta\n"
        f"até ter CERTEZA de que o grupo aprovou.\n\n"
        f"Agora envia a confirmação ao {client.name}.\n\n"
        f"─── Fluxo completo ───\n"
        f"Cliente → Carteiro → Líder → Votação →\n"
        f"Aprovação → Carteiro → Cliente ✓"
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
    """ADeliver — verifica se cross-op pode ser entregue (ordem GSN)."""
    req = st.current_request
    entry = DeliveryEntry(
        sn=req.sn, gsn=req.gsn, group_id=req.group_id, batch_digest=req.batch_digest,
    )
    delivered = st.delivery.register_commit(entry)
    blocked = st.delivery.get_blocked_entries(req.group_id)
    next_expected = st.delivery.get_next_expected_gsn(req.group_id)

    # Enriquece historico com touched_groups
    if delivered and st.delivery.delivery_history:
        last = st.delivery.delivery_history[-1]
        if last["sn"] == req.sn:
            last["groups"] = list(req.touched_groups) if req.touched_groups else [req.group_id]

    is_blocked = not delivered and req.gsn > 0

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
    """Checkpoint — broadcast entre todos os nós."""
    req = st.current_request
    st.last_checkpoint_sn = req.sn

    st.phase = Phase.CHECKPOINT
    st.log_event(Phase.CHECKPOINT, "CHECKPOINT",
                 f"SN={req.sn} | Intervalo: {st.checkpoint_interval}\n"
                 f"Broadcast CHECKPOINT -> quorum estabiliza",
                 "purple")

    st.info_text = (
        f"🏁 CHECKPOINT — Salvando progresso\n\n"
        f"O sistema 'salva o jogo' periodicamente.\n"
        f"Todos os nós confirmam entre si que estão\n"
        f"sincronizados até a decisão nº {req.sn}.\n\n"
        f"Isso permite descartar dados antigos e\n"
        f"ajuda nós que ficaram para trás a se\n"
        f"atualizarem rapidamente.\n\n"
        f"─── Detalhes ───\n"
        f"Epoch: {st.epoch_mgr.epoch} | Intervalo: a cada {st.checkpoint_interval} decisões"
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


def phase_epoch_transition(st: SimState):
    """Transição de epoch — leader policy + redistribuição de buckets."""
    info = st.epoch_mgr.advance_epoch()

    st.phase = Phase.EPOCH_TRANSITION
    st.log_event(Phase.EPOCH_TRANSITION, "EPOCH TRANSITION",
                 f"Epoch {info['old_epoch']} -> {info['new_epoch']}\n"
                 f"Lideres: {info['old_leaders']} -> {info['new_leaders']}\n"
                 f"Buckets redistribuidos",
                 "purple")

    old_assign = info['old_bucket_assignment']
    new_assign = info['new_bucket_assignment']

    st.info_text = (
        f"🔄 Troca de turno! (Epoch {info['old_epoch']} → {info['new_epoch']})\n\n"
        f"Periodicamente o sistema redistribui\n"
        f"responsabilidades entre os nós.\n\n"
        f"É como uma troca de turno num hospital:\n"
        f"novos médicos assumem os guichês.\n\n"
        f"Líderes antes: {info['old_leaders']}\n"
        f"Líderes agora: {info['new_leaders']}\n\n"
        f"As filas (buckets) foram redistribuídas\n"
        f"entre os novos responsáveis."
    )

    # Mensagem visual: broadcast de novo assignment
    st.messages = [Message(
        MsgType.CHECKPOINT, st.nodes[0].id, st.nodes[0].id,
        label=f"NEW EPOCH {info['new_epoch']}",
        detail="segments issued",
    )]


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
                 f"SN={req.sn} | Epoch {st.epoch_mgr.epoch} | Committed: {st.committed}",
                 "green")

    seg = st.epoch_mgr.get_segment_for_leader(req.leader)
    st.info_text = (
        f"✨ Pedido processado com sucesso!\n\n"
        f"O ciclo completo terminou:\n"
        f"  Envio → Classificação → Empacotamento →\n"
        f"  Votação → Aprovação → Confirmação ✓\n\n"
        f"Total de decisões: {st.committed}\n"
        f"Época atual: {st.epoch_mgr.epoch}\n\n"
        f"{'Próximo pedido será processado automaticamente...' if not st.step_mode else 'Pressione ⏭ para o próximo pedido.'}"
    )
