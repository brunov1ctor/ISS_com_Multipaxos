"""Renderização de mensagens (partículas, trails, cores)."""

import math
from PySide6.QtCore import Qt, QPointF, QRectF
from PySide6.QtGui import QPainter, QColor, QRadialGradient, QPen, QBrush, QFont

from mirbftview.qt.theme import C
from mirbftview.qt.canvas._constants import MSG_COLORS, MSG_COLOR_POOL


def assign_msg_colors(sim, color_pool_idx, known_msgs):
    """No-op: cores agora sao herdadas da request no tick.py."""
    return color_pool_idx


def get_active_msg_color(sim):
    """Retorna a cor da mensagem atualmente em trânsito."""
    for msg in sim.messages:
        if msg.progress < 1.0:
            return msg.color if msg.color else C["text2"]
    if sim.messages:
        msg = sim.messages[-1]
        return msg.color if msg.color else C["text2"]
    return C["node_idle"]


def draw_messages(p, sim, node_pos, client_pos):
    # Agrupa mensagens por par (from, to) para calcular offset
    pair_count: dict[tuple, int] = {}
    pair_index: dict[int, int] = {}

    for i, msg in enumerate(sim.messages):
        src_id = (msg.from_id, msg.from_is_client)
        dst_id = (msg.to_id, msg.to_is_client)
        key = (src_id, dst_id)
        # Também conta o par inverso como mesmo "corredor"
        key_rev = (dst_id, src_id)
        lane_key = min(key, key_rev)

        if lane_key not in pair_count:
            pair_count[lane_key] = 0
        pair_index[i] = pair_count[lane_key]
        pair_count[lane_key] += 1

    # Segundo passo: calcula total por lane
    pair_total: dict[tuple, int] = {}
    for i, msg in enumerate(sim.messages):
        src_id = (msg.from_id, msg.from_is_client)
        dst_id = (msg.to_id, msg.to_is_client)
        key = (src_id, dst_id)
        key_rev = (dst_id, src_id)
        lane_key = min(key, key_rev)
        pair_total[i] = pair_count[lane_key]

    offset_dist = 12  # pixels de separação entre mensagens paralelas

    for i, msg in enumerate(sim.messages):
        src = client_pos.get(msg.from_id) if msg.from_is_client else node_pos.get(msg.from_id)
        dst = client_pos.get(msg.to_id) if msg.to_is_client else node_pos.get(msg.to_id)
        if src is None or dst is None:
            continue

        # Calcula offset perpendicular para não sobrepor
        total = pair_total[i]
        idx = pair_index[i]
        dx = dst.x() - src.x()
        dy = dst.y() - src.y()
        length = math.hypot(dx, dy)

        if length > 0 and total > 1:
            # Vetor perpendicular normalizado
            perp_x = -dy / length
            perp_y = dx / length
            # Centraliza: offset vai de -(total-1)/2 até +(total-1)/2
            shift = (idx - (total - 1) / 2) * offset_dist
            off_x = perp_x * shift
            off_y = perp_y * shift
        else:
            off_x = 0
            off_y = 0

        src_off = QPointF(src.x() + off_x, src.y() + off_y)
        dst_off = QPointF(dst.x() + off_x, dst.y() + off_y)

        t = msg.progress
        x = src_off.x() + (dst_off.x() - src_off.x()) * t
        y = src_off.y() + (dst_off.y() - src_off.y()) * t
        color = QColor(msg.color) if msg.color else QColor(C["text2"])

        # Trail
        trail_c = QColor(color)
        trail_c.setAlpha(50)
        p.setPen(QPen(trail_c, 2.0))
        p.drawLine(src_off, QPointF(x, y))

        # Glow
        glow = QRadialGradient(QPointF(x, y), 14)
        gc = QColor(color)
        gc.setAlpha(80)
        glow.setColorAt(0.0, gc)
        glow.setColorAt(1.0, QColor(0, 0, 0, 0))
        p.setPen(Qt.NoPen)
        p.setBrush(QBrush(glow))
        p.drawEllipse(QPointF(x, y), 14, 14)

        # Partícula
        p.setBrush(color)
        p.drawEllipse(QPointF(x, y), 5, 5)

        # Label
        if msg.label:
            p.setPen(QColor(255, 255, 255, 220))
            p.setFont(QFont("Segoe UI", 7, QFont.Bold))
            p.drawText(QRectF(x - 60, y - 18, 120, 14), Qt.AlignCenter, msg.label)
