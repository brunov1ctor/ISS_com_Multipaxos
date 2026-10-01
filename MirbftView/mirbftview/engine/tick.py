"""Tick — Loop principal, modelado por INSTÂNCIA de Paxos como no Go.

Duas espécies de item circulam em `st.active_requests`:

  * PEDIDO (kind="request"): CLIENT_SEND -> [GSN_ASSIGN] -> BUCKET_ASSIGN. Depois disso
    ele só ESPERA nos buckets dos membros do grupo (não é mais um item do pipeline).
  * INSTÂNCIA (kind="instance"): uma por grupo de dados, UMA POR VEZ (runSegment):
    PREPARE -> PROMISE -> corte do batch -> ACCEPT -> ACCEPTED -> COMMIT -> ADELIVER ->
    COMMIT_NOTIFY -> DONE. Ao terminar, nasce a instância seguinte (SN + nGrupos) e ela
    refaz PREPARE/PROMISE (SetMembers cria e o líder envia o PREPARE em toda instância).

O corte (ProposeIfDue -> CutBatch) espera, com a instância preparada, até o batch encher
(BatchSize), o BatchTimeout vencer (contador simulado) ou chegar uma cross-op, que acorda o
corte na hora. Pedidos que chegam enquanto a instância do grupo ainda está em consenso
esperam e saem juntos no próximo batch: é assim que o batch com vários pedidos aparece.

Todas as mensagens em voo andam juntas (rodadas): quando todas chegam, cada item avança
uma fase.
"""

import random
import time

from mirbftview.protocol.types import (
    Phase, MsgType, Message, RequestInfo, snapshot_client_request, key_to_group,
    parse_cross_op_group_weights, pick_cross_op_group_count,
)
from mirbftview.engine.state import SimState
from mirbftview.engine import phases


# Pedido de cliente: só percorre o caminho até os buckets.
REQUEST_SEQUENCE = [Phase.CLIENT_SEND, Phase.BUCKET_ASSIGN, Phase.DONE]
# Cross-group: o proxy pede o GSN e publica o META ANTES de repassar as cópias aos grupos.
REQUEST_CROSS_SEQUENCE = [Phase.CLIENT_SEND, Phase.GSN_ASSIGN, Phase.BUCKET_ASSIGN, Phase.DONE]

