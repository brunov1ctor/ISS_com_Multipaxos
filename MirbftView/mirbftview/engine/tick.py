"""Tick — Loop principal: avança mensagens e transiciona fases (pipeline)."""

import random

from mirbftview.protocol.types import Phase, MsgType, Message, RequestInfo
from mirbftview.engine.state import SimState
from mirbftview.engine import phases


# Sequência de fases para uma request normal (single-group)
SINGLE_GROUP_SEQUENCE = [
    Phase.CLIENT_SEND,
    Phase.BUCKET_ASSIGN,
    Phase.BATCH_CUT,
    Phase.PREPARE,
    Phase.PROMISE,
    Phase.ACCEPT,
    Phase.ACCEPTED,
    Phase.COMMIT,
    Phase.COMMIT_NOTIFY,
    Phase.ADELIVER,
    Phase.DONE,
]

# Sequência para cross-group (inclui GSN)
CROSS_GROUP_SEQUENCE = [
    Phase.CLIENT_SEND,
    Phase.BUCKET_ASSIGN,
    Phase.BATCH_CUT,
    Phase.GSN_ASSIGN,
    Phase.PREPARE,
    Phase.PROMISE,
    Phase.ACCEPT,
    Phase.ACCEPTED,
    Phase.COMMIT,
    Phase.COMMIT_NOTIFY,
    Phase.ADELIVER,
    Phase.DONE,
]

RETRANSMIT_CHANCE = 0.15
MAX_ACTIVE_REQUESTS = 12  # limite de segurança


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

    # Todas as mensagens chegaram — avança lógica
    st.messages.clear()

    # Pipeline: se não há requests ativas, inicia
    if not st.active_requests and st.phase == Phase.IDLE:
        return
    if not st.active_requests and st.phase == Phase.DONE and not st.step_mode:
        _spawn_requests(st)
        return
    if not st.active_requests:
        return

    # Avança cada request ativa para sua próxima fase
    new_messages: list[Message] = []
    completed = []

    for req in list(st.active_requests):
        st.current_request = req
        _advance_request(st, req)
        # Coleta mensagens geradas (phases escrevem em st.messages)
        new_messages.extend(st.messages)
        st.messages = []

        # Quando uma request corta o batch, as outras que estavam
        # esperando tambem avançam (batch agrupa multiplas requests)
        if req.phase == Phase.BATCH_CUT:
            for peer in list(st.active_requests):
                if (peer is not req
                        and peer.phase == Phase.BUCKET_ASSIGN):
                    peer.batch_digest = req.batch_digest
                    peer.batch_requests = req.batch_requests
                    peer.phase = Phase.BATCH_CUT
                    # Reset fill do bucket do peer
                    st.batch_fill[peer.bucket_id] = 0

        if req.phase == Phase.DONE:
            completed.append(req)

    # Remove requests completadas
    for req in completed:
        st.active_requests.remove(req)

    # Spawn novas requests para manter pipeline cheio (se modo contínuo)
    # Garante que haja requests suficientes para preencher batches
    if not st.step_mode:
        waiting = sum(1 for r in st.active_requests
                      if r.phase == Phase.BUCKET_ASSIGN)
        need = st.batch_visual_size - waiting
        spawned = 0
        while spawned < need and len(st.active_requests) < MAX_ACTIVE_REQUESTS:
            _spawn_one(st)
            new_messages.extend(st.messages)
            st.messages = []
            spawned += 1

    # Atualiza estado global
    st.messages = new_messages
    _update_global_phase(st, completed)
    _update_info(st)
    _decay_visual_events(st)


def _update_global_phase(st: SimState, completed: list[RequestInfo]):
    """Atualiza phase e current_request globais para a UI."""
    if st.active_requests:
        st.phase = st.active_requests[0].phase
        st.current_request = st.active_requests[0]
    elif completed:
        st.phase = Phase.DONE
        st.current_request = completed[-1]


def _spawn_requests(st: SimState):
    """Inicia o pipeline com requests suficientes para preencher batches."""
    count = max(st.pipeline_size, st.batch_visual_size)
    for _ in range(count):
        _spawn_one(st)


def _spawn_one(st: SimState):
    """Cria uma nova request e a coloca na fase CLIENT_SEND."""
    st.current_request = None
    phases.phase_client_send(st)
    if st.current_request:
        st.current_request.phase = Phase.CLIENT_SEND
        st.active_requests.append(st.current_request)


