"""GlobalOrderPanel — visualização da Ordem Global Atômica."""

import math
from PySide6.QtWidgets import QWidget, QSizePolicy
from PySide6.QtCore import Qt, QRectF, QPointF, QTimer
from PySide6.QtGui import (
    QPainter, QPainterPath, QColor, QLinearGradient, QRadialGradient,
    QPen, QBrush, QFont
)

from mirbftview.qt.theme import C
from mirbftview.qt.simulation import Simulation


class GlobalOrderPanel(QWidget):
    """Visualizacao da Ordem Global Atomica — timeline com setas e efeitos visuais."""

    _GROUP_COLORS = ["#EF4444", "#6C63FF", "#34D399", "#F97316", "#A855F7", "#58D8FF"]

    def __init__(self, sim: Simulation, parent=None):
        super().__init__(parent)
        self.sim = sim
        self.setAttribute(Qt.WA_TranslucentBackground)
        self.setSizePolicy(QSizePolicy.Expanding, QSizePolicy.Expanding)
        self.setMinimumHeight(100)
        self.setMouseTracking(True)
        self._event_rects: list[tuple[QRectF, dict]] = []
        self._popup: dict | None = None
        self._timer = QTimer(self)
        self._timer.setInterval(120)
        self._timer.timeout.connect(self.update)
        self._timer.start()

    def mousePressEvent(self, event):
        if event.button() == Qt.LeftButton:
            pos = event.position()
            clicked = None
            for rect, entry in self._event_rects:
                if rect.contains(pos):
                    clicked = entry
                    break
            if clicked:
                self._popup = {"entry": clicked, "pos": pos}
            else:
                self._popup = None
            self.update()
        super().mousePressEvent(event)

    def mouseMoveEvent(self, event):
        pos = event.position()
        hovering = any(r.contains(pos) for r, _ in self._event_rects)
        self.setCursor(Qt.PointingHandCursor if hovering else Qt.ArrowCursor)
        super().mouseMoveEvent(event)

    def paintEvent(self, event):
        p = QPainter(self)
        p.setRenderHint(QPainter.Antialiasing)
        w, h = self.width(), self.height()
        s = self.sim

        bg = QPainterPath()
        bg.addRoundedRect(QRectF(0, 0, w, h), 10, 10)
        p.fillPath(bg, QColor(28, 46, 74, 100))
        p.setPen(QPen(QColor(255, 255, 255, 25), 1))
        p.drawPath(bg)

        p.setPen(QColor("#A855F7"))
        p.setFont(QFont("Segoe UI", 9, QFont.Bold))
        p.drawText(QRectF(10, 4, 300, 16), Qt.AlignLeft, "Ordem Global Atomica (ADeliver)")

        history = s.delivery.delivery_history if s.delivery else []
        if not history:
            p.setPen(QColor(C["text3"]))
            p.setFont(QFont("Segoe UI", 8))
            p.drawText(QRectF(10, 24, w - 20, h - 28), Qt.AlignCenter, "Aguardando entregas...")
            p.end()
            return

        top_y = 24
        groups = s.groups
        lane_h = max(16, min(26, (h - top_y - 24) / max(len(groups), 1)))
        lanes_bottom = top_y + len(groups) * lane_h

        for i, grp in enumerate(groups):
            ly = top_y + i * lane_h
            gc = QColor(self._GROUP_COLORS[i % len(self._GROUP_COLORS)])
            is_seq = (grp.id == 0)

            lane_rect = QRectF(54, ly + 1, w - 64, lane_h - 2)
            lane_grad = QLinearGradient(54, ly, w - 10, ly)
            c1 = QColor(gc)
            c1.setAlpha(12 if is_seq else 20)
            c2 = QColor(gc)
            c2.setAlpha(5 if is_seq else 8)
            lane_grad.setColorAt(0.0, c1)
            lane_grad.setColorAt(1.0, c2)
            lane_path = QPainterPath()
            lane_path.addRoundedRect(lane_rect, 3, 3)
            p.fillPath(lane_path, QBrush(lane_grad))

            bc = QColor(gc)
            bc.setAlpha(40)
            pen_style = Qt.DashLine if is_seq else Qt.SolidLine
            p.setPen(QPen(bc, 0.5, pen_style))
            p.setBrush(Qt.NoBrush)
            p.drawPath(lane_path)

            p.setPen(gc)
            p.setFont(QFont("Segoe UI", 7, QFont.Bold))
            label = "SEQ" if is_seq else f"G{grp.id}"
            p.drawText(QRectF(4, ly, 48, lane_h), Qt.AlignVCenter | Qt.AlignRight, label)

        arrow_y = lanes_bottom + 4
        p.setPen(QPen(QColor(C["text3"]), 1.5))
        p.drawLine(QPointF(54, arrow_y), QPointF(w - 16, arrow_y))
        p.setPen(Qt.NoPen)
        p.setBrush(QColor(C["text3"]))
        arr = QPainterPath()
        arr.moveTo(w - 16, arrow_y)
        arr.lineTo(w - 22, arrow_y - 3)
        arr.lineTo(w - 22, arrow_y + 3)
        arr.closeSubpath()
        p.fillPath(arr, QColor(C["text3"]))
        p.setPen(QColor(C["text3"]))
        p.setFont(QFont("Segoe UI", 6))
        p.drawText(QRectF(w - 50, arrow_y + 2, 40, 10), Qt.AlignRight, "tempo")

        visible = history[-16:]
        event_w = max(22, min(40, (w - 74) / max(len(visible), 1)))
        start_x = 60
        self._event_rects = []

        for idx, entry in enumerate(visible):
            ex = start_x + idx * event_w
            if ex > w - 20:
                break
            cx = ex + event_w / 2

            hit_rect = QRectF(ex, top_y - 14, event_w, lanes_bottom - top_y + 20)
            self._event_rects.append((hit_rect, entry))

            gsn = entry["gsn"]
            touched_groups = entry["groups"]
            is_cross = entry["type"] == "cross"
            is_last = (idx == len(visible) - 1)
            color = QColor("#D500F9") if is_cross else QColor(C["green"])

            lane_indices = []
            for gi, grp in enumerate(groups):
                if grp.id in touched_groups:
                    lane_indices.append(gi)
            if is_cross and 0 not in lane_indices:
                lane_indices.insert(0, 0)
            if not lane_indices:
                continue

            min_li = min(lane_indices)
            max_li = max(lane_indices)
            y1 = top_y + min_li * lane_h + lane_h / 2
            y2 = top_y + max_li * lane_h + lane_h / 2

            if is_last:
                glow_y = (y1 + y2) / 2
                glow = QRadialGradient(QPointF(cx, glow_y), 18)
                gc = QColor(color)
                gc.setAlpha(60)
                glow.setColorAt(0.0, gc)
                glow.setColorAt(1.0, QColor(0, 0, 0, 0))
                p.setPen(Qt.NoPen)
                p.setBrush(QBrush(glow))
                p.drawEllipse(QPointF(cx, glow_y), 18, 18)

            if is_cross and min_li != max_li:
                p.setPen(QPen(color, 2.5))
                p.drawLine(QPointF(cx, y1), QPointF(cx, y2))
                p.setPen(Qt.NoPen)
                p.setBrush(color)
                da = QPainterPath()
                da.moveTo(cx, y2 + 4)
                da.lineTo(cx - 3, y2 - 1)
                da.lineTo(cx + 3, y2 - 1)
                da.closeSubpath()
                p.fillPath(da, color)
                ua = QPainterPath()
                ua.moveTo(cx, y1 - 4)
                ua.lineTo(cx - 3, y1 + 1)
                ua.lineTo(cx + 3, y1 + 1)
                ua.closeSubpath()
                p.fillPath(ua, color)

            for li in lane_indices:
                dy = top_y + li * lane_h + lane_h / 2
                is_seq_lane = (li == 0)
                if is_seq_lane:
                    diamond = QPainterPath()
                    diamond.moveTo(cx, dy - 6)
                    diamond.lineTo(cx + 5, dy)
                    diamond.lineTo(cx, dy + 6)
                    diamond.lineTo(cx - 5, dy)
                    diamond.closeSubpath()
                    p.setPen(QPen(color, 1.5))
                    p.setBrush(Qt.NoBrush)
                    p.drawPath(diamond)
                    p.setPen(Qt.NoPen)
                    p.setBrush(color)
                    p.drawEllipse(QPointF(cx, dy), 2, 2)
                else:
                    ring_c = QColor(color)
                    ring_c.setAlpha(80)
                    p.setPen(QPen(ring_c, 1.5))
                    p.setBrush(Qt.NoBrush)
                    p.drawEllipse(QPointF(cx, dy), 7, 7)
                    p.setPen(Qt.NoPen)
                    p.setBrush(color)
                    p.drawEllipse(QPointF(cx, dy), 4, 4)

            if idx < len(visible) - 1:
                next_cx = start_x + (idx + 1) * event_w + event_w / 2
                mid_y = arrow_y
                arr_color = QColor(C["text3"])
                arr_color.setAlpha(100)
                p.setPen(QPen(arr_color, 1))
                p.drawLine(QPointF(cx + 8, mid_y), QPointF(next_cx - 8, mid_y))
                p.setPen(Qt.NoPen)
                p.setBrush(arr_color)
                sa = QPainterPath()
                sa.moveTo(next_cx - 8, mid_y)
                sa.lineTo(next_cx - 12, mid_y - 2)
                sa.lineTo(next_cx - 12, mid_y + 2)
                sa.closeSubpath()
                p.fillPath(sa, arr_color)

            p.setPen(color)
            p.setFont(QFont("Consolas", 7, QFont.Bold))
            lbl = f"{gsn}" if is_cross else "s"
            p.drawText(QRectF(ex, top_y - 14, event_w, 12), Qt.AlignCenter, lbl)

            p.setPen(QColor(C["text3"]))
            p.setFont(QFont("Consolas", 6))
            p.drawText(QRectF(ex, arrow_y + 2, event_w, 10), Qt.AlignCenter, f"sn{entry['sn']}")

        # Legend
        legend_y = h - 14
        p.setFont(QFont("Segoe UI", 6))
        p.setPen(QColor("#D500F9"))
        p.drawText(QRectF(10, legend_y, 80, 12), Qt.AlignLeft, "# = cross-op")
        p.setPen(QColor(C["green"]))
        p.drawText(QRectF(80, legend_y, 80, 12), Qt.AlignLeft, "s = single-op")
        p.setPen(QColor(C["text3"]))
        p.drawText(QRectF(150, legend_y, 100, 12), Qt.AlignLeft, "<> = SEQ publica GSN")
        p.setPen(QColor(C["text3"]))
        p.drawText(QRectF(260, legend_y, 80, 12), Qt.AlignLeft, "-> = ordem")

        blocked_count = 0
        if s.delivery:
            for gid in s.delivery._last_delivered_gsn:
                blocked_count += len(s.delivery.get_blocked_entries(gid))
        if blocked_count > 0:
            p.setPen(QColor(C["red"]))
            p.setFont(QFont("Segoe UI", 7, QFont.Bold))
            p.drawText(QRectF(w - 110, 4, 100, 16), Qt.AlignRight, f"BLOQUEADOS: {blocked_count}")

        if self._popup:
            self._draw_event_popup(p, w, h)

        p.end()

    def _draw_event_popup(self, p, w, h):
        entry = self._popup["entry"]
        pos = self._popup["pos"]

        is_cross = entry["type"] == "cross"
        gsn = entry["gsn"]
        sn = entry["sn"]
        groups = entry["groups"]
        color = QColor("#D500F9") if is_cross else QColor(C["green"])

        lines = [
            f"{'Cross-Group Op' if is_cross else 'Single-Group Op'}",
            f"SN: {sn}",
            f"GSN: {gsn}" if gsn > 0 else "GSN: (nenhum)",
            f"Tipo: {entry['type']}",
            f"Grupos: {groups}",
        ]
        if is_cross:
            lines.append(f"Coordenacao: SEQ publica GSN={gsn}")
            lines.append("Todos os grupos entregam na ordem GSN")
        else:
            lines.append(f"Entrega direta no grupo G{groups[0] if groups else '?'}")

        p.setFont(QFont("Segoe UI", 8))
        fm = p.fontMetrics()
        line_h = fm.height() + 2
        popup_w = 200
        popup_h = line_h * len(lines) + 14

        px = min(pos.x() + 10, w - popup_w - 6)
        py = max(6, pos.y() - popup_h - 10)
        if py < 6:
            py = pos.y() + 16

        rect = QRectF(px, py, popup_w, popup_h)
        bg = QPainterPath()
        bg.addRoundedRect(rect, 8, 8)
        p.fillPath(bg, QColor(10, 18, 32, 240))
        p.setPen(QPen(color, 1.5))
        p.setBrush(Qt.NoBrush)
        p.drawPath(bg)

        y = py + 8
        for i, line in enumerate(lines):
            if i == 0:
                p.setPen(color)
                p.setFont(QFont("Segoe UI", 9, QFont.Bold))
            else:
                p.setPen(QColor(C["text"]))
                p.setFont(QFont("Consolas", 7))
            p.drawText(QRectF(px + 10, y, popup_w - 20, line_h),
                       Qt.AlignLeft | Qt.AlignVCenter, line)
            y += line_h
