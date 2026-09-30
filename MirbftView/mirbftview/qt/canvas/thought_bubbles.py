"""Thought bubbles — balões de pensamento nos nós."""

from PySide6.QtCore import Qt, QPointF, QRectF
from PySide6.QtGui import QPainter, QPainterPath, QColor, QPen, QFont

from mirbftview.qt.theme import C
from mirbftview.qt.simulation import MsgType
from mirbftview.qt.canvas._constants import MSG_COLORS
from mirbftview.protocol.types import key_to_group

# Último CLIENT_REQUEST de cada cliente que já ganhou balão (guarda a própria
# mensagem, não só o id, para não repetir o balão a cada quadro e para reaparecer
# corretamente depois de um Reset).
_client_bubble_seen: dict = {}
_last_tick_count = 0  # último sim.tick_count visto por update_thought_bubbles

_FORMULA_TTL = 110
_FORMULA_MAX_W = 250
_BUCKET_TTL = 100  # cada um dos dois balões da escolha do bucket


def update_thought_bubbles(sim, thought_bubbles):
    """Atualiza TTL e gera novos bubbles para mensagens que chegaram."""
    # Os balões envelhecem por ticks da simulação (sim.tick_count), não por quadros de
    # tela: com a simulação pausada eles ficam parados, e no passo a passo avançam por passo.
    global _last_tick_count
    delta = max(0, sim.tick_count - _last_tick_count)
    _last_tick_count = sim.tick_count
    for nid in list(thought_bubbles):
        thought_bubbles[nid]["ttl"] -= delta
        if thought_bubbles[nid]["ttl"] <= 0:
            # Balão encadeado ("então"): quando um expira, o seguinte aparece no lugar.
            nxt = thought_bubbles[nid].get("then")
            if nxt:
                thought_bubbles[nid] = nxt
            else:
                del thought_bubbles[nid]

    for msg in sim.messages:
        # Balão do CLIENTE assim que a requisição sai: tipo (single/cross-group) e a
        # fórmula pela qual ele escolheu o proxy.
        if (msg.msg_type == MsgType.CLIENT_REQUEST and msg.from_is_client
                and msg.req_snapshot and _client_bubble_seen.get(msg.from_id) is not msg):
            _client_bubble_seen[msg.from_id] = msg
            thought_bubbles[("client", msg.from_id)] = {
                "text": _client_formula_text(sim, msg), "color": msg.color or C["orange"],
                "is_error": False, "ttl": _FORMULA_TTL, "max_w": _FORMULA_MAX_W,
            }
        if msg.progress < 1.0 or msg.to_is_client:
            continue
        # Cópia do pedido chegando a um membro do grupo (sendToGroup -> AddDirectToBucket):
        # é aqui que o Go escolhe o bucket. Balão com a fórmula e, em seguida, a tabela.
        if (msg.msg_type == MsgType.CLIENT_REQUEST and not msg.from_is_client
                and msg.req_snapshot and msg.label.startswith("FWD G")):
            _bucket_bubbles(sim, msg, thought_bubbles)
            continue
        # Balão da escolha do bucket (e a tabela que vem depois) não é coberto por
        # balões genéricos de outras mensagens que chegam ao mesmo nó.
        current = thought_bubbles.get(msg.to_id)
        if current and current.get("sticky"):
            continue
        text, color, is_error = _bubble_for_msg(sim, msg)
        if text:
            bubble = {"text": text, "color": color, "is_error": is_error, "ttl": 45}
            if msg.msg_type == MsgType.CLIENT_REQUEST and msg.from_is_client and msg.req_snapshot:
                # Balão do PROXY: fórmula do CRC32 que escolhe o grupo (ReplicaMapper).
                bubble["ttl"] = _FORMULA_TTL
                bubble["max_w"] = _FORMULA_MAX_W
            thought_bubbles[msg.to_id] = bubble


_bucket_bubble_seen: dict = {}


def _bucket_formula_text(em, cid, sn, gid) -> str:
    b = em.get_request_bucket(cid, sn, gid)
    k = len(em.buckets_of_group(gid))
    ng = em.sn_stride
    return (
        f"AddDirectToBucket()  G{gid}\n"
        "b = g + nG*((cliente + sn) mod nB)\n"
        f"g={gid}  nG={ng} (com G0)  nB={k}\n"
        f"b = {gid} + {ng}*(({cid} + {sn}) mod {k})\n"
        f"b = {gid} + {ng}*{(cid + sn) % k} = B{b}"
    )


