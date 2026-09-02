"""Tick — Loop principal que reflete o fluxo real do mirbft.

Fluxo real (multipaxosinstance.go):
  1. SetMembers → líder envia PREPARE (setup do segmento, uma vez)
  2. Membros respondem PROMISE → quorum → prepared=true
  3. Clientes enviam requests → proxy encaminha → acumula nos buckets
  4. ProposeIfDue → líder faz CutBatch → envia ACCEPT
  5. Membros respondem ACCEPTED → quorum → COMMIT
  6. deliverCommit → NotifyProxy → responde ao cliente

Múltiplas instâncias Paxos rodam em paralelo (uma por grupo de dados).
Cada grupo tem seu próprio líder e segmento independente.
"""

import random

from mirbftview.protocol.types import (
    Phase, MsgType, Message, RequestInfo, snapshot_client_request, key_to_group,
)
from mirbftview.protocol.delivery import DeliveryEntry
from mirbftview.engine.state import SimState
from mirbftview.engine import phases


# Sequência de fases refletindo o fluxo real:
# PREPARE/PROMISE = setup do segmento (SetMembers)
# CLIENT_SEND = requests chegam ao sistema
# BUCKET_ASSIGN = request entra no bucket (visível: leigo vê o balde enchendo)
# ACCEPT/ACCEPTED/COMMIT = proposta de batch (ProposeIfDue, CutBatch interno)
# Bootstrap: só o líder se anunciando ao grupo (SetMembers/PREPARE/PROMISE),
# sem nenhum cliente envolvido — termina em DONE sem passar por CLIENT_SEND.
BOOTSTRAP_SEQUENCE = [
    Phase.PREPARE,
    Phase.PROMISE,
    Phase.DONE,
]

# Sequência STEADY-STATE (após primeiro ciclo: prepared=true, pula PREPARE/PROMISE)
# Ordem real (multipaxosinstance.go:420-444): deliverCommit checa ADeliver
# ANTES de chamar announce() — e é announce() que dispara o COMMIT_NOTIFY.
# Se bloqueado, announce()/COMMIT_NOTIFY nem chegam a acontecer naquele
# momento (fica em BufferCommit ate o drainBuffer liberar depois).
STEADY_STATE_SEQUENCE = [
    Phase.CLIENT_SEND,
    Phase.BUCKET_ASSIGN,
    Phase.ACCEPT,
    Phase.ACCEPTED,
    Phase.COMMIT,
    Phase.ADELIVER,
    Phase.COMMIT_NOTIFY,
    Phase.DONE,
]

STEADY_STATE_CROSS_SEQUENCE = [
    Phase.CLIENT_SEND,
    Phase.BUCKET_ASSIGN,
    Phase.GSN_ASSIGN,
    Phase.ACCEPT,
    Phase.ACCEPTED,
    Phase.COMMIT,
    Phase.ADELIVER,
    Phase.COMMIT_NOTIFY,
    Phase.DONE,
]

RETRANSMIT_CHANCE = 0.15


def tick(st: SimState):
    """Chamado a cada frame (~16ms). Avança mensagens e lógica."""
    if st.paused and not st.advance_flag:
        return
    st.advance_flag = False

    # Avança TODAS as mensagens em trânsito
    speed = st.speed * 0.02
    all_arrived = True
    for msg in st.messages:
        if msg.progress < 1.0:
            msg.progress = min(1.0, msg.progress + speed)
            all_arrived = False

    if not all_arrived:
        return

    # Mensagens chegaram — avança lógica
    st.messages.clear()

    # Se não há requests ativas, spawna um lote
    if not st.active_requests and st.phase == Phase.IDLE:
        return
    if not st.active_requests and st.phase == Phase.DONE and not st.step_mode:
        _spawn_batch(st)
        _update_global_phase(st, [])
        _decay_visual_events(st)
        return
    if not st.active_requests:
        return

    # Avança TODAS as requests ativas para a próxima fase (em paralelo)
    new_messages: list[Message] = []
    completed = []

    for req in list(st.active_requests):
        st.current_request = req
        _advance_request(st, req)
        # Todas as mensagens desta request herdam a cor e o snapshot da struct
        snap = snapshot_client_request(req)
        for msg in st.messages:
            if not msg.color:
                msg.color = req.color
            msg.req_snapshot = snap
        new_messages.extend(st.messages)
        st.messages = []

        if req.phase == Phase.DONE:
            completed.append(req)

    # Remove requests completadas
    for req in completed:
        st.active_requests.remove(req)

    # Spawn novo lote quando todas completaram (modo contínuo)
    if not st.step_mode and not st.active_requests:
        _spawn_batch(st)
        new_messages.extend(st.messages)
        st.messages = []

    st.messages = new_messages
    _update_global_phase(st, completed)
    _decay_visual_events(st)


