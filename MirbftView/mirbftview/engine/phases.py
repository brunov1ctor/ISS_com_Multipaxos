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


BUCKET_ENTER_TICKS = 20   # animação de entrada de um pedido no bucket
BUCKET_LEAVE_TICKS = 34   # animação de saída (corte do batch)
CROSS_PULSE_TICKS = 90    # pulso da célula quando chega uma cross-op
TICKS_PER_SEC = 62        # 1 s simulado = 62 ticks (~16 ms cada) na velocidade 1x


def bubble_ttl(st, n_phases: float) -> int:
    """Vida de um balão em ticks, proporcional à duração de uma fase (na velocidade
    atual uma fase leva ~1/(velocidade*0.02) ticks): some depois de ~n fases, e não
    fica na tela por dezenas de fases."""
    return max(40, min(300, int(n_phases / max(st.speed * 0.02, 1e-3))))


def node_buckets(st, node_id):
    """Buckets do nó: lista por bucket com os labels dos pedidos, na ORDEM EM QUE
    CHEGARAM A ESSE NÓ (cada nó guarda os seus; podem diferir entre membros)."""
    lists = st.node_buckets.get(node_id)
    if lists is None or len(lists) != st.num_buckets:
        lists = [[] for _ in range(st.num_buckets)]
        st.node_buckets[node_id] = lists
    return lists


def bucket_add(st, node_id, bucket_id, label, meta):
    """AddDirectToBucket neste nó: a cópia do pedido entra no bucket LOCAL dele."""
    lists = node_buckets(st, node_id)
    if not (0 <= bucket_id < len(lists)) or label in lists[bucket_id]:
        return  # já existe (dedup por ClientId/ClientSn, como reqIndex no Go)
    lists[bucket_id].append(label)
    st.bucket_born[(node_id, bucket_id, label)] = st.tick_count
    st.bucket_meta.setdefault(label, dict(meta))
    if meta.get("cross"):
        st.cross_pulse[(node_id, bucket_id)] = st.tick_count
    st.visual_events.append({"type": "bucket_in", "bucket": bucket_id, "node": node_id, "ttl": 18})


def bucket_remove(st, node_id, items):
    """deliverCommit neste nó: RemoveCommittedFromBuckets (seguidor, se tiver a cópia).
    No líder o pedido já saiu no corte, então RemoveBatch não acha mais nada (no-op).
    items = [(bucket_id, label)]. Anima a saída de quem ainda tinha a cópia."""
    lists = node_buckets(st, node_id)
    for j, (bucket_id, label) in enumerate(items):
        if not (0 <= bucket_id < len(lists)):
            continue
        if label in lists[bucket_id]:
            lists[bucket_id].remove(label)
            meta = st.bucket_meta.get(label, {})
            st.bucket_leaving.append({"node": node_id, "bucket": bucket_id, "slot": j,
                                      "color": meta.get("color", ""), "born": st.tick_count})
        st.bucket_born.pop((node_id, bucket_id, label), None)
        if not any(label in c for cs in st.node_buckets.values() for c in cs):
            st.bucket_meta.pop(label, None)


def _take_singles(st, bucket, n):
    """RemoveFirstSingleGroup: tira até n pedidos single-group do início do bucket."""
    taken = []
    i = 0
    while i < len(bucket) and len(taken) < n:
        if st.bucket_meta.get(bucket[i], {}).get("cross"):
            i += 1
            continue
        taken.append(bucket.pop(i))
    return taken