def _bucket_bubbles(sim, msg, thought_bubbles):
    """Balão da escolha do bucket, nos membros do grupo que receberam a cópia.

    Mostra a fórmula de GetBucketNr com os números deste pedido. Quem é dono de
    cada bucket aparece no painel de Buckets (uma linha por grupo, com a cor dele).
    O proxy, quando é membro do grupo, faz o AddDirectToBucket local (sem mensagem):
    ganha o mesmo balão na primeira chegada.
    """
    import re
    m = re.match(r"FWD G(\d+)", msg.label)
    if not m or not hasattr(sim, "epoch_mgr"):
        return
    gid = int(m.group(1))
    snap = msg.req_snapshot
    cid, sn = snap["client_id"], snap["client_sn"]
    em = sim.epoch_mgr

    targets = [msg.to_id]
    members = sim.groups[gid].members if gid < len(sim.groups) else []
    if msg.from_id in members:
        targets.append(msg.from_id)  # proxy que também é membro: insere localmente

    color = msg.color or C["gold"]
    for node in targets:
        tag = (cid, sn, gid, node)
        if _bucket_bubble_seen.get(tag) is msg or (node != msg.to_id and tag in _bucket_bubble_seen):
            continue
        _bucket_bubble_seen[tag] = msg
        thought_bubbles[node] = {
            "text": _bucket_formula_text(em, cid, sn, gid), "color": color,
            "is_error": False, "ttl": _BUCKET_TTL, "max_w": _FORMULA_MAX_W, "sticky": True,
        }


def _bubble_for_msg(sim, msg):
    is_error = False
    dest_node = None
    for n in sim.nodes:
        if n.id == msg.to_id:
            dest_node = n
            break
    if dest_node and not dest_node.is_alive:
        return "FALHA! No offline", C["red"], True

    color = msg.color if msg.color else C["text2"]
    phase_name = sim.phase.name if sim.phase else ""

    dispatch = {
        MsgType.CLIENT_REQUEST: f"handleClientRequest()\n[{phase_name}] bucket enqueue",
        MsgType.PREPARE: f"handlePrepare()\n[{phase_name}] ballot={msg.ballot} sn={msg.sn}",
        MsgType.PROMISE: f"handlePromise()\n[{phase_name}] ballot={msg.ballot} ok",
        MsgType.ACCEPT: f"handleAccept()\n[{phase_name}] sn={msg.sn} d={msg.batch_digest[:6]}",
        MsgType.ACCEPTED: f"handleAccepted()\n[{phase_name}] sn={msg.sn} quorum++",
        MsgType.COMMIT: f"handleCommit()\n[{phase_name}] sn={msg.sn} -> log",
        MsgType.COMMIT_NOTIFY: f"outputProcessing()\n[{phase_name}] COMMIT_NOTIFY",
        MsgType.META_STREAM: f"handleMETA()\n[{phase_name}] GSN publicado",
        MsgType.CHECKPOINT: f"handleCheckpoint()\n[{phase_name}] watermarks",
        MsgType.VIEW_CHANGE: f"handleViewChange()\n[{phase_name}] ballot={msg.ballot}",
        MsgType.NEW_VIEW: f"handleNewView()\n[{phase_name}] lider confirmado",
    }

    text = dispatch.get(msg.msg_type, "")
    if msg.msg_type == MsgType.CLIENT_REQUEST and msg.from_is_client and msg.req_snapshot:
        text = _proxy_formula_text(sim, msg)
    if text:
        if msg.msg_type == MsgType.ACCEPT and msg.label and "RE-" in msg.label:
            text = f"handleAccept()\n[RETRANSMIT] sn={msg.sn}"
            is_error = True
            color = C["red"]
        return text, color, is_error
    return "", C["text"], False


def _payload_keys(payload: str):
    """Chaves do payload como o ReplicaMapper as lê: GET k | TX k1,k2."""
    parts = payload.split()
    if len(parts) < 2:
        return parts[0] if parts else "?", []
    return parts[0], [k for k in parts[1].split(",") if k]


def _client_formula_text(sim, msg) -> str:
    snap = msg.req_snapshot
    op, _keys = _payload_keys(snap["payload"])
    kind = "cross-group" if op == "TX" else "single-group"
    n = len(sim.nodes)
    sn = snap["client_sn"]
    return (
        "guessTargetOrderers()\n"
        f"pedido {op} ({kind})\n"
        "proxy = clientSn % nNos\n"
        f"      = {sn} % {n} = Node {msg.to_id}"
    )