def _update_global_phase(st: SimState, completed: list[RequestInfo]):
    """Atualiza phase e current_request globais para a UI."""
    if st.active_requests:
        st.phase = st.active_requests[0].phase
        st.current_request = st.active_requests[0]
    elif completed:
        st.phase = Phase.DONE
        st.current_request = completed[-1]


def _spawn_batch(st: SimState):
    """Spawna instâncias Paxos paralelas — uma por grupo de dados.

    No ISS real, SetMembers é chamado no início do segmento.
    O líder de cada grupo envia PREPARE imediatamente (sem esperar requests).
    No steady-state (prepared=true), pula direto para CLIENT_SEND.
    """
    from mirbftview.qt.canvas._constants import MSG_COLOR_POOL

    data_groups = [g for g in st.groups if g.id != 0]

    if not st.prepared:
        _spawn_bootstrap(st, data_groups, MSG_COLOR_POOL)
        return

    # "single_request" só limita QUANTAS requisições novas nascem por vez
    # (1, em vez de uma por grupo em paralelo) — qual grupo cada uma toca
    # não é mais escolhido aqui: é consequência do hash real da chave
    # (key_to_group), exatamente como no cliente real (createPayload).
    n_slots = 1 if st.scenarios.get('single_request', False) else len(data_groups)

    for i in range(n_slots):
        color = MSG_COLOR_POOL[i % len(MSG_COLOR_POOL)]
        req = _generate_client_request(st, color)
        st.active_requests.append(req)

    # Executa CLIENT_SEND para todas as requests recem-criadas
    new_messages: list[Message] = []
    for req in st.active_requests:
        st.current_request = req
        _do_client_send(st, req)
        snap = snapshot_client_request(req)
        for msg in st.messages:
            if not msg.color:
                msg.color = req.color
            msg.req_snapshot = snap
        new_messages.extend(st.messages)
        st.messages = []

    st.messages = new_messages
    st.phase = Phase.CLIENT_SEND
    if st.active_requests:
        st.current_request = st.active_requests[0]


def _spawn_bootstrap(st: SimState, data_groups, color_pool):
    """Bootstrap real: cada grupo se prepara (SetMembers/PREPARE/PROMISE) de
    uma so vez, em paralelo, ANTES de qualquer requisicao de cliente existir.
    Nenhum cliente participa disso, por isso sempre roda em paralelo, mesmo
    com o cenario "single_request" ligado (que so restringe requisicoes
    reais de cliente, geradas depois deste bootstrap).
    """
    new_messages: list[Message] = []
    for i, group in enumerate(data_groups):
        quorum = len(group.members) // 2 + 1
        sn = st.epoch_mgr.next_sn_for(group.id)
        leader = st.epoch_mgr.leader_for_group(group.id, sn)

        req = RequestInfo(
            group_id=group.id,
            sn=sn,
            leader=leader,
            ballot=0,
            quorum=quorum,
            phase=Phase.PREPARE,
        )
        req.color = color_pool[i % len(color_pool)]
        req._is_bootstrap = True
        st.active_requests.append(req)

        st.current_request = req
        phases.phase_prepare(st)
        for msg in st.messages:
            if not msg.color:
                msg.color = req.color
        new_messages.extend(st.messages)
        st.messages = []

    st.messages = new_messages
    st.phase = Phase.PREPARE
    if st.active_requests:
        st.current_request = st.active_requests[0]

    st.info_text = (
        "\u270b SETUP do Segmento (primeira vez)\n\n"
        "No MultiPaxos, o l\u00edder precisa fazer PREPARE/PROMISE\n"
        "UMA VEZ no in\u00edcio do segmento (SetMembers), antes de\n"
        "qualquer requisicao de cliente existir. Sempre roda em\n"
        "paralelo para todos os grupos, mesmo com o modo\n"
        "'uma mensagem por vez' ligado - nenhum cliente esta\n"
        "envolvido aqui ainda.\n\n"
        "Depois disso, entra em STEADY STATE:\n"
        "pula direto para ACCEPT (ProposeIfDue)."
    )