def _cut_items(st, leader, gid, skip_cross=False):
    """CutBatch (bucketgroup.go) sobre os buckets DO LÍDER, sem timeout: corta o que houver.

    1) cross-ops de TODOS os buckets do grupo, por GSN, vêm primeiro; se há alguma, o
       batch é completado com single-group que já estão nos buckets (sem esperar);
    2) sem cross-op: initCut = tamanho/nº de buckets (ou total/nº de buckets se há menos
       pedidos que o tamanho) de cada bucket, e depois completa em ordem de bucket.
    Os pedidos saem da lista do bucket do líder agora (removeNoLock)."""
    lists = node_buckets(st, leader)
    bids = st.epoch_mgr.buckets_of_group(gid)
    size = max(int(st.batch_size), 1)
    meta = st.bucket_meta
    batch = []
    cross = [] if skip_cross else sorted(
        (meta.get(l, {}).get("gsn", 0), b, l)
        for b in bids for l in lists[b] if meta.get(l, {}).get("cross"))
    for _gsn, b, l in cross:
        if len(batch) >= size:
            break
        lists[b].remove(l)
        batch.append((b, l))
    if batch:
        remaining = size - len(batch)
        for b in bids:
            if remaining <= 0:
                break
            taken = _take_singles(st, lists[b], remaining)
            batch += [(b, l) for l in taken]
            remaining -= len(taken)
    else:
        total = sum(len(lists[b]) for b in bids)
        if total == 0:
            return []
        init = size // len(bids) if size <= total else total // len(bids)
        for b in bids:
            batch += [(b, l) for l in _take_singles(st, lists[b], init)]
        for b in bids:
            if len(batch) >= size:
                break
            batch += [(b, l) for l in _take_singles(st, lists[b], size - len(batch))]
    return batch


def request_label(req):
    """Identificador único do pedido nos buckets: (ClientId, ClientSn)."""
    return f"c{req.client_id}#{req.client_sn}"


def new_instance(st, gid):
    """Nasce a instância seguinte do grupo: SN = próximo do grupo (g, g+nGrupos, ...),
    líder pela política (members[sn % n] ou members[0]); começa pelo PREPARE."""
    from mirbftview.qt.canvas._constants import MSG_COLOR_POOL
    group = st.groups[gid]
    sn = st.epoch_mgr.next_sn_for(gid)
    st.epoch_mgr.advance(gid)
    leader = st.epoch_mgr.leader_for_group(gid, sn)
    data_ids = [g.id for g in st.groups if g.id != 0]
    color = MSG_COLOR_POOL[data_ids.index(gid) % len(MSG_COLOR_POOL)]
    inst = RequestInfo(kind="instance", group_id=gid, sn=sn, leader=leader, ballot=0,
                       quorum=len(group.members) // 2 + 1, phase=Phase.PREPARE,
                       color=color, touched_groups=[gid], batch_requests=0)
    inst._waiting = False
    inst._batch_items = []
    inst._wait_started = False   # o timer do CutBatch começa quando ele passa a esperar
    inst._wait_elapsed = 0.0     # ticks (ponderados pela velocidade) esperando pedidos
    inst._cross_at_start = False
    inst._cut_reason = ""
    st.instances[gid] = inst
    st.active_requests.append(inst)
    hist = st.sn_history.setdefault(gid, [])
    hist.append({"sn": sn, "leader": leader, "born": st.tick_count, "inst": inst})
    if len(hist) > 60:
        del hist[:-60]
    return inst


def _queued(st, leader, bids):
    """(pedidos single-group, cross-ops) nos buckets do líder para os buckets do grupo."""
    lists = node_buckets(st, leader)
    singles = cross = 0
    for b in bids:
        for l in lists[b]:
            if st.bucket_meta.get(l, {}).get("cross"):
                cross += 1
            else:
                singles += 1
    return singles, cross


