"""Renderização de nós e clientes."""

from PySide6.QtCore import Qt, QPointF, QRectF
from PySide6.QtGui import (
    QPainter, QPainterPath, QColor, QLinearGradient, QRadialGradient,
    QPen, QBrush, QFont
)
from mirbftview.qt.theme import C
from mirbftview.qt.simulation import Phase
from mirbftview.qt.canvas._constants import GROUP_COLORS


def draw_groups(p, sim, node_pos):
    import math
    for group in sim.groups:
        if not group.members:
            continue
        points = [node_pos[nid] for nid in group.members if nid in node_pos]
        if len(points) < 2:
            continue
        cx = sum(pt.x() for pt in points) / len(points)
        cy = sum(pt.y() for pt in points) / len(points)
        max_dist = max(math.hypot(pt.x() - cx, pt.y() - cy) for pt in points)
        r = max_dist + 60
        color = QColor(GROUP_COLORS[group.id % len(GROUP_COLORS)])
        color.setAlpha(15)
        p.setPen(Qt.NoPen)
        p.setBrush(color)
        p.drawEllipse(QPointF(cx, cy), r, r * 0.7)
        border_c = QColor(GROUP_COLORS[group.id % len(GROUP_COLORS)])
        border_c.setAlpha(45)
        p.setPen(QPen(border_c, 1.2, Qt.DashLine))
        p.setBrush(Qt.NoBrush)
        p.drawEllipse(QPointF(cx, cy), r, r * 0.7)
        p.setPen(QColor(GROUP_COLORS[group.id % len(GROUP_COLORS)]))
        p.setFont(QFont("Segoe UI", 8, QFont.Bold))
        p.drawText(QRectF(cx - 60, cy - r * 0.7 - 18, 120, 16), Qt.AlignCenter, group.name)


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
    for node in sim.nodes:
        pos = node_pos.get(node.id)
        if pos is None:
            continue
        is_leader = req and req.leader == node.id
        is_in_group = req and req.group_id in [g.id for g in sim.groups if node.id in g.members]
        _draw_single_node(p, sim, node, pos, is_leader, is_in_group, msg_color)


def _draw_single_node(p, sim, node, pos, is_leader, is_in_group, msg_color):
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

    if is_leader and sim.phase not in (Phase.IDLE, Phase.DONE):
        p.setPen(QColor(C["phase_prepare"]))
        p.setFont(QFont("Segoe UI", 7, QFont.Bold))
        p.drawText(QRectF(pos.x() - r, pos.y() - r - 16, r * 2, 12), Qt.AlignCenter, "\u2605 L\u00cdDER")


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