def _advance_request(st: SimState, req: RequestInfo):
    """Avança uma request individual para sua próxima fase."""
    # Sequência é determinada pelo ciclo em que a request nasceu:
    # bootstrap (PREPARE/PROMISE, sem cliente) usa a sequência truncada;
    # qualquer requisição real de cliente usa sempre a sequência steady-state,
    # mesmo a primeira — PREPARE/PROMISE já foram feitos separadamente no
    # bootstrap antes de qualquer requisição de cliente existir.
    if getattr(req, '_is_bootstrap', False):
        seq = BOOTSTRAP_SEQUENCE
    else:
        seq = STEADY_STATE_CROSS_SEQUENCE if req.is_cross_group else STEADY_STATE_SEQUENCE

    current = req.phase
    try:
        idx = seq.index(current)
    except ValueError:
        _handle_special(st, req)
        return

    next_idx = idx + 1
    if next_idx >= len(seq):
        return

    next_phase = seq[next_idx]

    # Quando transiciona de BUCKET_ASSIGN para ACCEPT (ou GSN_ASSIGN),
    # verifica se o batch está pronto (acumulou requests ou timeout).
    # Simula waitForRequestsLocked do BucketGroup.CutBatch real.
    if current == Phase.BUCKET_ASSIGN and next_phase in (Phase.ACCEPT, Phase.GSN_ASSIGN):
        bucket_id = req.bucket_id
        fill = st.batch_fill.get(bucket_id, 0)
        is_cross = req.is_cross_group

        # Cross-ops cortam imediatamente (RequestAddedCrossOp no Go)
        if is_cross:
            st.batch_ready[bucket_id] = True

        # Single-group: espera acumular batch_visual_size ou timeout
        if not is_cross and not st.batch_ready.get(bucket_id, False):
            # Incrementa timeout counter
            st.batch_timeout_counter[bucket_id] = st.batch_timeout_counter.get(bucket_id, 0) + 1
            timeout_hit = st.batch_timeout_counter[bucket_id] >= st.batch_timeout_limit
            size_hit = fill >= st.batch_visual_size

            if size_hit or timeout_hit:
                st.batch_ready[bucket_id] = True
                reason = "size" if size_hit else "timeout"
                st.bucket_bubbles[bucket_id] = {
                    "text": f"CutBatch()\ntrigger={reason}\nreqs={fill}/{st.batch_visual_size}",
                    "color": "green", "ttl": 30,
                }
            else:
                # Não está pronto: bubble de espera e bloqueia avanço
                st.bucket_bubbles[bucket_id] = {
                    "text": f"waitForRequests()\n{fill}/{st.batch_visual_size} | timeout={st.batch_timeout_counter[bucket_id]}/{st.batch_timeout_limit}",
                    "color": "gold", "ttl": 10,
                }
                # Mantém na fase BUCKET_ASSIGN (não avança)
                req.phase = Phase.BUCKET_ASSIGN
                st.messages = []
                return

        # Batch pronto: executa o corte
        st._suppress_log = True
        phases.phase_batch_cut(st)
        st.messages.clear()
        st._suppress_log = False
        # Reset counters para este bucket
        st.batch_timeout_counter[bucket_id] = 0
        st.batch_ready[bucket_id] = False

    # Cenário: batch_resurrect
    if next_phase == Phase.ACCEPT and st.scenarios.get('batch_resurrect', False):
        if not getattr(req, '_resurrect_done', False) and random.random() < 0.2:
            req._resurrect_done = True
            bucket_id = req.bucket_id
            label = f"sn={req.client_sn} h={req.payload_hash[:8]}"
            if bucket_id < len(st.bucket_contents):
                st.bucket_contents[bucket_id].append(label)
            st.batch_fill[bucket_id] = st.batch_fill.get(bucket_id, 0) + 1
            st.phase = Phase.ACCEPT
            st.log_event(Phase.ACCEPT, "Batch RESURRECT",
                         f"Instancia desistiu do quorum (Eventual Progress)\n"
                         f"Request volta ao Bucket {bucket_id}\n"
                         f"Aguardando novo batch cut",
                         "orange")
            st.info_text = (
                f"Batch INVALIDADO!\n\n"
                f"A instancia de consenso desistiu de esperar\n"
                f"quorum para esta posicao do log (mecanismo real\n"
                f"de Eventual Progress) e commitou um valor nulo.\n\n"
                f"Requisicoes que ja tinham sido cortadas em lote\n"
                f"mas nao decididas voltam a fila do bucket {bucket_id}\n"
                f"para serem re-propostas depois."
            )
            st.messages = [Message(
                MsgType.CLIENT_REQUEST, req.leader, req.leader,
                label=f"RESURRECT B{bucket_id}",
                detail="batch invalidated",
            )]
            return

    # Cenário: timeout/retransmissão
    if next_phase == Phase.ACCEPTED and not req._retransmit_done:
        if st.scenarios.get('timeout', False) and random.random() < RETRANSMIT_CHANCE:
            req._retransmit_done = True
            phases.phase_retransmit(st)
            req.phase = Phase.RETRANSMIT
            return

    # Cenário: falha de nó
    if next_phase == Phase.ACCEPT and st.scenarios.get('node_failure', False):
        if not req._failure_done and random.random() < 0.12:
            req._failure_done = True
            _trigger_node_failure(st, req)
            return

    # Cenário: adeliver_block
    if next_phase == Phase.ADELIVER and st.scenarios.get('adeliver_block', False):
        if req.is_cross_group and not getattr(req, '_adeliver_blocked_done', False):
            req._adeliver_blocked_done = True
            st.phase = Phase.ADELIVER
            st.log_event(Phase.ADELIVER, "ADeliver BLOQUEADO (cenario)",
                         f"GSN={req.gsn} bloqueado artificialmente\n"
                         f"Simulando espera por GSN anterior",
                         "red")
            st.info_text = (
                f"ADeliver BLOQUEADO (cenario ativo)\n\n"
                f"GSN={req.gsn} precisa esperar GSN anterior.\n"
                f"Na proxima tentativa sera liberado."
            )
            st.messages = [Message(
                MsgType.COMMIT, req.leader, req.leader,
                label=f"ADeliver BLOQ gsn={req.gsn}",
                detail="waiting previous GSN",
            )]
            req.phase = Phase.ADELIVER
            return

    # Checkpoint antes de DONE
    if next_phase == Phase.DONE:
        if _needs_checkpoint(st):
            phases.phase_checkpoint(st)
            req.phase = Phase.CHECKPOINT
            return

    # ADeliver bloqueou de verdade (BufferCommit real, orderer/multipaxos
    # instance.go:432-436): a request sai do pipeline ativo e so volta
    # quando outro commit do mesmo grupo liberar o GSN anterior — nao avanca
    # para COMMIT_NOTIFY (announce()) enquanto isso nao acontece.
    if current == Phase.ADELIVER and next_phase == Phase.COMMIT_NOTIFY:
        entries = getattr(req, '_delivery_entries', None)
        if entries and not all(e.delivered for e in entries):
            st.active_requests.remove(req)
            for gid in {e.group_id for e in entries}:
                st.blocked_requests.setdefault(gid, []).append(req)
            return

    # Executa fase
    _execute_phase(st, req, next_phase)
    req.phase = next_phase

    # Depois de rodar ADeliver, tenta liberar requests de outros commits do
    # mesmo grupo que ficaram bloqueadas antes: o _try_deliver interno pode
    # ter destravado varias entradas pendentes de uma vez (drainBuffer real).
    if next_phase == Phase.ADELIVER:
        _release_unblocked(st, req.group_id)

    # Marca prepared=true após PROMISE (steady-state a partir do próximo ciclo)
    if next_phase == Phase.PROMISE and not st.prepared:
        st.prepared = True