def try_cut(st, inst):
    """ProposeIfDue -> CutBatch (bucketgroup.go) com a instância preparada.

    O timer começa quando o CutBatch passa a esperar (logo após o PROMISE) e a espera acaba
    por: (a) cross-op já pronta no início -> corta na hora, cross-ops primeiro; (b) batch
    cheio (BatchSize); (c) timeout, cortando o que houver; (d) uma cross-op que CHEGA durante
    a espera acorda o CutBatch na hora (RequestAddedCrossOp): sai o que já estava acumulado
    de single-group e a cross-op fica para o corte seguinte (se não havia nenhum single, ela
    sai já). Devolve True se cortou."""
    gid = inst.group_id
    bids = st.epoch_mgr.buckets_of_group(gid)
    size = max(int(st.batch_size), 1)
    singles, cross = _queued(st, inst.leader, bids)

    if not inst._wait_started:
        inst._wait_started = True
        inst._cross_at_start = cross > 0
        inst._wait_elapsed = 0.0

    reason = ""
    skip_cross = False
    if cross and inst._cross_at_start:
        reason = "cross-op pronta: corte imediato"
    elif cross and singles:
        reason = "cross-op chegou durante a espera: acordou o corte (single-group acumulados saem, a cross-op fica para o próximo)"
        skip_cross = True
    elif cross:
        reason = "cross-op chegou durante a espera: acordou o corte"
    elif singles >= size:
        reason = f"batch cheio ({singles}/{size})"
    elif inst._wait_elapsed >= st.batch_timeout_ticks:
        if singles:
            reason = f"timeout de {st.batch_timeout_ticks / TICKS_PER_SEC:.0f}s: corta o que houver"
        else:
            inst._wait_elapsed = 0.0   # timeout sem nada nos buckets: volta a esperar
    if not reason:
        st.messages = []   # waitForRequests: a instância só espera (contador no painel de SNs)
        return False

    items = _cut_items(st, inst.leader, gid, skip_cross=skip_cross)
    if not items:
        st.messages = []
        return False
    inst._cut_reason = reason

    inst._waiting = False
    inst._batch_items = items
    members = []
    for j, (b, l) in enumerate(items):
        m = dict(st.bucket_meta.get(l, {}))
        m["label"] = l
        m["bucket"] = b
        members.append(m)
        st.bucket_leaving.append({"node": inst.leader, "bucket": b, "slot": j,
                                  "color": m.get("color", ""), "born": st.tick_count})
        st.bucket_born.pop((inst.leader, b, l), None)
    inst.members = members
    inst.batch_requests = len(members)
    crosses = sorted((m for m in members if m.get("cross")), key=lambda m: m.get("gsn", 0))
    inst.is_cross_group = bool(crosses)
    inst.gsn = crosses[0]["gsn"] if crosses else 0
    inst.touched_groups = list(crosses[0].get("touched", [gid])) if crosses else [gid]
    inst.batch_digest = hashlib.sha256(
        (f"{inst.sn}:{gid}:" + ",".join(l for _, l in items)).encode()).hexdigest()[:12]

    for b in sorted({b for b, _ in items}):
        st.visual_events.append({"type": "batch_cut", "bucket": b, "node": inst.leader, "ttl": 20})
    anchor = items[0][0]
    n_cross = len(crosses)
    st.bucket_bubbles[anchor] = {
        "text": (f"CutBatch()\n{len(items)} pedido(s)"
                 + (f" ({n_cross} cross)" if n_cross else "")
                 + f"\ndigest={inst.batch_digest[:6]}"
                 + f"\n{reason}"),
        "color": "green", "ttl": bubble_ttl(st, 1.5), "node": inst.leader}

    st.phase = Phase.BATCH_CUT
    st.log_event(Phase.BATCH_CUT, "CutBatch",
                 f"G{gid} | Lider Node {inst.leader} | SN={inst.sn}\n"
                 f"Batch: {len(items)} pedido(s), {n_cross} cross-op\n"
                 f"Buckets usados: {sorted({b for b, _ in items})}\n"
                 f"Pedidos: {', '.join(l for _, l in items)}\n"
                 f"Digest: {inst.batch_digest}",
                 "gold")
    st.info_text = (
        f"✂️ CutBatch — o líder junta os pedidos\n\n"
        f"Node {inst.leader} (líder de G{gid}, SN={inst.sn}) cortou\n"
        f"{len(items)} pedido(s) dos buckets {bids} do grupo:\n"
        + ("• cross-ops primeiro, ordenadas por GSN\n" if n_cross else "")
        + "• depois os single-group, em ordem de bucket\n\n"
        f"Os pedidos saíram dos buckets do líder agora; nos\n"
        f"seguidores a cópia só sai quando o COMMIT chegar.\n\n"
        f"Motivo do corte: {reason}"
    )
    st.messages = [Message(
        MsgType.CLIENT_REQUEST, inst.leader, inst.leader,
        label=f"CutBatch n={len(items)} d={inst.batch_digest[:6]}",
        detail=f"size={len(items)} cross={n_cross}",
    )]
    return True


