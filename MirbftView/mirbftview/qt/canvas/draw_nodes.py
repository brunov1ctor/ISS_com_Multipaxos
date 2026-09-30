"""Renderização de nós e clientes."""

from PySide6.QtCore import Qt, QPointF, QRectF
from PySide6.QtGui import (
    QPainter, QPainterPath, QPainterPathStroker, QPolygonF, QColor,
    QLinearGradient, QRadialGradient, QPen, QBrush, QFont
)
from mirbftview.qt.theme import C
from mirbftview.qt.simulation import Phase
from mirbftview.qt.canvas._constants import GROUP_COLORS


_NODE_R = 34          # raio do círculo do nó (igual ao de _draw_single_node)
_GROUP_MARGIN = 10    # folga do contorno do grupo além do círculo dos nós


def _convex_hull(points):
    """Envoltória convexa (monotone chain) de uma lista de QPointF."""
    pts = sorted({(pt.x(), pt.y()) for pt in points})
    if len(pts) <= 2:
        return [QPointF(x, y) for x, y in pts]

    def cross(o, a, b):
        return (a[0] - o[0]) * (b[1] - o[1]) - (a[1] - o[1]) * (b[0] - o[0])

    lower, upper = [], []
    for pt in pts:
        while len(lower) >= 2 and cross(lower[-2], lower[-1], pt) <= 0:
            lower.pop()
        lower.append(pt)
    for pt in reversed(pts):
        while len(upper) >= 2 and cross(upper[-2], upper[-1], pt) <= 0:
            upper.pop()
        upper.append(pt)
    return [QPointF(x, y) for x, y in lower[:-1] + upper[:-1]]


def _enclosing_shape(points, extra=0):
    """Contorno que envolve TODOS os pontos: envoltória convexa engrossada por
    (raio do nó + margem + extra), com cantos arredondados e o interior preenchido."""
    pad = _NODE_R + _GROUP_MARGIN + extra
    hull = _convex_hull(points)
    shape = QPainterPath()
    if len(hull) == 1:
        shape.addEllipse(hull[0], pad, pad)
        return shape
    base = QPainterPath()
    base.addPolygon(QPolygonF(hull))
    base.closeSubpath()
    stroker = QPainterPathStroker()
    stroker.setWidth(pad * 2)
    stroker.setJoinStyle(Qt.RoundJoin)
    stroker.setCapStyle(Qt.RoundCap)
    return stroker.createStroke(base).united(base).simplified()


_GROUP_BASE_EXTRA = -2     # folga do primeiro grupo em relação a (raio do nó + margem)
_GROUP_MAX_SPREAD = 16.0   # quanto o último grupo pode ficar mais folgado que o primeiro


def _group_offsets(sim):
    """Folga extra de cada grupo de dados: cada grupo ganha um contorno um pouco mais
    afastado dos nós que o anterior. Dois grupos que compartilham um lado (dois nós em
    comum) ficam com bordas paralelas, e não com o pontilhado de um em cima do outro."""
    data = [g for g in sim.groups if g.id != 0 and g.members]
    step = min(4.0, _GROUP_MAX_SPREAD / max(len(data) - 1, 1))
    return {g.id: _GROUP_BASE_EXTRA + rank * step for rank, g in enumerate(data)}


def _group_shape(points, extra=_GROUP_BASE_EXTRA):
    """Região do grupo: envoltória convexa dos membros engrossada por (raio do nó +
    margem + extra), com cantos arredondados. Envolve TODOS os membros, e o
    posicionamento (layout.py) afasta os nós de fora, então a região não engloba
    quem não é do grupo."""
    return _enclosing_shape(points, extra=extra)


def _draw_eye(p, cx, cy, color, hidden):
    """Olhinho ao lado do nome do grupo: aberto = região visível; riscado = escondida."""
    c = QColor(color)
    c.setAlpha(90 if hidden else 230)
    p.setBrush(Qt.NoBrush)
    p.setPen(QPen(c, 1.3))
    lens = QPainterPath()
    lens.moveTo(cx - 7, cy)
    lens.quadTo(cx, cy - 7, cx + 7, cy)
    lens.quadTo(cx, cy + 7, cx - 7, cy)
    p.drawPath(lens)
    p.setPen(Qt.NoPen)
    p.setBrush(c)
    p.drawEllipse(QPointF(cx, cy), 2.2, 2.2)
    if hidden:
        p.setPen(QPen(c, 1.6))
        p.drawLine(QPointF(cx - 7, cy + 6), QPointF(cx + 7, cy - 6))