def _release_unblocked(st: SimState, group_id: int):
    """Libera requests bloqueadas pelo ADeliver deste grupo cujas entradas
    (uma por grupo tocado) acabaram de ser TODAS entregues (drainBuffer
    real). Uma request cross-group fica registrada em blocked_requests de
    cada grupo que ela toca; só é liberada (e removida de todos eles) quando
    todas as suas entradas estiverem entregues. Continua a partir de
    ADELIVER (já executado) — o próximo tick avança para COMMIT_NOTIFY."""
    blocked = st.blocked_requests.get(group_id)
    if not blocked:
        return
    still_blocked = []
    released = []
    for req in blocked:
        entries = getattr(req, '_delivery_entries', None)
        if entries and all(e.delivered for e in entries):
            released.append(req)
        else:
            still_blocked.append(req)
    if still_blocked:
        st.blocked_requests[group_id] = still_blocked
    else:
        st.blocked_requests.pop(group_id, None)

    for req in released:
        # Remove de qualquer outro grupo em que também estivesse pendurada.
        for gid, lst in list(st.blocked_requests.items()):
            if req in lst:
                lst.remove(req)
                if not lst:
                    st.blocked_requests.pop(gid, None)
        st.log_event(Phase.ADELIVER, "ADeliver liberado",
                     f"GSN={req.gsn} finalmente entregue\n"
                     f"Request retomada para COMMIT_NOTIFY",
                     "green")
        st.active_requests.append(req)