def _proxy_formula_text(sim, msg) -> str:
    import zlib
    snap = msg.req_snapshot
    op, keys = _payload_keys(snap["payload"])
    nd = len([g for g in sim.groups if g.id != 0]) or 1
    lines = [f"ReplicaMapper() {op}", f"G = 1 + crc32(chave) % {nd} (n-grupos)", ""]
    groups = []
    for idx, k in enumerate(keys):
        crc = zlib.crc32(k.encode())
        g = key_to_group(k, nd)
        groups.append(g)  # o resultado usa TODAS as chaves; só o detalhe é limitado
        if idx == 0:
            # Primeira chave: passo a passo, para dar para conferir a conta à mão.
            lines += [
                f"chave = {k}",
                f"crc32 = 0x{crc:08X} (= {crc})",
                f"{crc} % {nd} = {crc % nd} (resto)",
                f"G = 1 + {crc % nd} = G{g}",
            ]
        elif idx < 4:
            # Demais chaves: a mesma conta em uma linha.
            lines.append(f"{k}: resto {crc % nd} -> G{g}")
    if len(keys) > 4:
        lines.append(f"... +{len(keys) - 4} chaves")
    touched = sorted(set(groups))
    lines.append("")
    if len(touched) > 1:
        lines.append(f"cross-group {touched} -> GSN")
    else:
        lines.append(f"single-group G{touched[0] if touched else snap['group_id']}")
    return "\n".join(lines)


_placement_cache: dict = {}   # chave do balão -> índice do candidato escolhido (evita "pular")
_NODE_OBS_R = 40
_CLIENT_OBS_R = 26


def _overlap(a: QRectF, b: QRectF) -> float:
    w = min(a.right(), b.right()) - max(a.left(), b.left())
    h = min(a.bottom(), b.bottom()) - max(a.top(), b.top())
    return w * h if w > 0 and h > 0 else 0.0


def _candidates(pos, bw, bh):
    """Posições possíveis do balão ao redor do ponto de ancoragem: 8 direções em 3 distâncias.
    A ordem é a preferência (primeiro acima e à direita, como era antes)."""
    out = []
    for dist in (46, 86, 130):
        out += [
            QRectF(pos.x() + 14, pos.y() - dist - bh, bw, bh),             # cima-direita
            QRectF(pos.x() - bw - 14, pos.y() - dist - bh, bw, bh),        # cima-esquerda
            QRectF(pos.x() + dist, pos.y() - bh / 2, bw, bh),              # direita
            QRectF(pos.x() - dist - bw, pos.y() - bh / 2, bw, bh),         # esquerda
            QRectF(pos.x() + 14, pos.y() + dist, bw, bh),                  # baixo-direita
            QRectF(pos.x() - bw - 14, pos.y() + dist, bw, bh),             # baixo-esquerda
            QRectF(pos.x() - bw / 2, pos.y() - dist - bh, bw, bh),         # cima
            QRectF(pos.x() - bw / 2, pos.y() + dist, bw, bh),              # baixo
        ]
    return out