def draw_groups(p, sim, node_pos, hidden=None, eye_rects=None):
    """`hidden`: ids de grupos com a região escondida (só o nome e o olhinho ficam).
    `eye_rects`: dicionário preenchido com a área de clique de cada olhinho (coordenadas da cena)."""
    import math
    hidden = hidden if hidden is not None else set()
    all_pts = [pt for pt in node_pos.values()]
    if not all_pts:
        return
    gcx = sum(pt.x() for pt in all_pts) / len(all_pts)
    gcy = sum(pt.y() for pt in all_pts) / len(all_pts)

    # G0 (Sequenciador): contém TODOS os nós. Contorno pontilhado por fora, sem
    # preenchimento (cobriria o desenho), desenhado antes das manchas dos outros grupos.
    g0 = next((g for g in sim.groups if g.id == 0 and g.members), None)
    if g0 is not None:
        g0_points = [node_pos[nid] for nid in g0.members if nid in node_pos]
        if g0_points:
            g0_color = QColor(GROUP_COLORS[0 % len(GROUP_COLORS)])
            offsets = _group_offsets(sim)
            widest = max(offsets.values(), default=_GROUP_BASE_EXTRA)
            shape0 = _enclosing_shape(g0_points, extra=max(16, widest + 12))
            off0 = 0 in hidden
            if not off0:
                fill0 = QColor(g0_color)
                fill0.setAlpha(6)
                p.setPen(Qt.NoPen)
                p.setBrush(fill0)
                p.drawPath(shape0)
                border0 = QColor(g0_color)
                border0.setAlpha(120)
                p.setPen(QPen(border0, 1.4, Qt.DotLine))
                p.setBrush(Qt.NoBrush)
                p.drawPath(shape0)
            # Nome no ponto mais alto do contorno, fora dele
            top = shape0.boundingRect()
            name_c = QColor(g0_color)
            name_c.setAlpha(110 if off0 else 255)
            p.setPen(name_c)
            p.setFont(QFont("Segoe UI", 8, QFont.Bold))
            p.drawText(QRectF(top.center().x() - 90, top.top() - 16, 180, 14), Qt.AlignCenter, g0.name)
            tw = p.fontMetrics().horizontalAdvance(g0.name)
            ex, ey = top.center().x() + tw / 2 + 11, top.top() - 9
            _draw_eye(p, ex, ey, g0_color, off0)
            if eye_rects is not None:
                eye_rects[0] = QRectF(ex - 9, ey - 8, 18, 16)

    label_slots: dict = {}
    for group in sim.groups:
        if group.id == 0 or not group.members:
            continue
        points = [node_pos[nid] for nid in group.members if nid in node_pos]
        if not points:
            continue
        base_color = QColor(GROUP_COLORS[group.id % len(GROUP_COLORS)])
        extra = _group_offsets(sim).get(group.id, _GROUP_BASE_EXTRA)
        shape = _group_shape(points, extra)

        off = group.id in hidden
        if not off:
            fill = QColor(base_color)
            fill.setAlpha(16)
            p.setPen(Qt.NoPen)
            p.setBrush(fill)
            p.drawPath(shape)
            border = QColor(base_color)
            border.setAlpha(110)
            p.setPen(QPen(border, 1.4, Qt.DashLine))
            p.setBrush(Qt.NoBrush)
            p.drawPath(shape)

        # Nome do grupo no lado de fora, junto ao membro mais afastado do centro.
        far = max(points, key=lambda pt: math.hypot(pt.x() - gcx, pt.y() - gcy))
        dx, dy = far.x() - gcx, far.y() - gcy
        norm = math.hypot(dx, dy) or 1.0
        # Grupos que escolhem o mesmo nó âncora empilham os nomes para fora, em vez de
        # escrever um em cima do outro.
        anchor = (round(far.x()), round(far.y()))
        slot = label_slots.get(anchor, 0)
        label_slots[anchor] = slot + 1
        reach = _NODE_R + _GROUP_MARGIN + extra + 12 + slot * 15
        lx, ly = far.x() + dx / norm * reach, far.y() + dy / norm * reach
        label_c = QColor(base_color)
        label_c.setAlpha(110 if off else 255)
        p.setPen(label_c)
        p.setFont(QFont("Segoe UI", 8, QFont.Bold))
        p.drawText(QRectF(lx - 60, ly - 8, 120, 16), Qt.AlignCenter, group.name)
        tw = p.fontMetrics().horizontalAdvance(group.name)
        ex, ey = lx + tw / 2 + 11, ly
        _draw_eye(p, ex, ey, base_color, off)
        if eye_rects is not None:
            eye_rects[group.id] = QRectF(ex - 9, ey - 8, 18, 16)