def _execute_phase(st: SimState, req: RequestInfo, phase: Phase):
    """Executa a fase especificada."""
    if phase == Phase.CLIENT_SEND:
        _do_client_send(st, req)
    elif phase == Phase.BUCKET_ASSIGN:
        phases.phase_bucket_assign(st)
    elif phase == Phase.GSN_ASSIGN:
        phases.phase_gsn_assign(st)
    elif phase == Phase.PROMISE:
        phases.phase_promise(st)
    elif phase == Phase.ACCEPT:
        phases.phase_accept(st)
    elif phase == Phase.ACCEPTED:
        phases.phase_accepted(st)
    elif phase == Phase.COMMIT:
        phases.phase_commit(st)
    elif phase == Phase.COMMIT_NOTIFY:
        phases.phase_commit_notify(st)
    elif phase == Phase.ADELIVER:
        phases.phase_adeliver(st)
    elif phase == Phase.DONE:
        phases.phase_done(st)


def _generate_client_request(st: SimState, color: str) -> RequestInfo:
    """Gera uma request de cliente nova, fiel ao cliente real
    (cmd/orderingclient/client.go: createPayload):

      - cada cliente mantém seu PRÓPRIO contador sequencial (seqNr), não um
        contador global compartilhado;
      - a chave é sempre K{seqNr:08d} (sequencial, não aleatória);
      - TX vs GET é decidido deterministicamente por seqNr%100 < CrossOpRatio
        (não por sorteio); a segunda chave de uma TX é sempre seqNr+1000;
      - o GRUPO é uma CONSEQUÊNCIA do hash real da chave (key_to_group,
        mesmo CRC32%numDataGroups de keyToGroup em request.go) — não é mais
        escolhido antes e "encaixado" numa chave depois.

    Se as duas chaves de uma TX colidirem no mesmo grupo (hash), a operação
    vira single-group de verdade, exatamente como a deduplicação real de
    TouchedGroups faria (ver "Quando duas chaves colidem no mesmo grupo").
    """
    data_groups = [g for g in st.groups if g.id != 0]
    num_data_groups = len(data_groups)

    client = random.choice(st.clients)
    seq_nr = st.client_seq.get(client.id, 0)
    st.client_seq[client.id] = seq_nr + 1

    cross_ratio_pct = round(st.cross_op_pct * 100)
    k1 = f"K{seq_nr:08d}"
    if (seq_nr % 100) < cross_ratio_pct:
        k2 = f"K{seq_nr + 1000:08d}"
        payload = f"TX {k1},{k2}"
        touched = sorted({key_to_group(k1, num_data_groups), key_to_group(k2, num_data_groups)})
    else:
        payload = f"GET {k1}"
        touched = [key_to_group(k1, num_data_groups)]

    is_cross = len(touched) > 1
    group_id = touched[0]
    group = st.groups[group_id]
    quorum = len(group.members) // 2 + 1
    sn = st.epoch_mgr.next_sn_for(group_id)
    leader = st.epoch_mgr.leader_for_group(group_id, sn)

    req = RequestInfo(
        client_id=client.id,
        client_sn=seq_nr,
        payload=payload,
        group_id=group_id,
        sn=sn,
        leader=leader,
        ballot=0,
        quorum=quorum,
        is_cross_group=is_cross,
        touched_groups=touched,
        phase=Phase.CLIENT_SEND,
        color=color,
    )
    if is_cross:
        st.gsn += 1
        req.gsn = st.gsn
    return req