# Instância de Paxos de um grupo. Ordem real (multipaxosinstance.go): deliverCommit checa
# ADeliver ANTES de chamar announce() — e é announce() que dispara o COMMIT_NOTIFY. Se
# bloqueado, o batch inteiro fica em BufferCommit até o drainBuffer liberar.
INSTANCE_SEQUENCE = [
    Phase.PREPARE,
    Phase.PROMISE,
    Phase.BATCH_CUT,
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
        st._last_wall = None      # na pausa o relógio do timeout congela
        return
    st.advance_flag = False
    st.tick_count += 1
    _advance_batch_timers(st)
    # Envelhece brilhos e balões dos buckets A CADA tick (não só ao fim de cada fase).
    _decay_visual_events(st)

    # Avança TODAS as mensagens em trânsito
    speed = st.speed * 0.02
    all_arrived = True
    for msg in st.messages:
        if msg.progress < 1.0:
            msg.progress = min(1.0, msg.progress + speed)
            all_arrived = False

    if not all_arrived:
        return

    # Ganchos de chegada (cada nó recebe a cópia do pedido no seu bucket, ou remove o
    # batch no commit, só quando a MENSAGEM dele chega).
    for msg in st.messages:
        if msg.on_arrive is not None and not msg.fired:
            msg.fired = True
            msg.on_arrive(st)

    # Mensagens com segundo salto encadeado (ex.: líder -> proxy, depois proxy -> clientes)
    # voam de novo antes de a lógica avançar.
    followups = []
    for msg in st.messages:
        if msg.then is not None:
            nexts = msg.then if isinstance(msg.then, list) else [msg.then]
            for nxt in nexts:
                nxt.color = nxt.color or msg.color
                nxt.req_snapshot = nxt.req_snapshot or msg.req_snapshot
                followups.append(nxt)
    if followups:
        st.messages = followups
        return

    # Mensagens chegaram — avança lógica
    st.messages.clear()

    if not st.instances:
        return  # simulação ainda não iniciada

    st.round_count += 1
    new_messages: list[Message] = []
    completed = []

    for req in list(st.active_requests):
        if req not in st.active_requests:
            continue
        st.current_request = req
        _advance_request(st, req)
        _tag_messages(st, req)
        new_messages.extend(st.messages)
        st.messages = []
        new_messages.extend(st.extra_messages)   # PREPARE do sucessor, se ele nasceu agora
        st.extra_messages = []
        if req.phase == Phase.DONE:
            completed.append(req)

    for req in completed:
        if req in st.active_requests:
            st.active_requests.remove(req)
        if req.kind == "instance" and not getattr(req, "_successor", False):
            _open_successor(st, req)      # não passou pelo NOTIFY: abre a seguinte agora
            new_messages.extend(st.extra_messages)
            st.extra_messages = []

    _late_meta_step(st, new_messages)
    _spawn_requests(st, new_messages)
    st.messages = new_messages
    _update_global_phase(st)


def _advance_batch_timers(st: SimState):
    """Contador do BatchTimeout das instâncias que esperam pedidos: segue o RELÓGIO REAL (um
    tick não dura sempre 16 ms: o redesenho atrasa o laço), multiplicado pela velocidade, e
    congela na pausa. Guarda em cada instância o que os painéis mostram."""
    now = time.monotonic()
    last = getattr(st, "_last_wall", None)
    st._last_wall = now
    dt = 0.0 if last is None else min(now - last, 0.5)
    for inst in st.instances.values():
        if not (inst.phase == Phase.BATCH_CUT and getattr(inst, "_waiting", False)):
            continue
        inst._wait_elapsed += dt * st.speed * phases.TICKS_PER_SEC
        singles, cross = phases._queued(st, inst.leader, st.epoch_mgr.buckets_of_group(inst.group_id))
        inst._wait_queued = singles + cross
        inst._wait_limit = st.batch_timeout_ticks
        inst._wait_size = st.batch_size


def _late_meta_step(st: SimState, new_messages: list):
    """Cenário 'ADeliver bloqueado': quando o grupo atrasado já está com o commit retido (falta o
    META), espera algumas rodadas para o bloqueio ficar visível e então envia o META aos nós
    dele. Quando a mensagem chega, o RegisterMetadata registra o GSN e o drainBuffer libera."""
    seq_leader = min(st.groups[0].members) if st.groups and st.groups[0].members else 0
    for item in list(st.meta_late):
        if item["state"] != "held":
            continue
        item["age"] += 1
        g, gsn = item["group"], item["gsn"]
        blocked = any(any(e.gsn == gsn for e in (getattr(r, '_delivery_entries', None) or []))
                      for r in st.blocked_requests.get(g, []))
        if blocked:
            item["wait"] += 1
        if item["wait"] < 4 and item["age"] < 80:
            continue
        item["state"] = "sent"
        for nid in item["nodes"]:
            new_messages.append(Message(
                MsgType.META_STREAM, seq_leader, nid,
                label=f"META gsn={gsn} (chegou)",
                detail=f"atrasado para G{g}",
                color="#FFB020",
                on_arrive=(lambda st_, it=item: _late_meta_arrived(st_, it)),
            ))


def _late_meta_arrived(st: SimState, item: dict):
    if item.get("state") == "done":
        return
    item["state"] = "done"
    g, gsn = item["group"], item["gsn"]
    st.delivery.meta_arrived(gsn, g)
    if item in st.meta_late:
        st.meta_late.remove(item)
    st.log_event(Phase.ADELIVER, "META chegou: drainBuffer",
                 f"G{g}: RegisterMetadata(gsn={gsn}) -> drainBuffer libera o commit retido\n"
                 f"O batch e anunciado, o proxy e avisado e o grupo abre a proxima SN",
                 "green")
    st.info_text = (
        f"\U0001f513 META chegou ao G{g}\n\n"
        f"O ADeliver estava bloqueado: o G{g} ja tinha decidido a SN,\n"
        f"mas nao conhecia o GSN {gsn}. Com o META registrado, o\n"
        f"drainBuffer entrega o commit guardado e o grupo segue."
    )
    _release_unblocked(st, g)


def _tag_messages(st: SimState, req: RequestInfo):
    """As mensagens de um item herdam a cor e o snapshot da struct dele."""
    snap = snapshot_client_request(req)
    for msg in st.messages:
        if not msg.color:
            msg.color = req.color
        msg.req_snapshot = snap


def _update_global_phase(st: SimState):
    """Escolhe o item em foco para a UI: um pedido a caminho, senão uma instância com batch."""
    items = st.active_requests
    if not items:
        return
    reqs = [r for r in items if r.kind == "request"]
    busy = [r for r in items if r.kind == "instance" and r.members]
    focus = reqs[0] if reqs else (busy[0] if busy else items[0])
    st.current_request = focus
    st.phase = focus.phase


def _spawn_batch(st: SimState):
    """Inicia a simulação: cada grupo de dados cria a sua primeira instância (bootstrap do
    Start()) e o líder envia o PREPARE. Chamado por Simulation.start()."""
    if st.instances:
        return
    from mirbftview.qt.canvas._constants import MSG_COLOR_POOL  # noqa: F401 (cores das instâncias)

    msgs: list[Message] = []
    for g in [g for g in st.groups if g.id != 0]:
        inst = phases.new_instance(st, g.id)
        st.current_request = inst
        phases.phase_prepare(st)
        _tag_messages(st, inst)
        msgs.extend(st.messages)
        st.messages = []
    st.messages = msgs
    st.phase = Phase.PREPARE
    if st.active_requests:
        st.current_request = st.active_requests[0]
    st.info_text = (
        "✋ Cada grupo abre a sua 1ª instância de Paxos\n\n"
        "Uma instância por grupo, uma de cada vez (runSegment):\n"
        "PREPARE → PROMISE → corte do batch → ACCEPT →\n"
        "ACCEPTED → COMMIT. Quando fecha, nasce a seguinte (SN +\n"
        "nGrupos) e refaz PREPARE/PROMISE.\n\n"
        "Os pedidos dos clientes só começam depois que todos os\n"
        "grupos estiverem preparados."
    )


def _spawn_requests(st: SimState, new_messages: list):
    """Chegada de pedidos. Didático: um por vez (só quando o anterior foi respondido).
    Contínuo: `arrival_burst` pedidos a cada `arrival_every` rodadas, até `max_pending`
    sem resposta, independente de as instâncias terem terminado (é isso que enche os buckets)."""
    from mirbftview.qt.canvas._constants import MSG_COLOR_POOL

    if not st.instances or not st.prepared:
        return
    pending = len(st.pending_info)
    in_flight = any(r.kind == "request" for r in st.active_requests)
    if st.scenarios.get('single_request', False) or st.scenarios.get('adeliver_block', False):
        # Didático: um pedido por vez, só quando o anterior foi respondido (e sem META atrasado pendente)
        n = 1 if (pending == 0 and not in_flight and not st.meta_late) else 0
    else:
        if st.round_count % max(st.arrival_every, 1) != 0 or pending >= st.max_pending:
            return
        n = st.arrival_burst

    for _ in range(n):
        color = MSG_COLOR_POOL[st.spawn_counter % len(MSG_COLOR_POOL)]
        st.spawn_counter += 1
        req = _generate_client_request(st, color)
        st.active_requests.append(req)
        st.current_request = req
        _do_client_send(st, req)
        _tag_messages(st, req)
        new_messages.extend(st.messages)
        st.messages = []


def _advance_request(st: SimState, req: RequestInfo):
    if req.kind == "instance":
        _advance_instance(st, req)
    else:
        _advance_pedido(st, req)


def _advance_pedido(st: SimState, req: RequestInfo):
    """Pedido de cliente: CLIENT_SEND -> [GSN_ASSIGN] -> BUCKET_ASSIGN -> (sai do pipeline)."""
    seq = REQUEST_CROSS_SEQUENCE if (req.is_cross_group and req.gsn > 0) else REQUEST_SEQUENCE
    try:
        idx = seq.index(req.phase)
    except ValueError:
        req.phase = Phase.DONE
        return
    if idx + 1 >= len(seq):
        return
    next_phase = seq[idx + 1]
    if next_phase == Phase.DONE:
        req.phase = Phase.DONE  # as cópias já chegaram aos buckets; agora ele só espera lá
        return
    _execute_phase(st, req, next_phase)
    req.phase = next_phase


def _advance_instance(st: SimState, req: RequestInfo):
    """Instância de Paxos de um grupo para a sua próxima fase."""
    current = req.phase
    try:
        idx = INSTANCE_SEQUENCE.index(current)
    except ValueError:
        _handle_special(st, req)
        return
    if idx + 1 >= len(INSTANCE_SEQUENCE):
        return
    next_phase = INSTANCE_SEQUENCE[idx + 1]

    # Instância preparada mas sem pedidos: waitForRequests (sem timeout). A cada rodada
    # tenta cortar de novo; quando corta, a próxima rodada segue para o ACCEPT.
    if current == Phase.BATCH_CUT and getattr(req, '_waiting', False):
        phases.try_cut(st, req)
        return

    # PROMISE -> corte: quórum de PROMISE atingido (prepared=true) e ProposeIfDue chama o
    # CutBatch dos buckets do grupo.
    if next_phase == Phase.BATCH_CUT:
        req.phase = Phase.BATCH_CUT
        req._waiting = True
        req._prepared_once = True
        if all(getattr(i, '_prepared_once', False) for i in st.instances.values()):
            st.prepared = True
        phases.try_cut(st, req)
        return

    # Cenário: batch_resurrect (GIVE-UP sem quórum): o batch volta aos buckets do líder
    # e a instância segue com um valor vazio (NOP).
    if next_phase == Phase.ACCEPT and st.scenarios.get('batch_resurrect', False):
        if req.members and not getattr(req, '_resurrect_done', False) and random.random() < 0.2:
            req._resurrect_done = True
            phases.resurrect_batch(st, req, "a instancia desistiu do quorum (Eventual Progress)")

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

    # Cenário: adeliver_block — agora é o META atrasado (ver phase_gsn_assign e _late_meta_step);
    # o bloqueio artificial antigo foi desligado.
    if False and next_phase == Phase.ADELIVER and st.scenarios.get('adeliver_block', False):
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

    # ADeliver bloqueou de verdade (BufferCommit real, multipaxosinstance.go:432-436): a
    # instância sai do pipeline ativo e só volta quando outro commit do grupo liberar o GSN
    # anterior — não avança para COMMIT_NOTIFY (announce()) enquanto isso não acontece.
    if current == Phase.ADELIVER and next_phase == Phase.COMMIT_NOTIFY:
        entries = getattr(req, '_delivery_entries', None)
        if entries and not all(e.delivered for e in entries):
            st.active_requests.remove(req)
            for gid in {e.group_id for e in entries}:
                st.blocked_requests.setdefault(gid, []).append(req)
            return

    _execute_phase(st, req, next_phase)
    req.phase = next_phase

    # runSegment: assim que a entrada deste SN está no log (announce -> NotifyProxy), o laço do
    # grupo avança para o SN seguinte e cria a instância dele: o PREPARE do sucessor sai junto
    # com a notificação/resposta ao cliente, e não depois dela. (Com ADeliver bloqueado a
    # entrada não é anunciada, então o sucessor também espera.) Com líder diferente
    # (Simple), o PREPARE sai do NOVO líder.
    if next_phase == Phase.COMMIT_NOTIFY:
        _open_successor(st, req)

    # Depois de rodar ADeliver, tenta liberar instâncias que ficaram bloqueadas antes:
    # o _try_deliver interno pode ter destravado várias entradas de uma vez (drainBuffer).
    if next_phase == Phase.ADELIVER:
        _release_unblocked(st, req.group_id)


def _open_successor(st: SimState, req: RequestInfo):
    """Cria a instância seguinte do grupo (SN + nGrupos) e envia o PREPARE dela. As mensagens
    vão para `st.extra_messages` para não atropelar as do item que está sendo avançado."""
    req._successor = True
    saved_current, saved_msgs = st.current_request, st.messages
    nxt = phases.new_instance(st, req.group_id)
    st.current_request = nxt
    st.messages = []
    phases.phase_prepare(st)
    _tag_messages(st, nxt)
    st.extra_messages.extend(st.messages)
    st.messages = saved_msgs
    st.current_request = saved_current


def _release_unblocked(st: SimState, group_id: int):
    """Libera instâncias bloqueadas pelo ADeliver deste grupo cujas entradas acabaram de ser
    todas entregues (drainBuffer real). Continua a partir de ADELIVER (já executado): o
    próximo tick avança para COMMIT_NOTIFY."""
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
        for gid, lst in list(st.blocked_requests.items()):
            if req in lst:
                lst.remove(req)
                if not lst:
                    st.blocked_requests.pop(gid, None)
        st.log_event(Phase.ADELIVER, "ADeliver liberado",
                     f"G{req.group_id} SN={req.sn}: GSN {[e.gsn for e in req._delivery_entries if e.gsn]} "
                     f"finalmente entregue\nBatch retomado para COMMIT_NOTIFY",
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
    elif phase == Phase.PREPARE:
        phases.phase_prepare(st)
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

      - cada cliente mantém seu PRÓPRIO contador sequencial (seqNr);
      - a chave é sempre K{seqNr:08d} (sequencial, não aleatória);
      - TX vs GET é decidido deterministicamente por seqNr%100 < CrossOpRatio; as chaves de
        uma TX ficam a 1000 de distância, e a quantidade vem do CrossOpGroupWeights;
      - o GRUPO é uma CONSEQUÊNCIA do hash real da chave (key_to_group).

    Se as chaves de uma TX colidirem no mesmo grupo, a operação vira single-group de verdade.
    """
    data_groups = [g for g in st.groups if g.id != 0]
    num_data_groups = len(data_groups)

    client = random.choice(st.clients)
    seq_nr = st.client_seq.get(client.id, 0)
    st.client_seq[client.id] = seq_nr + 1

    cross_ratio_pct = round(st.cross_op_pct * 100)
    k1 = f"K{seq_nr:08d}"
    force_cross = st.scenarios.get('cross_group', False) or st.scenarios.get('adeliver_block', False)
    if (seq_nr % 100) < cross_ratio_pct or force_cross:
        weights = parse_cross_op_group_weights(st.cross_op_group_weights)
        n_keys = pick_cross_op_group_count(seq_nr, weights)
        keys = [f"K{seq_nr + i * 1000:08d}" for i in range(n_keys)]
        touched = sorted({key_to_group(k, num_data_groups) for k in keys})
        if st.scenarios.get('adeliver_block', False):
            # Didático: garante 2 grupos diferentes (se as chaves colidem, afasta a segunda)
            step = 1
            while len(touched) < 2 and step < 400:
                keys = [f"K{seq_nr:08d}", f"K{seq_nr + (step + 1) * 1000:08d}"]
                touched = sorted({key_to_group(k, num_data_groups) for k in keys})
                step += 1
        payload = "TX " + ",".join(keys)
    else:
        payload = f"GET {k1}"
        touched = [key_to_group(k1, num_data_groups)]

    is_cross = len(touched) > 1
    group_id = touched[0]
    group = st.groups[group_id]
    quorum = len(group.members) // 2 + 1
    # Instância corrente do grupo (só para exibição: o líder/SN que provavelmente vai cortá-lo)
    inst = st.instances.get(group_id)
    sn = inst.sn if inst else st.epoch_mgr.next_sn_for(group_id)
    leader = inst.leader if inst else st.epoch_mgr.leader_for_group(group_id, sn)

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
        kind="request",
    )
    if is_cross:
        st.gsn += 1
        req.gsn = st.gsn
    return req


def _do_client_send(st: SimState, req: RequestInfo):
    """Log/mensagem visual do envio ao proxy."""
    import hashlib

    client = st.clients[req.client_id] if req.client_id < len(st.clients) else st.clients[0]
    payload_hash = hashlib.sha256(req.payload.encode()).hexdigest()[:16]
    req.payload_hash = payload_hash

    # Bucket (informativo): GetBucketNr com o GroupId da cópia
    req.bucket_id = st.epoch_mgr.get_request_bucket(client.id, req.client_sn, req.group_id)

    group = st.groups[req.group_id]
    req.proxy_node = st.proxy.pick_proxy(req.client_sn, [n.id for n in st.nodes])
    st.proxy.register_request(client.id, req.client_sn, req.proxy_node)
    st.pending_info[(client.id, req.client_sn)] = {
        "proxy": req.proxy_node, "color": req.color, "name": client.name}
    proxy_in_group = req.proxy_node in group.members

    st.phase = Phase.CLIENT_SEND
    st.log_event(Phase.CLIENT_SEND, "Cliente envia request",
                 f"{client.name} -> Node {req.proxy_node} (proxy, clSn={req.client_sn} % {len(st.nodes)} nós)\n"
                 f"Payload: \"{req.payload}\" | Hash: {payload_hash[:8]}\n"
                 f"Grupo: G{req.group_id}"
                 + (f" (toca {['G' + str(g) for g in req.touched_groups]})" if req.is_cross_group else "") + "\n"
                 f"Proxy {'é' if proxy_in_group else 'NÃO é'} membro de G{req.group_id}",
                 "orange")

    st.info_text = (
        f"\U0001f536 Cliente envia request ao sistema\n\n"
        f"{client.name} envia para Node {req.proxy_node} (proxy).\n"
        f"O cliente real escolhe o proxy em rodízio: nº do pedido\n"
        f"({req.client_sn}) mod {len(st.nodes)} nós = Node {req.proxy_node}.\n"
        f"{'' if proxy_in_group else 'Este nó NÃO pertence ao grupo: só repassa. '}"
        f"O proxy encaminha ao grupo G{req.group_id}.\n\n"
        f"O pedido só será levado a um batch quando a instância do\n"
        f"grupo (líder Node {req.leader}) estiver preparada e cortar.\n\n"
        f"─── Detalhes ───\n"
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
    """Trata fases especiais de uma instância."""
    if req.phase == Phase.CHECKPOINT:
        # Checkpoint só permite truncar o log — não troca líder nem redistribui buckets.
        phases.phase_done(st)
        req.phase = Phase.DONE
    elif req.phase == Phase.VIEW_CHANGE:
        # O líder falhou: o batch cortado volta aos buckets e a posição é preenchida com
        # NOP (Eventual Progress); os pedidos serão propostos pela instância seguinte.
        if req.members:
            phases.resurrect_batch(st, req, f"o lider Node {req.leader} falhou (view change)")
        phases.phase_done(st)
        req.phase = Phase.DONE
    elif req.phase == Phase.RETRANSMIT:
        phases.phase_accepted(st)
        req.phase = Phase.ACCEPTED
    else:
        phases.phase_done(st)
        req.phase = Phase.DONE


def _needs_checkpoint(st: SimState) -> bool:
    """Verifica se é hora de fazer checkpoint NESTE grupo (ver recordGroupCommit em
    multipaxosorderer.go): cada grupo de dados conta seus próprios commits e estabiliza seu
    checkpoint sozinho, dividindo checkpoint_interval pelo número de grupos de dados pra manter
    a mesma frequência relativa que o intervalo teria num log não particionado em grupos.
    """
    req = st.current_request
    gid = req.group_id
    count = st.group_commit_count.get(gid, 0)
    num_groups = max(st.epoch_mgr.sn_stride, 1)
    interval = max(st.checkpoint_interval // num_groups, 1)
    last_sn = st.group_last_checkpoint_sn.get(gid, -1)
    if st.scenarios.get('checkpoint_force', False) and count > 0:
        return last_sn != req.sn
    return count > 0 and count % interval == 0 and last_sn != req.sn


def _decay_visual_events(st: SimState):
    """Decrementa TTL dos visual events e bucket bubbles, remove expirados."""
    for ev in st.visual_events:
        ev["ttl"] -= 1
    st.visual_events = [ev for ev in st.visual_events if ev["ttl"] > 0]
    if len(st.visual_events) > 40:
        st.visual_events = st.visual_events[-40:]
    # Pedidos que saíram do bucket: a animação de saída dura BUCKET_LEAVE_TICKS
    st.bucket_leaving = [e for e in st.bucket_leaving
                         if st.tick_count - e["born"] < phases.BUCKET_LEAVE_TICKS]

    # Decay bucket thought bubbles
    expired = [bid for bid, b in st.bucket_bubbles.items() if b["ttl"] <= 0]
    for bid in expired:
        del st.bucket_bubbles[bid]
    for bid in st.bucket_bubbles:
        st.bucket_bubbles[bid]["ttl"] -= 1