def draw_connections(p, sim, node_pos):
    for group in sim.groups:
        if group.id == 0:
            continue
        color = QColor(GROUP_COLORS[group.id % len(GROUP_COLORS)])
        color.setAlpha(20)
        p.setPen(QPen(color, 0.8))
        members = [node_pos[nid] for nid in group.members if nid in node_pos]
        for i in range(len(members)):
            for j in range(i + 1, len(members)):
                p.drawLine(members[i], members[j])


def draw_nodes(p, sim, node_pos, msg_color):
    req = sim.current_request
    instances = list(sim.instances.values())
    pending = dict(sim.pending_info)   # (clienteId, clienteSn) -> {"proxy", "color", "name"}

    # Foco (realce forte de líder e do grupo): o(s) grupo(s) do item atual — o pedido a
    # caminho, ou a instância em foco — e as instâncias que estão em consenso com batch.
    focus_groups = set()
    if req is not None:
        if req.kind == "instance":
            focus_groups.add(req.group_id)
        else:
            focus_groups.update(req.touched_groups or [req.group_id])
    busy = (Phase.ACCEPT, Phase.ACCEPTED, Phase.COMMIT, Phase.ADELIVER, Phase.COMMIT_NOTIFY)
    focus = [i for i in instances
             if i.group_id in focus_groups or (i.members and i.phase in busy)]
    leader_nodes = {i.leader for i in focus}
    family_groups = {i.group_id for i in focus}

    # Rótulos de papel por nó, na cor do item:
    #   líder -> "★ LÍDER G3 · SN 8 · 2 ped." (grupo, posição do log e tamanho do batch)
    #   proxy -> "📬 PROXY · Cliente 1 #0" (de qual cliente/pedido ele é o carteiro)
    labels: dict[int, list] = {}

    def _color(c, fallback):
        col = QColor(c) if c else QColor(fallback)
        return col if col.isValid() else QColor(fallback)

    for inst in instances:
        text = f"\u2605 L\u00cdDER G{inst.group_id} \u00b7 SN {inst.sn}"
        if inst.members:
            text += f" \u00b7 {len(inst.members)} ped."
        labels.setdefault(inst.leader, []).append((text, _color(inst.color, C["phase_prepare"])))
    for (cid, csn), info in pending.items():
        labels.setdefault(info["proxy"], []).append(
            (f"\U0001f4ec PROXY \u00b7 {info['name']} #{csn}", _color(info.get("color"), "#FF5A5A")))
    proxy_nodes = {info["proxy"] for info in pending.values()}

    for node in sim.nodes:
        pos = node_pos.get(node.id)
        if pos is None:
            continue
        is_leader = node.id in leader_nodes
        is_in_group = any(g.id in family_groups and node.id in g.members for g in sim.groups)
        # Proxy: recebeu do cliente algum pedido que ainda não foi respondido. Vale mesmo que
        # o nó também seja líder: com um só pedido (modo didático) isso é frequente.
        is_proxy = node.id in proxy_nodes and sim.phase not in (Phase.IDLE, Phase.DONE)
        _draw_single_node(p, sim, node, pos, is_leader, is_in_group, msg_color, is_proxy,
                          labels.get(node.id, []))