def _do_client_send(st: SimState, req: RequestInfo):
    """Log/mensagem visual do envio — a request já chega aqui totalmente
    gerada por _generate_client_request (cliente, seqNr, payload, grupo,
    líder, SN)."""
    import hashlib

    client = st.clients[req.client_id] if req.client_id < len(st.clients) else st.clients[0]
    payload_hash = hashlib.sha256(req.payload.encode()).hexdigest()[:16]
    req.payload_hash = payload_hash

    # Bucket assignment (interno) — (ClientId+ClientSn) mod numBuckets, com
    # ClientSn = seqNr real deste cliente (GetBucketNr real em request.go).
    req.bucket_id = st.epoch_mgr.get_request_bucket(client.id, req.client_sn)

    group = st.groups[req.group_id]
    req.proxy_node = random.choice(group.members)
    st.proxy.register_request(client.id, req.client_sn, req.proxy_node)

    st.phase = Phase.CLIENT_SEND
    st.log_event(Phase.CLIENT_SEND, "Cliente envia request",
                 f"{client.name} -> Node {req.proxy_node} (proxy)\n"
                 f"Payload: \"{req.payload}\" | Hash: {payload_hash[:8]}\n"
                 f"Grupo: G{req.group_id} | Lider: Node {req.leader}",
                 "orange")

    st.info_text = (
        f"\U0001f536 Cliente envia request ao sistema\n\n"
        f"{client.name} envia para Node {req.proxy_node} (proxy).\n"
        f"O proxy encaminha ao grupo G{req.group_id}.\n\n"
        f"O segmento ja esta PREPARADO (PREPARE/PROMISE\n"
        f"ja foram feitos). Quando o batch estiver pronto,\n"
        f"o lider (Node {req.leader}) vai propor via ACCEPT.\n\n"
        f"\u2500\u2500\u2500 Detalhes \u2500\u2500\u2500\n"
        f"Payload: \"{req.payload}\" | Hash: {payload_hash[:8]}\n"
        f"Grupo G{req.group_id} membros={group.members}"
    )

    st.messages = [Message(
        MsgType.CLIENT_REQUEST, client.id, req.proxy_node,
        from_is_client=True,
        label=f"REQ h={payload_hash[:8]}",
        sn=req.sn,
        batch_digest="",
    )]


