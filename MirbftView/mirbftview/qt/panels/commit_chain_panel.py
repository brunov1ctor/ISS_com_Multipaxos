"""CommitChainPanel — cadeia de blocos mostrando o encadeamento."""

from PySide6.QtWidgets import QWidget, QSizePolicy
from PySide6.QtCore import Qt, QRectF, QPointF, QTimer
from PySide6.QtGui import (
    QPainter, QPainterPath, QColor, QLinearGradient, QPen, QBrush, QFont
)

from mirbftview.qt.theme import C
from mirbftview.qt.simulation import Simulation


class CommitChainPanel(QWidget):
    """Panorama visual dos commits — cadeia de blocos."""

    def __init__(self, sim: Simulation, parent=None):
        super().__init__(parent)
        self.sim = sim
        self.setAttribute(Qt.WA_TranslucentBackground)
        self.setSizePolicy(QSizePolicy.Expanding, QSizePolicy.Expanding)
        self.setMinimumHeight(90)
        self.setMouseTracking(True)
        self._scroll_offset = 0.0
        self._dragging = False
        self._drag_start_x = 0.0
        self._drag_start_offset = 0.0
        self._timer = QTimer(self)
        self._timer.setInterval(120)
        self._timer.timeout.connect(self.update)
        self._timer.start()

    def wheelEvent(self, event):
        delta = event.angleDelta().y() or event.angleDelta().x()
        self._scroll_offset += delta * 0.5
        self._clamp_scroll()
        self.update()
        event.accept()

    def mousePressEvent(self, event):
        if event.button() == Qt.LeftButton:
            self._dragging = True
            self._drag_start_x = event.position().x()
            self._drag_start_offset = self._scroll_offset
            self.setCursor(Qt.ClosedHandCursor)
        super().mousePressEvent(event)

    def mouseMoveEvent(self, event):
        if self._dragging:
            dx = event.position().x() - self._drag_start_x
            self._scroll_offset = self._drag_start_offset + dx
            self._clamp_scroll()
            self.update()
        super().mouseMoveEvent(event)

    def mouseReleaseEvent(self, event):
        if event.button() == Qt.LeftButton:
            self._dragging = False
            self.setCursor(Qt.ArrowCursor)
        super().mouseReleaseEvent(event)

    def _clamp_scroll(self):
        history = self.sim.commit_history
        if not history:
            self._scroll_offset = 0
            return
        cell_w = 52 + 6 + 12
        total_content_w = len(history) * cell_w + 10
        visible_w = self.width()
        min_scroll = min(0, visible_w - total_content_w)
        self._scroll_offset = max(min_scroll, min(0, self._scroll_offset))

    def paintEvent(self, event):
        p = QPainter(self)
        p.setRenderHint(QPainter.Antialiasing)
        w, h = self.width(), self.height()

        bg = QPainterPath()
        bg.addRoundedRect(QRectF(0, 0, w, h), 10, 10)
        p.fillPath(bg, QColor(28, 46, 74, 100))
        p.setPen(QPen(QColor(255, 255, 255, 25), 1))
        p.drawPath(bg)

        p.setPen(QColor(C["green"]))
        p.setFont(QFont("Segoe UI", 9, QFont.Bold))
        p.drawText(QRectF(10, 4, 300, 16), Qt.AlignLeft, "\u26d3 Cadeia de Commits (Log Replicado)")

        history = self.sim.commit_history
        if not history:
            p.setPen(QColor(C["text3"]))
            p.setFont(QFont("Segoe UI", 8))
            p.drawText(QRectF(10, 24, w - 20, h - 28), Qt.AlignCenter, "Nenhum commit ainda...")
            p.end()
            return

        top_y = 24
        block_h = h - top_y - 10
        block_w = 52
        gap = 6
        arrow_w = 12
        cell_w = block_w + gap + arrow_w
        start_x = 10 + self._scroll_offset

        leader_colors = ["#F97316", "#6C63FF", "#34D399", "#EF4444", "#A855F7",
                         "#58D8FF", "#FACC15", "#8B7DFF"]

        for i, entry in enumerate(history):
            x = start_x + i * cell_w
            if x + block_w < 0 or x > w:
                continue
            lc = QColor(leader_colors[entry["leader"] % len(leader_colors)])
            self._draw_commit_block(p, x, top_y, block_w, block_h, entry, lc)
            if i < len(history) - 1:
                self._draw_chain_arrow(p, x + block_w + 2, top_y + block_h / 2, arrow_w, w)

        p.setPen(QColor(C["text2"]))
        p.setFont(QFont("Segoe UI", 7))
        p.drawText(QRectF(w - 80, 4, 70, 16), Qt.AlignRight,
                   f"Total: {len(history)} | E{history[-1]['epoch']}")

        for ev in (self.sim.visual_events if hasattr(self.sim, 'visual_events') else []):
            if ev["type"] == "commit":
                alpha = min(180, ev["ttl"] * 6)
                glow_x = start_x + (len(history) - 1) * cell_w
                if 0 < glow_x < w:
                    glow_c = QColor(C["green"])
                    glow_c.setAlpha(alpha)
                    glow_rect = QRectF(glow_x - 4, top_y - 4, block_w + 8, block_h + 8)
                    glow_path = QPainterPath()
                    glow_path.addRoundedRect(glow_rect, 8, 8)
                    p.setPen(QPen(glow_c, 2.5))
                    p.setBrush(Qt.NoBrush)
                    p.drawPath(glow_path)
                break
        p.end()

    def _draw_commit_block(self, p, x, y, block_w, block_h, entry, lc):
        block_path = QPainterPath()
        block_path.addRoundedRect(QRectF(x, y, block_w, block_h), 6, 6)

        grad = QLinearGradient(x, y, x, y + block_h)
        c1 = QColor(lc)
        c1.setAlpha(50)
        c2 = QColor(lc)
        c2.setAlpha(20)
        grad.setColorAt(0.0, c1)
        grad.setColorAt(1.0, c2)
        p.fillPath(block_path, QBrush(grad))

        border_c = QColor(lc)
        border_c.setAlpha(140)
        p.setPen(QPen(border_c, 1.2))
        p.setBrush(Qt.NoBrush)
        p.drawPath(block_path)

        if entry["is_cross"]:
            p.setPen(Qt.NoPen)
            p.setBrush(QColor("#D500F9"))
            p.drawEllipse(QPointF(x + block_w - 6, y + 6), 3, 3)

        p.setPen(QColor(C["text"]))
        p.setFont(QFont("Consolas", 9, QFont.Bold))
        p.drawText(QRectF(x, y + 2, block_w, 16), Qt.AlignCenter, f"SN{entry['sn']}")

        p.setPen(lc)
        p.setFont(QFont("Segoe UI", 6))
        p.drawText(QRectF(x, y + 17, block_w, 10), Qt.AlignCenter, f"N{entry['leader']}")

        p.setPen(QColor(C["text3"]))
        p.setFont(QFont("Consolas", 6))
        p.drawText(QRectF(x, y + 28, block_w, 10), Qt.AlignCenter, entry["hash"])

        if block_h > 48:
            p.setPen(QColor(C["text3"]))
            p.setFont(QFont("Segoe UI", 6))
            p.drawText(QRectF(x, y + block_h - 12, block_w, 10), Qt.AlignCenter, f"E{entry['epoch']}")

    def _draw_chain_arrow(self, p, ax, ay, arrow_w, max_w):
        if ax >= max_w:
            return
        p.setPen(QPen(QColor(C["text3"]), 1.5))
        p.drawLine(QPointF(ax, ay), QPointF(ax + arrow_w - 4, ay))
        p.setBrush(QColor(C["text3"]))
        p.setPen(Qt.NoPen)
        arrow_path = QPainterPath()
        arrow_path.moveTo(ax + arrow_w - 4, ay - 3)
        arrow_path.lineTo(ax + arrow_w, ay)
        arrow_path.lineTo(ax + arrow_w - 4, ay + 3)
        arrow_path.closeSubpath()
        p.fillPath(arrow_path, QColor(C["text3"]))
