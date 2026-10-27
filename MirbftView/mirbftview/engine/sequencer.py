"""Sequenciador GSN (grupo 0) -- espelha orderer/sequencer.go + sequencer_election.go.

Reserva de blocos: o líder só numera dentro de uma marca que a MAIORIA do grupo 0 confirmou
(SEQ_RESERVE / SEQ_RESERVE_ACK). Cada nó guarda a maior reserva que confirmou (accepted). Na
eleição, cada voto leva essa reserva e o maior GSN com META que o nó viu; o novo líder retoma
logo após a maior reserva informada pela maioria (maiorias se cruzam => nenhum GSN acima dela
foi usado) e declara pulados os números entre o maior META e a marca (META_SKIP).
"""

from mirbftview.protocol.types import Message, MsgType, Phase

SEQ_BLOCK = 100  # gsnBlockSize no Go


def ensure_seq(st):
    """Inicializa o estado do sequenciador (preguiçoso: grupos podem mudar no Config)."""
    if getattr(st, "seq_leader", None) is None:
        members = st.groups[0].members if st.groups and st.groups[0].members else [0]
        st.seq_leader = min(members)
        st.seq_term = 0
        st.seq_confirmed = 0          # confirmedUpTo do líder
        st.seq_accepted = {}          # nó -> acceptedUpTo
        st.seq_down = set()           # nós que deixaram de responder como sequenciador
        st.seq_skips = []             # faixas META_SKIP publicadas
        st.seq_visual = []            # ondas de mensagens (listas) para a fase GSN Atribuído
        st.seq_failover_pending = False
        st.seq_events = []            # histórico curto para o painel


def _alive_members(st):
    return [n for n in st.groups[0].members if n not in st.seq_down]


def _reserve(st, upto, reason):
    """SEQ_RESERVE: líder pede "posso numerar até upto"; vivos confirmam (SEQ_RESERVE_ACK)."""
    members = st.groups[0].members
    alive = _alive_members(st)
    for n in alive:
        st.seq_accepted[n] = max(st.seq_accepted.get(n, 0), upto)
    leader = st.seq_leader
    ok = len(alive) > len(members) // 2
    others = [n for n in members if n != leader and n not in st.seq_down]
    # 1ª onda: líder -> membros (SEQ_RESERVE); 2ª onda: membros -> líder (SEQ_RESERVE_ACK)
    st.seq_visual.append([Message(MsgType.GSN_REQUEST, leader, n, label=f"RESERVE ≤{upto}",
                                  detail=f"SEQ_RESERVE term={st.seq_term}") for n in others])
    st.seq_visual.append([Message(MsgType.GSN_RESPONSE, n, leader, label=f"ACK ≤{upto}",
                                  detail="SEQ_RESERVE_ACK") for n in others])
    if ok:
        st.seq_confirmed = upto
    st.log_event(Phase.GSN_ASSIGN, "Reserva de GSN" + (" CONFIRMADA" if ok else " SEM MAIORIA"),
                 f"Lider Node {leader} (term {st.seq_term}) pede: 'posso numerar ate {upto}'\n"
                 f"Confirmaram: {sorted(alive)} ({len(alive)}/{len(members)})"
                 + (f" -> maioria, numera ate {upto}" if ok else " -> sem maioria, NAO numera") + "\n"
                 f"Motivo: {reason}",
                 "purple" if ok else "red")
    st.seq_events.append(f"reserva ≤{upto} ({len(alive)}/{len(members)})")
    return ok


def _failover(st):
    """O líder do grupo 0 para de responder; um novo líder é eleito por maioria."""
    from mirbftview.engine.phases import _gdeliver

    old = st.seq_leader
    st.seq_down.add(old)
    alive = _alive_members(st)
    members = st.groups[0].members
    if len(alive) <= len(members) // 2:
        st.log_event(Phase.GSN_ASSIGN, "Sequenciador sem maioria",
                     f"Node {old} caiu e restam {sorted(alive)}: sem maioria, nao ha eleicao.", "red")
        return
    st.seq_term += 1
    new = min(alive)
    max_meta = max((m["gsn"] for m in st.meta_stream), default=0)
    votes = {n: st.seq_accepted.get(n, 0) for n in alive}
    max_acc = max(votes.values(), default=0)
    resume = max_acc + 1

    # Onda de votos (cada voto leva a reserva aceita e o maior META visto) -- antes da reserva nova
    st.seq_visual.append([Message(MsgType.VIEW_CHANGE, n, new,
                                  label=f"VOTO reserva≤{votes[n]} META≤{max_meta}",
                                  detail=f"SEQ_VOTE term={st.seq_term}") for n in alive if n != new])
    st.log_event(Phase.GSN_ASSIGN, "Eleicao do sequenciador",
                 f"Node {old} parou de enviar heartbeat. Node {new} vira candidato (term {st.seq_term}).\n"
                 f"Votos (reserva aceita): " + ", ".join(f"N{n}={v}" for n, v in sorted(votes.items())) + "\n"
                 f"Maior reserva da maioria = {max_acc}; maior META visto = {max_meta}\n"
                 f"Nenhum GSN acima de {max_acc} pode ter sido usado (maiorias se cruzam).\n"
                 f"Retoma em {resume}, sem palpite.",
                 "gold")
    st.seq_leader = new

    skip_from, skip_to = max_meta + 1, resume - 1
    if skip_to >= skip_from:
        st.seq_skips.append((skip_from, skip_to))
        _gdeliver(st, lambda gd: gd.on_skip(skip_from, skip_to))
        inflight = [g for g in range(skip_from, min(skip_to, st.gsn) + 1)]
        st.log_event(Phase.GSN_ASSIGN, "META_SKIP",
                     f"Node {new} declara pulados os GSNs {skip_from}..{skip_to} (nunca terao META).\n"
                     f"Repetido em todo heartbeat do lider."
                     + (f"\nAVISO: GSN(s) {inflight} ja tinham sido entregues a proxies pelo lider antigo;\n"
                        f"se o META deles chegar agora, a entrega global os ignora (falta termo no GSN)."
                        if inflight else ""),
                     "orange")
    st.gsn = resume - 1
    st.seq_confirmed = resume - 1
    st.seq_events.append(f"term {st.seq_term}: N{old}→N{new}, retoma {resume}")
    _reserve(st, resume - 1 + SEQ_BLOCK, f"novo lider Node {new} precisa de reserva antes de numerar")


def next_gsn(st):
    """allocateGSN: numera só dentro da reserva confirmada pela maioria."""
    ensure_seq(st)
    if st.seq_failover_pending:
        st.seq_failover_pending = False
        _failover(st)
    if st.gsn + 1 > st.seq_confirmed:
        if not _reserve(st, max(st.seq_confirmed, st.gsn) + SEQ_BLOCK,
                        "contador chegou ao fim da reserva confirmada"):
            return 0
    st.gsn += 1
    gsn = st.gsn
    # Pede o próximo bloco na metade, fora do caminho das mensagens.
    if st.seq_confirmed - gsn == SEQ_BLOCK // 2 - 1:
        _reserve(st, st.seq_confirmed + SEQ_BLOCK, f"metade do bloco usada (GSN {gsn})")
    return gsn