def resurrect_batch(st, inst, why):
    """Batch.Resurrect: os pedidos cortados voltam ao INÍCIO dos buckets do líder e a
    instância segue com um batch vazio (NOP); serão cortados pela instância seguinte."""
    lists = node_buckets(st, inst.leader)
    for b, l in reversed(inst._batch_items):
        if l not in lists[b]:
            lists[b].insert(0, l)
            st.bucket_born[(inst.leader, b, l)] = st.tick_count
        if l not in st.bucket_meta:
            st.bucket_meta[l] = next((dict(m) for m in inst.members if m.get("label") == l), {})
    n = len(inst._batch_items)
    inst._batch_items = []
    inst.members = []
    inst.batch_requests = 0
    inst.is_cross_group = False
    inst.gsn = 0
    st.log_event(Phase.ACCEPT, "Batch RESURRECT",
                 f"G{inst.group_id} SN={inst.sn}: {why}\n"
                 f"{n} pedido(s) voltam ao inicio dos buckets do lider\n"
                 f"A instancia segue com batch vazio (NOP)", "orange")
    st.info_text = (
        f"♻️ RESURRECT — {why}\n\n"
        f"Os {n} pedido(s) cortados voltam ao INÍCIO dos\n"
        f"buckets do líder (Batch.Resurrect) e serão\n"
        f"propostos por outra instância. Esta posição do log\n"
        f"é preenchida com um valor vazio (NOP)."
    )


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
    bucket_id = st.epoch_mgr.get_request_bucket(client.id, st.client_sn, group_id)

    # Proxy = no que recebe do cliente: rodizio por ClientSn entre TODOS os nos
    # (guessTargetOrderers real), pode ser de fora do grupo.
    proxy_node = st.proxy.pick_proxy(st.client_sn, [n.id for n in st.nodes])

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
    """O proxy encaminha o pedido a TODOS os membros de cada grupo tocado; cada membro faz
    AddDirectToBucket no SEU bucket quando a cópia chega. Depois o pedido só espera nos
    buckets: quem o leva para um batch é a instância do grupo (try_cut)."""
    req = st.current_request
    label = request_label(req)
    src = req.proxy_node
    dest_groups = req.touched_groups if len(req.touched_groups) > 1 else [req.group_id]
    bucket_of = {gid: st.epoch_mgr.get_request_bucket(req.client_id, req.client_sn, gid)
                 for gid in dest_groups}
    meta = {"color": req.color, "client_id": req.client_id, "client_sn": req.client_sn,
            "proxy": src, "gsn": req.gsn, "cross": bool(req.is_cross_group and req.gsn > 0),
            "touched": list(req.touched_groups), "payload": req.payload}

    group = st.groups[req.group_id]
    inst = st.instances.get(req.group_id)
    st.phase = Phase.BUCKET_ASSIGN
    st.log_event(Phase.BUCKET_ASSIGN, "Bucket Assignment",
                 f"{label} -> " + ", ".join(f"G{g}:B{bucket_of[g]}" for g in dest_groups) + "\n"
                 f"  b = grupo + nGrupos*((clienteId + clienteSn) mod nBucketsDoGrupo)\n"
                 f"  Cada membro coloca a copia no SEU bucket ao receber a mensagem\n"
                 + (f"  Instancia atual de G{req.group_id}: SN={inst.sn} lider Node {inst.leader}\n"
                    if inst else ""),
                 "gold")
    st.info_text = (
        f"🪣 O pedido entra nos buckets\n\n"
        f"O proxy (Node {src}) envia a cópia a TODOS os membros do\n"
        f"grupo G{req.group_id}: {group.members}\n"
        f"(sendToGroup/GsnReqForward). Cada um faz AddDirectToBucket\n"
        f"no SEU bucket local quando a mensagem chega.\n\n"
        f"─── Qual bucket? ───\n"
        f"b = grupo + nGrupos × ((clienteId + clienteSn) mod nB)\n"
        f"= G{req.group_id} → B{bucket_of[req.group_id]}\n\n"
        f"Depois disso o pedido só ESPERA no bucket. Quem o leva para\n"
        f"um batch é o líder da instância do grupo, quando ela estiver\n"
        f"preparada (PREPARE/PROMISE) e cortar (CutBatch)."
    )
    if len(dest_groups) > 1:
        st.info_text += (
            f"\n\n─── Cross-group: uma cópia por grupo ───\n"
            f"Toca {['G' + str(g) for g in dest_groups]}: cada grupo guarda a cópia no seu\n"
            f"bucket e a leva para o batch da sua própria instância."
        )

    st.messages = []
    for gid in dest_groups:
        gmembers = st.groups[gid].members
        b = bucket_of[gid]
        for nid in gmembers:
            if nid == src:
                # O proxy que também é membro do grupo insere localmente (sem mensagem).
                bucket_add(st, nid, b, label, meta)
                continue
            st.messages.append(Message(
                MsgType.CLIENT_REQUEST, src, nid,
                label=f"FWD G{gid} h={req.payload_hash[:6]}",
                detail=f"GsnReqForward group=G{gid}",
                on_arrive=(lambda st_, n=nid, bb=b, l=label, m=meta: bucket_add(st_, n, bb, l, m)),
            ))
    if not st.messages:
        st.messages = [Message(
            MsgType.CLIENT_REQUEST, src, src,
            label=f"AddDirectToBucket B{bucket_of[req.group_id]}",
            detail="local insert",
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
    # Cenário "ADeliver bloqueado": o META deste GSN chega ATRASADO a um dos grupos tocados. O
    # grupo decide a cópia dele normalmente, mas o ADeliver bloqueia ("missing META") até o META
    # chegar. É um bloqueio real do Go e se resolve sozinho (RegisterMetadata -> drainBuffer).
    late_group = None
    if st.scenarios.get('adeliver_block', False) and len(req.touched_groups) > 1:
        late_group = req.touched_groups[-1]
        others = {n for g in req.touched_groups if g != late_group for n in st.groups[g].members}
        exclusive = [n for n in st.groups[late_group].members if n not in others]
        late_nodes = exclusive or list(st.groups[late_group].members)
        st.delivery.hold_meta(req.gsn, late_group)
        st.meta_late.append({"gsn": req.gsn, "group": late_group, "nodes": late_nodes,
                             "state": "held", "wait": 0, "age": 0})
    for gid in req.touched_groups:
        if gid != late_group:
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
    held_nodes = set(st.meta_late[-1]["nodes"]) if late_group is not None else set()
    for nid in st.groups[0].members:
        if nid != seq_leader and nid not in held_nodes:
            st.messages.append(Message(
                MsgType.META_STREAM, seq_leader, nid,
                label=f"META gsn={req.gsn}",
                detail=f"groups={req.touched_groups}",
            ))
    if late_group is not None:
        st.log_event(Phase.GSN_ASSIGN, "Cenario: META atrasado",
                     f"O META do GSN={req.gsn} ainda nao chegou ao G{late_group}\n"
                     f"(nos {sorted(held_nodes)}). Esse grupo vai decidir a copia dele,\n"
                     f"mas o ADeliver vai bloquear ate o META chegar.",
                     "orange")


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
        f"⚡ No código real, TODA instância (cada SN) tem o seu\n"
        f"PREPARE: o líder o envia ao criar a instância\n"
        f"(SetMembers), e só depois pode cortar o batch.\n\n"
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
        f"e chama ProposeIfDue() → CutBatch → ACCEPT.\n\n"
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
        f"O ACCEPT leva o batch cortado do BucketGroup\n"
        f"({req.batch_requests} pedido(s), digest {req.batch_digest[:6]}).\n\n"
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

    st.visual_events.append({"type": "commit", "sn": req.sn, "leader": req.leader, "ttl": 22})

    st.phase = Phase.COMMIT
    st.log_event(Phase.COMMIT, "COMMIT",
                 f"G{req.group_id} SN={req.sn} committed ({req.batch_requests} pedido(s))\n"
                 f"Total: {st.committed}",
                 "phase_commit")

    st.info_text = (
        f"🎉 COMMIT — Decisão final!\n\n"
        f"Líder emite MPxCommit com digest do batch.\n"
        f"Cada membro chama onCommit → deliverCommit:\n"
        f"• Seguidores tiram o batch do bucket (RemoveCommittedFromBuckets);\n"
        f"  no líder os pedidos já saíram no corte\n"
        f"• Anuncia ao announcer (log.Entry)\n"
        f"• Se cross-op: verifica ADeliver (GSN order)\n\n"
        f"Total de decisões: {st.committed}\n\n"
        f"─── Detalhes (MPxCommit) ───\n"
        f"Digest={req.batch_digest[:8]} | GroupId={req.group_id}\n"
        f"SN={req.sn} | {req.batch_requests} pedido(s)"
    )

    items = list(getattr(req, "_batch_items", []))
    st.messages = [Message(
        MsgType.COMMIT, req.leader, nid,
        label=f"COMMIT d={req.batch_digest[:6]}",
        detail=f"sn={req.sn} groupId={req.group_id}",
        sn=req.sn,
        ballot=req.ballot,
        batch_digest=req.batch_digest,
        # deliverCommit em cada membro: sai do bucket local dele quando o COMMIT chega
        on_arrive=(lambda st_, n=nid, its=items: bucket_remove(st_, n, its)),
    ) for nid in group.members]


def phase_commit_notify(st: SimState):
    """COMMIT_NOTIFY — o líder avisa o proxy de cada pedido do batch; o proxy responde ao
    cliente na PRIMEIRA confirmação (LoadAndDelete em proxyPending)."""
    req = st.current_request
    leader = req.leader
    members = list(req.members)
    if members:
        st.committed += 1

    answered = []      # pedidos que ESTA instância respondeu
    for m in members:
        first = st.proxy.notify_commit(m["client_id"], m["client_sn"]) is not None
        if first:
            answered.append(m)
            st.pending_info.pop((m["client_id"], m["client_sn"]), None)

    first_cross = next((m for m in members if m.get("cross")), None)
    st.commit_history.append({
        "sn": req.sn,
        "leader": leader,
        "checkpoint_idx": st.checkpoints_done,
        "hash": req.batch_digest[:6],
        "is_cross": req.is_cross_group,
        "gsn": req.gsn,
        "group": req.group_id,
        "color": (members[0].get("color") if members else req.color) or req.color,
        "n": len(members),
        "items": [{"label": m["label"], "cross": bool(m.get("cross")), "gsn": m.get("gsn", 0),
                   "color": m.get("color", "")} for m in members],
    })
    if len(st.commit_history) > 400:
        st.commit_history = st.commit_history[-400:]

    st.phase = Phase.COMMIT_NOTIFY
    names = {m["label"]: (st.clients[m["client_id"]].name if m["client_id"] < len(st.clients)
                          else f"Client {m['client_id']}") for m in members}
    st.log_event(Phase.COMMIT_NOTIFY, "COMMIT_NOTIFY",
                 f"G{req.group_id} SN={req.sn}: lider Node {leader} avisa os proxies\n"
                 f"{len(members)} pedido(s): {', '.join(m['label'] for m in members) or '(NOP)'}\n"
                 f"Respondidos agora: {len(answered)} | ja respondidos por outro grupo: "
                 f"{len(members) - len(answered)}",
                 "green")
    st.info_text = (
        f"📬 COMMIT_NOTIFY — proxies respondem aos clientes\n\n"
        f"No código real (NotifyProxy), só o LÍDER do SN avisa,\n"
        f"UMA mensagem COMMIT_NOTIFY_BATCH com todos os pedidos do\n"
        f"batch. O proxy de cada pedido chama RespondToClient na\n"
        f"primeira confirmação (num cross-group, o outro grupo\n"
        f"chega depois e não gera 2ª resposta).\n\n"
        f"Batch de G{req.group_id} SN={req.sn}: {len(members)} pedido(s)\n"
        f"respondidos agora: {len(answered)}"
    )

    # Uma NOTIFY_BATCH por proxy; as respostas ao cliente voam depois que ela chega.
    by_proxy = {}
    for m in members:
        by_proxy.setdefault(m["proxy"], []).append(m)
    st.messages = []
    for proxy, ms in by_proxy.items():
        responses = [Message(
            MsgType.COMMIT_NOTIFY, proxy, m["client_id"],
            from_is_client=False, to_is_client=True,
            label=f"RESPONSE {m['label']} sn={req.sn}",
            detail=f"sn={req.sn} committed",
            sn=req.sn, batch_digest=req.batch_digest,
            color=m.get("color", ""),
        ) for m in ms if m in answered]
        if proxy == leader:
            st.messages.extend(responses)
        else:
            st.messages.append(Message(
                MsgType.COMMIT_NOTIFY, leader, proxy,
                label=f"NOTIFY_BATCH n={len(ms)} sn={req.sn}",
                detail="COMMIT_NOTIFY_BATCH orderSn=%d (%s)" % (req.sn, ", ".join(m["label"] for m in ms)),
                sn=req.sn, batch_digest=req.batch_digest,
                then=(responses or None),
            ))


def phase_adeliver(st: SimState):
    """ADeliver — para cada cross-op do batch (em ordem de GSN) o grupo só entrega depois de
    entregar todo GSN anterior que também o toca; se alguma bloqueia, o batch INTEIRO fica
    em BufferCommit (inclusive os single-group que estão nele)."""
    req = st.current_request
    crosses = sorted((m for m in req.members if m.get("cross")), key=lambda m: m.get("gsn", 0))
    if crosses:
        entries = [DeliveryEntry(sn=req.sn, gsn=m["gsn"], group_id=req.group_id,
                                 batch_digest=req.batch_digest) for m in crosses]
    elif req.members:
        entries = [DeliveryEntry(sn=req.sn, gsn=0, group_id=req.group_id,
                                 batch_digest=req.batch_digest)]
    else:
        entries = []
    # Guardado no item: se ficar bloqueado agora, tick.py checa depois (quando outro commit
    # do grupo rodar _try_deliver de novo) se todas as entradas foram liberadas — espelha o
    # BufferCommit/drainBuffer real.
    req._delivery_entries = entries
    for entry in entries:
        st.delivery.register_commit(entry)
    next_expected = st.delivery.get_next_expected_gsn(req.group_id)
    blocked = st.delivery.get_blocked_entries(req.group_id)
    is_blocked = any(e.gsn > 0 and not e.delivered for e in entries)

    st.phase = Phase.ADELIVER
    if is_blocked:
        st.log_event(Phase.ADELIVER, "ADeliver BLOQUEADO",
                     f"G{req.group_id} SN={req.sn}: GSN {[e.gsn for e in entries]} no batch\n"
                     f"Esperando GSN={next_expected} ser entregue primeiro\n"
                     f"Bloqueados: {[e.gsn for e in blocked]}",
                     "red")
        st.info_text = (
            f"🔒 Entrega BLOQUEADA — esperando a vez\n\n"
            f"Este batch tem uma cross-op (GSN {[e.gsn for e in entries if e.gsn]}) que não\n"
            f"pode ser entregue ainda: o GSN {next_expected} anterior ainda\n"
            f"não foi processado neste grupo.\n\n"
            f"O batch INTEIRO espera (BufferCommit), inclusive os\n"
            f"pedidos single-group que estão nele."
        )
    else:
        st.log_event(Phase.ADELIVER, "ADeliver OK",
                     f"G{req.group_id} SN={req.sn}: "
                     + (f"GSN {[e.gsn for e in entries if e.gsn]} entregue(s)" if crosses
                        else "single-group -> entrega imediata")
                     + f"\nProximo GSN esperado: {next_expected}",
                     "green")
        st.info_text = (
            f"🔓 Entrega confirmada!\n\n"
            f"{'Cross-op(s) entregue(s) na ordem global correta.' if crosses else 'Pedidos simples (1 grupo) → entregues imediatamente.'}\n\n"
            f"O sistema garante que pedidos que afetam múltiplos\n"
            f"grupos sejam entregues na mesma ordem em todos os grupos."
        )

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
    """Instância completa (o próximo SN do grupo nasce em seguida)."""
    req = st.current_request
    st.phase = Phase.DONE
    st.log_event(Phase.DONE, "Instância completa",
                 f"G{req.group_id} SN={req.sn} | {req.batch_requests} pedido(s) | Committed: {st.committed}",
                 "green")
    st.info_text = (
        f"✨ Instância G{req.group_id} SN={req.sn} concluída\n\n"
        f"O grupo abre a próxima posição do log (SN "
        f"{st.epoch_mgr.next_sn_for(req.group_id)}) e refaz\n"
        f"PREPARE → PROMISE antes de cortar o próximo batch.\n\n"
        f"Total de decisões: {st.committed}"
    )


