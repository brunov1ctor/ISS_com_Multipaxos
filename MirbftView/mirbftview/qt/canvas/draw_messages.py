"""Renderização de mensagens (partículas, trails, cores)."""

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
            return msg.color if msg.color else MSG_COLORS.get(msg.msg_type, C["text2"])
    if sim.messages:
        msg = sim.messages[-1]
        return msg.color if msg.color else MSG_COLORS.get(msg.msg_type, C["text2"])
    return C["node_idle"]


def draw_messages(p, sim, node_pos, client_pos):
    for msg in sim.messages:
        src = client_pos.get(msg.from_id) if msg.from_is_client else node_pos.get(msg.from_id)
        dst = client_pos.get(msg.to_id) if msg.to_is_client else node_pos.get(msg.to_id)
        if src is None or dst is None:
            continue

        t = msg.progress
        x = src.x() + (dst.x() - src.x()) * t
        y = src.y() + (dst.y() - src.y()) * t
        color = QColor(msg.color if msg.color else MSG_COLORS.get(msg.msg_type, C["text2"]))

        trail_c = QColor(color)
        trail_c.setAlpha(50)
        p.setPen(QPen(trail_c, 2.0))
        p.drawLine(src, QPointF(x, y))

        glow = QRadialGradient(QPointF(x, y), 16)
        gc = QColor(color)
        gc.setAlpha(100)
        glow.setColorAt(0.0, gc)
        glow.setColorAt(1.0, QColor(0, 0, 0, 0))
        p.setPen(Qt.NoPen)
        p.setBrush(QBrush(glow))
        p.drawEllipse(QPointF(x, y), 16, 16)

        p.setBrush(color)
        p.drawEllipse(QPointF(x, y), 5, 5)

        if msg.label:
            p.setPen(QColor(255, 255, 255, 220))
            p.setFont(QFont("Segoe UI", 7, QFont.Bold))
            p.drawText(QRectF(x - 60, y - 20, 120, 14), Qt.AlignCenter, msg.label)
