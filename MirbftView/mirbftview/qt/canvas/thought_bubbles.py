"""Thought bubbles — balões de pensamento nos nós."""

from PySide6.QtCore import Qt, QPointF, QRectF
from PySide6.QtGui import QPainter, QPainterPath, QColor, QPen, QFont

from mirbftview.qt.theme import C
from mirbftview.qt.simulation import MsgType
from mirbftview.qt.canvas._constants import MSG_COLORS


def update_thought_bubbles(sim, thought_bubbles):
    """Atualiza TTL e gera novos bubbles para mensagens que chegaram."""
    expired = [nid for nid, b in thought_bubbles.items() if b["ttl"] <= 0]
    for nid in expired:
        del thought_bubbles[nid]
    for nid in thought_bubbles:
        thought_bubbles[nid]["ttl"] -= 1

    for msg in sim.messages:
        if msg.progress < 1.0 or msg.to_is_client:
            continue
        text, color, is_error = _bubble_for_msg(sim, msg)
        if text:
            thought_bubbles[msg.to_id] = {
                "text": text, "color": color, "is_error": is_error, "ttl": 45
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

    color = msg.color if msg.color else MSG_COLORS.get(msg.msg_type, C["text2"])
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
    if text:
        if msg.msg_type == MsgType.ACCEPT and msg.label and "RE-" in msg.label:
            text = f"handleAccept()\n[RETRANSMIT] sn={msg.sn}"
            is_error = True
            color = C["red"]
        return text, color, is_error
    return "", C["text"], False


def draw_thought_bubbles(p, thought_bubbles, node_pos):
    for nid, bubble in thought_bubbles.items():
        pos = node_pos.get(nid)
        if pos is None:
            continue
        text = bubble["text"]
        color = bubble["color"]
        is_error = bubble.get("is_error", False)
        ttl = bubble["ttl"]
        alpha = min(255, ttl * 6)
        if alpha <= 0:
            continue

        lines = text.split("\n")
        p.setFont(QFont("Consolas", 7))
        fm = p.fontMetrics()
        line_h = fm.height() + 1
        text_w = max(fm.horizontalAdvance(l) for l in lines) + 16
        bubble_w = max(80, min(160, text_w))
        bubble_h = line_h * len(lines) + 10
        bx = pos.x() + 20
        by = pos.y() - 50 - bubble_h

        bg_path = QPainterPath()
        bg_path.addRoundedRect(QRectF(bx, by, bubble_w, bubble_h), 8, 8)
        bg_color = QColor(40, 10, 10, alpha) if is_error else QColor(12, 20, 38, alpha)
        p.fillPath(bg_path, bg_color)

        border_c = QColor(color)
        border_c.setAlpha(alpha)
        p.setPen(QPen(border_c, 1.5 if is_error else 1.0))
        p.setBrush(Qt.NoBrush)
        p.drawPath(bg_path)

        p.setPen(Qt.NoPen)
        dot_c = QColor(color)
        dot_c.setAlpha(alpha)
        p.setBrush(dot_c)
        dx = bx - pos.x()
        dy = by + bubble_h - pos.y()
        for frac, r in [(0.3, 2.5), (0.55, 3.5), (0.8, 2.0)]:
            cx = pos.x() + 24 + dx * frac * 0.3
            cy = pos.y() - 14 + dy * frac
            p.drawEllipse(QPointF(cx, cy), r, r)

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
            p.drawText(QRectF(bx + 6, ty, bubble_w - 12, line_h),
                       Qt.AlignLeft | Qt.AlignVCenter, line)
            ty += line_h