def _draw_single_node(p, sim, node, pos, is_leader, is_in_group, msg_color, is_proxy=False, labels=()):
    r = 34
    if is_leader:
        border_color = QColor(msg_color) if msg_color else QColor(C["accent"])
    elif is_in_group and sim.phase not in (Phase.IDLE, Phase.DONE):
        bc = QColor(msg_color) if msg_color else QColor(C["accent"])
        bc.setAlpha(160)
        border_color = bc
    else:
        border_color = QColor(C["node_idle"])

    if is_leader and sim.phase not in (Phase.IDLE, Phase.DONE):
        glow = QRadialGradient(pos, r + 16)
        gc = QColor(border_color)
        gc.setAlpha(50)
        glow.setColorAt(0.0, gc)
        glow.setColorAt(1.0, QColor(0, 0, 0, 0))
        p.setPen(Qt.NoPen)
        p.setBrush(QBrush(glow))
        p.drawEllipse(pos, r + 16, r + 16)

    path = QPainterPath()
    path.addEllipse(pos, r, r)
    grad = QLinearGradient(pos.x(), pos.y() - r, pos.x(), pos.y() + r)
    grad.setColorAt(0.0, QColor(40, 60, 90, 210))
    grad.setColorAt(1.0, QColor(20, 36, 60, 230))
    p.fillPath(path, QBrush(grad))

    spec = QPainterPath()
    spec.addEllipse(QPointF(pos.x(), pos.y() - r * 0.3), r * 0.65, r * 0.35)
    sg = QLinearGradient(pos.x(), pos.y() - r, pos.x(), pos.y())
    sg.setColorAt(0.0, QColor(255, 255, 255, 40))
    sg.setColorAt(1.0, QColor(255, 255, 255, 0))
    p.setPen(Qt.NoPen)
    p.fillPath(spec, QBrush(sg))

    if is_proxy:
        # Névoa vermelha interna: nó atuando como proxy (carteiro do cliente).
        haze = QRadialGradient(pos, r)
        haze.setColorAt(0.0, QColor(255, 40, 40, 90))
        haze.setColorAt(0.55, QColor(255, 40, 40, 150))
        haze.setColorAt(1.0, QColor(255, 40, 40, 215))
        p.fillPath(path, QBrush(haze))

    bc = QColor(border_color)
    bc.setAlpha(200 if is_leader else 100)
    p.setPen(QPen(bc, 2.5 if is_leader else 1.5))
    p.setBrush(Qt.NoBrush)
    p.drawEllipse(pos, r, r)

    p.setPen(QColor(C["text"]))
    p.setFont(QFont("Segoe UI", 10, QFont.Bold))
    p.drawText(QRectF(pos.x() - r, pos.y() - 8, r * 2, 16), Qt.AlignCenter, node.name)

    groups_str = ",".join(f"G{g}" for g in node.groups)
    p.setPen(QColor(C["text3"]))
    p.setFont(QFont("Segoe UI", 7))
    p.drawText(QRectF(pos.x() - r, pos.y() + 10, r * 2, 12), Qt.AlignCenter, groups_str)

    # Rótulos de papel empilhados acima do nó (líderes primeiro, depois proxies), cada um
    # na cor do pedido a que se refere.
    if labels and sim.phase not in (Phase.IDLE, Phase.DONE):
        p.setFont(QFont("Segoe UI", 7, QFont.Bold))
        max_lines = 4
        shown = list(labels)
        if len(shown) > max_lines:
            hidden = len(shown) - (max_lines - 1)
            shown = shown[:max_lines - 1] + [(f"+{hidden} mais", QColor(C["text3"]))]
        for k, (text, color) in enumerate(shown):
            p.setPen(color)
            p.drawText(QRectF(pos.x() - 120, pos.y() - r - 16 - k * 13, 240, 12), Qt.AlignCenter, text)


def draw_clients(p, sim, client_pos):
    for client in sim.clients:
        pos = client_pos.get(client.id)
        if pos is None:
            continue
        r = 22
        rect = QRectF(pos.x() - r, pos.y() - r, r * 2, r * 2)
        path = QPainterPath()
        path.addRoundedRect(rect, 10, 10)
        p.fillPath(path, QColor(60, 40, 100, 190))
        p.setPen(QPen(QColor(C["orange"]), 1.5))
        p.drawPath(path)
        p.setPen(QColor(C["text"]))
        p.setFont(QFont("Segoe UI", 8, QFont.Bold))
        p.drawText(rect, Qt.AlignCenter, client.name)


def draw_sequencer_glow(p, sim, node_pos):
    meta = sim.meta_stream
    if not meta:
        return
    seq_id = min(sim.groups[0].members) if sim.groups and sim.groups[0].members else 0
    pos = node_pos.get(seq_id)
    if pos is None:
        return
    gsn_val = sim.gsn
    if gsn_val <= 0:
        return
    glow = QRadialGradient(pos, 55)
    gc = QColor("#A855F7")
    gc.setAlpha(35)
    glow.setColorAt(0.0, gc)
    gc2 = QColor("#D500F9")
    gc2.setAlpha(15)
    glow.setColorAt(0.6, gc2)
    glow.setColorAt(1.0, QColor(0, 0, 0, 0))
    p.setPen(Qt.NoPen)
    p.setBrush(QBrush(glow))
    p.drawEllipse(pos, 55, 55)
    p.setPen(QColor("#A855F7"))
    p.setFont(QFont("Segoe UI", 7, QFont.Bold))
    p.drawText(QRectF(pos.x() - 30, pos.y() + 38, 60, 12), Qt.AlignCenter, f"SEQ GSN={gsn_val}")


def draw_meta_broadcast_lines(p, sim, node_pos):
    if sim.phase != Phase.GSN_ASSIGN:
        return
    seq_id = min(sim.groups[0].members) if sim.groups and sim.groups[0].members else 0
    seq_pos = node_pos.get(seq_id)
    if seq_pos is None:
        return
    pen = QPen(QColor("#A855F7"), 1.5, Qt.DashLine)
    p.setPen(pen)
    for nid in sim.groups[0].members:
        if nid == seq_id:
            continue
        npos = node_pos.get(nid)
        if npos:
            p.drawLine(seq_pos, npos)