def _force_close_delivery(st: SimState, req: RequestInfo):
    """Fecha a posição de entrega desta request em cada grupo tocado, mesmo
    sem ter passado por ADeliver de verdade — usado quando a request é
    interrompida antes disso (ex.: falha de nó). Evita travar para sempre a
    fila de outro grupo esperando por este GSN, e libera qualquer request
    que já estivesse bloqueada atrás dele."""
    if not req.is_cross_group or req.gsn <= 0:
        return
    groups = req.touched_groups if req.touched_groups else [req.group_id]
    touched = set()
    for gid in groups:
        entry = DeliveryEntry(sn=req.sn, gsn=req.gsn, group_id=gid, batch_digest=req.batch_digest)
        st.delivery.register_commit(entry)
        touched.add(gid)
    for gid in touched:
        _release_unblocked(st, gid)


def _trigger_node_failure(st: SimState, req: RequestInfo):
    """Simula falha de nó e inicia view change."""
    old_leader = req.leader
    all_ids = [n.id for n in st.nodes]
    old_idx = all_ids.index(old_leader) if old_leader in all_ids else 0
    new_leader = all_ids[(old_idx + 1) % len(all_ids)]
    new_ballot = req.ballot + len(all_ids)
    phases.phase_view_change(st, old_leader, new_leader, new_ballot)
    req.phase = Phase.VIEW_CHANGE


def _handle_special(st: SimState, req: RequestInfo):
    """Trata fases especiais para uma request."""
    if req.phase == Phase.CHECKPOINT:
        # Checkpoint no MultiPaxosMulticastOrderer só trunca o log — não troca
        # líder nem redistribui buckets (grupos são estáticos), então segue
        # direto para DONE, sem uma fase de transição de época.
        phases.phase_done(st)
        req.phase = Phase.DONE
    elif req.phase == Phase.VIEW_CHANGE:
        # Falha de nó pode ter interrompido esta request ANTES do ADeliver.
        # Se ela já tinha GSN alocado (passou por GSN_ASSIGN/register_touch),
        # sua posição na fila de entrega de cada grupo tocado ficaria
        # travada para sempre sem isto — Eventual Progress real garante que
        # toda posição do log eventualmente avança (mesmo que para ⊥)
        # justamente para isso não acontecer.
        _force_close_delivery(st, req)
        phases.phase_done(st)
        req.phase = Phase.DONE
    elif req.phase == Phase.RETRANSMIT:
        phases.phase_accepted(st)
        req.phase = Phase.ACCEPTED
    else:
        phases.phase_done(st)
        req.phase = Phase.DONE


def _needs_checkpoint(st: SimState) -> bool:
    """Verifica se é hora de fazer checkpoint."""
    if st.scenarios.get('checkpoint_force', False) and st.committed > 0:
        return st.last_checkpoint_sn != st.current_request.sn
    return (
        st.committed > 0
        and st.committed % st.checkpoint_interval == 0
        and st.last_checkpoint_sn != st.current_request.sn
    )


def _decay_visual_events(st: SimState):
    """Decrementa TTL dos visual events e bucket bubbles, remove expirados."""
    for ev in st.visual_events:
        ev["ttl"] -= 1
    st.visual_events = [ev for ev in st.visual_events if ev["ttl"] > 0]
    if len(st.visual_events) > 12:
        st.visual_events = st.visual_events[-12:]

    # Decay bucket thought bubbles
    expired = [bid for bid, b in st.bucket_bubbles.items() if b["ttl"] <= 0]
    for bid in expired:
        del st.bucket_bubbles[bid]
    for bid in st.bucket_bubbles:
        st.bucket_bubbles[bid]["ttl"] -= 1
