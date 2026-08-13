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

from mirbftview.protocol.types import Phase, MsgType, Message, RequestInfo
from mirbftview.engine.state import SimState
from mirbftview.engine import phases


# Sequência de fases refletindo o fluxo real:
# PREPARE/PROMISE = setup do segmento (SetMembers)
# CLIENT_SEND = requests chegam ao sistema
# BUCKET_ASSIGN = request entra no bucket (visível: leigo vê o balde enchendo)
# ACCEPT/ACCEPTED/COMMIT = proposta de batch (ProposeIfDue, CutBatch interno)
SINGLE_GROUP_SEQUENCE = [
    Phase.PREPARE,
    Phase.PROMISE,
    Phase.CLIENT_SEND,
    Phase.BUCKET_ASSIGN,
    Phase.ACCEPT,
    Phase.ACCEPTED,
    Phase.COMMIT,
    Phase.COMMIT_NOTIFY,
    Phase.ADELIVER,
    Phase.DONE,
]

CROSS_GROUP_SEQUENCE = [
    Phase.PREPARE,
    Phase.PROMISE,
    Phase.CLIENT_SEND,
    Phase.BUCKET_ASSIGN,
    Phase.GSN_ASSIGN,
    Phase.ACCEPT,
    Phase.ACCEPTED,
    Phase.COMMIT,
    Phase.COMMIT_NOTIFY,
    Phase.ADELIVER,
    Phase.DONE,
]

# Sequência STEADY-STATE (após primeiro ciclo: prepared=true, pula PREPARE/PROMISE)
STEADY_STATE_SEQUENCE = [
    Phase.CLIENT_SEND,
    Phase.BUCKET_ASSIGN,
    Phase.ACCEPT,
    Phase.ACCEPTED,
    Phase.COMMIT,
    Phase.COMMIT_NOTIFY,
    Phase.ADELIVER,
    Phase.DONE,
]