def _advance_request(st: SimState, req: RequestInfo):
    """Avança uma request individual para sua próxima fase."""
    seq = CROSS_GROUP_SEQUENCE if req.is_cross_group else SINGLE_GROUP_SEQUENCE

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

    # Comportamento real: batch só é cortado quando acumula requests suficientes
    # Na visualização, usamos contagem global de requests em BUCKET_ASSIGN
    if next_phase == Phase.BATCH_CUT:
        waiting = sum(1 for r in st.active_requests
                      if r.phase == Phase.BUCKET_ASSIGN)
        if waiting < st.batch_visual_size:
            return  # aguarda mais requests acumularem

    # Cenário: batch_resurrect — batch invalidado, requests voltam ao bucket
    if next_phase == Phase.PREPARE and st.scenarios.get('batch_resurrect', False):
        if not getattr(req, '_resurrect_done', False) and random.random() < 0.2:
            req._resurrect_done = True
            # Requests voltam ao bucket
            bucket_id = req.bucket_id
            label = f"sn={req.client_sn} h={req.payload_hash[:8]}"
            if bucket_id < len(st.bucket_contents):
                st.bucket_contents[bucket_id].append(label)
            st.batch_fill[bucket_id] = st.batch_fill.get(bucket_id, 0) + 1
            st.phase = Phase.BUCKET_ASSIGN
            st.log_event(Phase.BUCKET_ASSIGN, "Batch RESURRECT",
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
            req.phase = Phase.BUCKET_ASSIGN
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

    # Cenário: adeliver_block — força bloqueio artificial
    if next_phase == Phase.ADELIVER and st.scenarios.get('adeliver_block', False):
        if req.is_cross_group and not getattr(req, '_adeliver_blocked_done', False):
            req._adeliver_blocked_done = True
            # Simula: GSN anterior ainda nao chegou, bloqueia esta entrega
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
    _execute_phase(st, next_phase)
    req.phase = next_phase


def _trigger_node_failure(st: SimState, req: RequestInfo):
    """Simula falha de nó e inicia view change."""
    old_leader = req.leader
    all_ids = [n.id for n in st.nodes]
    old_idx = all_ids.index(old_leader) if old_leader in all_ids else 0
    new_leader = all_ids[(old_idx + 1) % len(all_ids)]
    new_ballot = req.ballot + len(all_ids)
    phases.phase_view_change(st, old_leader, new_leader, new_ballot)
    req.phase = Phase.VIEW_CHANGE


_PHASE_DISPATCH = {
    Phase.CLIENT_SEND: phases.phase_client_send,
    Phase.BUCKET_ASSIGN: phases.phase_bucket_assign,
    Phase.BATCH_CUT: phases.phase_batch_cut,
    Phase.GSN_ASSIGN: phases.phase_gsn_assign,
    Phase.PREPARE: phases.phase_prepare,
    Phase.PROMISE: phases.phase_promise,
    Phase.ACCEPT: phases.phase_accept,
    Phase.ACCEPTED: phases.phase_accepted,
    Phase.COMMIT: phases.phase_commit,
    Phase.COMMIT_NOTIFY: phases.phase_commit_notify,
    Phase.ADELIVER: phases.phase_adeliver,
    Phase.DONE: phases.phase_done,
}


def _execute_phase(st: SimState, phase: Phase):
    """Executa a fase especificada."""
    fn = _PHASE_DISPATCH.get(phase)
    if fn:
        fn(st)


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


def _update_info(st: SimState):
    """Atualiza info_text com resumo do pipeline."""
    if not st.active_requests:
        return
    n = len(st.active_requests)
    if n <= 1:
        return
    phase_counts: dict[str, int] = {}
    for req in st.active_requests:
        name = req.phase.name
        phase_counts[name] = phase_counts.get(name, 0) + 1
    summary = " | ".join(f"{k}:{v}" for k, v in phase_counts.items())
    st.info_text += f"\n\n-- Pipeline ({n} requests ativas) --\n{summary}"


def _decay_visual_events(st: SimState):
    """Decrementa TTL dos visual events e remove expirados."""
    for ev in st.visual_events:
        ev["ttl"] -= 1
    st.visual_events = [ev for ev in st.visual_events if ev["ttl"] > 0]
    # Limita para evitar acumulo
    if len(st.visual_events) > 12:
        st.visual_events = st.visual_events[-12:]