def draw_thought_bubbles(p, thought_bubbles, node_pos, client_pos=None):
    """Desenha os balões em espaços livres: cada um escolhe, entre várias posições ao redor do
    nó/cliente dele, a que menos cobre nós, rótulos de papel, clientes, outros balões e as
    bordas visíveis da tela. A escolha anterior ganha um bônus para o balão não ficar pulando."""
    items = []
    for nid, bubble in thought_bubbles.items():
        pos = (client_pos or {}).get(nid[1]) if isinstance(nid, tuple) else node_pos.get(nid)
        if pos is None or min(255, bubble["ttl"] * 6) <= 0:
            continue
        lines = bubble["text"].split("\n")
        p.setFont(QFont("Consolas", 7))
        fm = p.fontMetrics()
        line_h = fm.height() + 1
        text_w = max(fm.horizontalAdvance(l) for l in lines) + 16
        bw = max(80, min(bubble.get("max_w", 160), text_w))
        bh = line_h * len(lines) + 10
        items.append((nid, bubble, pos, lines, line_h, bw, bh))
    if not items:
        _placement_cache.clear()
        return

    # Área visível em coordenadas do mundo (o canvas tem zoom/pan)
    inv, ok = p.worldTransform().inverted()
    vis = inv.mapRect(QRectF(p.viewport())) if ok else QRectF(0, 0, 4000, 4000)
    vis = vis.adjusted(6, 6, -6, -6)

    # (retângulo, peso): nós e clientes pesam mais; a faixa dos rótulos de papel, menos
    obstacles = []
    for pt in node_pos.values():
        obstacles.append((QRectF(pt.x() - _NODE_OBS_R, pt.y() - _NODE_OBS_R,
                                 2 * _NODE_OBS_R, 2 * _NODE_OBS_R), 3.0))
        obstacles.append((QRectF(pt.x() - 95, pt.y() - _NODE_OBS_R - 66, 190, 62), 1.0))
    for pt in (client_pos or {}).values():
        obstacles.append((QRectF(pt.x() - _CLIENT_OBS_R, pt.y() - _CLIENT_OBS_R,
                                 2 * _CLIENT_OBS_R, 2 * _CLIENT_OBS_R), 3.0))

    placed: list[QRectF] = []
    live = set()
    for nid, bubble, pos, lines, line_h, bw, bh in items:
        live.add(nid)
        cands = _candidates(pos, bw, bh)
        prev = _placement_cache.get(nid)
        best, best_cost = 0, None
        for i, r in enumerate(cands):
            area = bw * bh
            cost = sum(_overlap(r, o) * wgt for o, wgt in obstacles)
            cost += sum(_overlap(r, q) for q in placed) * 4.0
            cost += (area - _overlap(r, vis)) * 4.0          # parte fora da tela
            cost += i * 0.002 * area                          # preferência pela ordem
            if i == prev:
                cost *= 0.6                                   # histerese
            if best_cost is None or cost < best_cost:
                best, best_cost = i, cost
        _placement_cache[nid] = best
        rect = cands[best]
        placed.append(rect)
        _draw_bubble(p, bubble, pos, rect, lines, line_h, isinstance(nid, tuple))
    for k in list(_placement_cache):
        if k not in live:
            del _placement_cache[k]


def _draw_bubble(p, bubble, pos, rect, lines, line_h, is_client):
    color = bubble["color"]
    is_error = bubble.get("is_error", False)
    alpha = min(255, bubble["ttl"] * 6)
    bx, by, bw, bh = rect.x(), rect.y(), rect.width(), rect.height()

    bg_path = QPainterPath()
    bg_path.addRoundedRect(rect, 8, 8)
    p.fillPath(bg_path, QColor(40, 10, 10, alpha) if is_error else QColor(12, 20, 38, alpha))
    border_c = QColor(color)
    border_c.setAlpha(alpha)
    p.setPen(QPen(border_c, 1.5 if is_error else 1.0))
    p.setBrush(Qt.NoBrush)
    p.drawPath(bg_path)

    # Bolinhas de pensamento: da borda do nó/cliente até o ponto do balão mais próximo dele
    near = QPointF(min(max(pos.x(), rect.left()), rect.right()),
                   min(max(pos.y(), rect.top()), rect.bottom()))
    dx, dy = near.x() - pos.x(), near.y() - pos.y()
    dist = (dx * dx + dy * dy) ** 0.5 or 1.0
    start_r = 24 if is_client else 38
    ux, uy = dx / dist, dy / dist
    sx, sy = pos.x() + ux * start_r, pos.y() + uy * start_r
    span = max(dist - start_r, 0)
    p.setPen(Qt.NoPen)
    dot_c = QColor(color)
    dot_c.setAlpha(alpha)
    p.setBrush(dot_c)
    for frac, r in [(0.25, 2.0), (0.55, 3.0), (0.85, 3.8)]:
        p.drawEllipse(QPointF(sx + ux * span * frac, sy + uy * span * frac), r, r)

    ty = by + 5
    for i, line in enumerate(lines):
        if i == 0:
            p.setPen(QColor(color))
            p.setFont(QFont("Consolas", 7, QFont.Bold))
        else:
            tc = QColor(C["text"] if not is_error else C["red"])
            tc.setAlpha(alpha)
            p.setPen(tc)
            p.setFont(QFont("Consolas", 7))
        p.drawText(QRectF(bx + 6, ty, bw - 12, line_h), Qt.AlignLeft | Qt.AlignVCenter, line)
        ty += line_h