STEADY_STATE_CROSS_SEQUENCE = [
    Phase.CLIENT_SEND,
    Phase.BUCKET_ASSIGN,
    Phase.GSN_ASSIGN,
    Phase.ACCEPT,
    Phase.ACCEPTED,
    Phase.COMMIT,
    Phase.COMMIT_NOTIFY,
    Phase.ADELIVER,
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
        # Todas as mensagens desta request herdam a cor da request
        for msg in st.messages:
            if not msg.color:
                msg.color = req.color
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
    is_first_cycle = not st.prepared

    for i, group in enumerate(data_groups):
        # Líder do grupo (conforme GetGroupLeaderForSegment)
        leader = group.members[st.committed % len(group.members)]
        quorum = len(group.members) // 2 + 1

        # Segmento do líder
        seg = st.epoch_mgr.get_segment_for_leader(leader)

        # Atribui SN
        if seg and seg.sns:
            sn = seg.sns[min(st.committed % seg.sn_length, seg.sn_length - 1)]
        else:
            sn = st.committed + i

        # Fase inicial depende se é primeiro ciclo ou steady-state
        initial_phase = Phase.PREPARE if is_first_cycle else Phase.CLIENT_SEND

        req = RequestInfo(
            group_id=group.id,
            sn=sn,
            segment_id=seg.seg_id if seg else -1,
            leader=leader,
            ballot=seg.seg_id if seg else i,
            quorum=quorum,
            is_cross_group=random.random() < st.cross_op_pct,
            phase=initial_phase,
        )

        if req.is_cross_group:
            st.gsn += 1
            req.gsn = st.gsn
            req.touched_groups = [group.id, random.choice([g.id for g in data_groups if g.id != group.id])]
        else:
            req.touched_groups = [group.id]

        # Cor única
        req.color = MSG_COLOR_POOL[i % len(MSG_COLOR_POOL)]
        # Marca se esta request é do ciclo de setup (usa sequência com PREPARE/PROMISE)
        req._is_setup = is_first_cycle
        st.active_requests.append(req)

    # Executa a fase inicial para todas
    new_messages: list[Message] = []
    for req in st.active_requests:
        st.current_request = req
        if is_first_cycle:
            phases.phase_prepare(st)
        else:
            _do_client_send(st, req)
        # Mensagens herdam cor da request
        for msg in st.messages:
            if not msg.color:
                msg.color = req.color
        new_messages.extend(st.messages)
        st.messages = []

    st.messages = new_messages
    st.phase = Phase.PREPARE if is_first_cycle else Phase.CLIENT_SEND
    if st.active_requests:
        st.current_request = st.active_requests[0]

    # Marca prepared após primeiro ciclo (PROMISE vai setar)
    if is_first_cycle:
        st.info_text = (
            "\u270b SETUP do Segmento (primeira vez)\n\n"
            "No MultiPaxos, o l\u00edder precisa fazer PREPARE/PROMISE\n"
            "UMA VEZ no in\u00edcio do segmento (SetMembers).\n\n"
            "Depois disso, entra em STEADY STATE:\n"
            "pula direto para ACCEPT (ProposeIfDue).\n\n"
            "Isso \u00e9 a otimiza\u00e7\u00e3o do MultiPaxos vs Paxos b\u00e1sico."
        )


def _advance_request(st: SimState, req: RequestInfo):
    """Avança uma request individual para sua próxima fase."""
    # Sequência é determinada pelo ciclo em que a request nasceu:
    # requests que começaram com PREPARE usam sequência completa
    # requests que começaram com CLIENT_SEND usam steady-state
    if getattr(req, '_is_setup', False):
        seq = CROSS_GROUP_SEQUENCE if req.is_cross_group else SINGLE_GROUP_SEQUENCE
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
                         f"Batch invalidado (lider mudou/epoch)\n"
                         f"Request volta ao Bucket {bucket_id}\n"
                         f"Aguardando novo batch cut",
                         "orange")
            st.info_text = (
                f"Batch INVALIDADO!\n\n"
                f"O batch foi descartado (ex: lider mudou,\n"
                f"epoch transitou, ou conflito detectado).\n\n"
                f"As requests voltam ao bucket {bucket_id}\n"
                f"e serao re-empacotadas pelo novo lider."
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

    # Executa fase
    _execute_phase(st, req, next_phase)
    req.phase = next_phase

    # Marca prepared=true após PROMISE (steady-state a partir do próximo ciclo)
    if next_phase == Phase.PROMISE and not st.prepared:
        st.prepared = True


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


def _do_client_send(st: SimState, req: RequestInfo):
    """Gera a request do cliente APÓS o segmento estar preparado.

    No código real, requests chegam em paralelo ao setup do segmento.
    Aqui simplificamos: após PROMISE (prepared=true), mostramos
    o cliente enviando a request que será proposta no ACCEPT.
    """
    import hashlib
    import string

    client = random.choice(st.clients)
    st.client_sn += 1

    payload = ''.join(random.choices(string.ascii_uppercase + string.digits, k=12))
    payload_hash = hashlib.sha256(payload.encode()).hexdigest()[:16]

    # Preenche dados da request
    req.client_id = client.id
    req.client_sn = st.client_sn
    req.payload = payload
    req.payload_hash = payload_hash

    # Bucket assignment (interno)
    req.bucket_id = st.epoch_mgr.get_request_bucket(client.id, st.client_sn)

    # Proxy = qualquer membro do grupo
    group = st.groups[req.group_id] if req.group_id < len(st.groups) else st.groups[1]
    req.proxy_node = random.choice(group.members)

    # Registra proxy
    st.proxy.register_request(client.id, st.client_sn, req.proxy_node)

    st.phase = Phase.CLIENT_SEND
    st.log_event(Phase.CLIENT_SEND, "Cliente envia request",
                 f"{client.name} -> Node {req.proxy_node} (proxy)\n"
                 f"Payload: \"{payload}\" | Hash: {payload_hash[:8]}\n"
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
        f"Payload: \"{payload}\" | Hash: {payload_hash[:8]}\n"
        f"Grupo G{req.group_id} membros={group.members}"
    )

    st.messages = [Message(
        MsgType.CLIENT_REQUEST, client.id, req.proxy_node,
        from_is_client=True,
        label=f"REQ h={payload_hash[:8]}",
        detail=f"Payload=\"{payload}\"",
        sn=req.sn,
        batch_digest="",
    )]


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
        phases.phase_epoch_transition(st)
        req.phase = Phase.EPOCH_TRANSITION
    elif req.phase == Phase.EPOCH_TRANSITION:
        phases.phase_done(st)
        req.phase = Phase.DONE
    elif req.phase == Phase.VIEW_CHANGE:
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
    if st.scenarios.get('epoch_force', False) and st.committed > 0:
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
