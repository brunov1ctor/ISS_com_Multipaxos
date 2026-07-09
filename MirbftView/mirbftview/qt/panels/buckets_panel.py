"""BucketsPanel — visualização gráfica dos buckets."""

import math
from PySide6.QtWidgets import QWidget, QSizePolicy
from PySide6.QtCore import Qt, QRectF, QPointF, QTimer
from PySide6.QtGui import (
    QPainter, QPainterPath, QColor, QLinearGradient, QPen, QBrush, QFont
)

from mirbftview.qt.theme import C
from mirbftview.qt.simulation import Simulation


class BucketsPanel(QWidget):
    """Visualização gráfica dos buckets — baldes com requests dentro."""

    def __init__(self, sim: Simulation, parent=None):
        super().__init__(parent)
        self.sim = sim
        self.setAttribute(Qt.WA_TranslucentBackground)
        self.setMinimumHeight(140)
        self.setSizePolicy(QSizePolicy.Expanding, QSizePolicy.Expanding)
        self.setMouseTracking(True)
        self._bucket_rects: list[QRectF] = []
        self._selected_bucket: int = -1
        self._timer = QTimer(self)
        self._timer.setInterval(100)
        self._timer.timeout.connect(self.update)
        self._timer.start()

    def mousePressEvent(self, event):
        if event.button() == Qt.LeftButton:
            pos = event.position()
            clicked = -1
            for i, rect in enumerate(self._bucket_rects):
                if rect.contains(pos):
                    clicked = i
                    break
            if clicked == self._selected_bucket:
                self._selected_bucket = -1
            else:
                self._selected_bucket = clicked
            self.update()
        super().mousePressEvent(event)

    def mouseMoveEvent(self, event):
        pos = event.position()
        hovering = any(r.contains(pos) for r in self._bucket_rects)
        self.setCursor(Qt.PointingHandCursor if hovering else Qt.ArrowCursor)
        super().mouseMoveEvent(event)

    def paintEvent(self, event):
        p = QPainter(self)
        p.setRenderHint(QPainter.Antialiasing)
        w, h = self.width(), self.height()
        num = self.sim.num_buckets
        if num == 0:
            p.end()
            return

        p.setPen(QColor(C["gold"]))
        p.setFont(QFont("Segoe UI", 10, QFont.Bold))
        p.drawText(QRectF(10, 4, w, 18), Qt.AlignLeft, "\U0001faa3 Buckets (clique para detalhes)")

        p.setPen(QColor(C["text3"]))
        p.setFont(QFont("Segoe UI", 7))
        p.drawText(QRectF(10, h - 16, w - 20, 14), Qt.AlignLeft,
                   f"Regra: bucket = (clientID + clientSN) mod {num}")

        top_y = 26
        bot_y = h - 22
        available_h = bot_y - top_y
        margin_x = 14
        available_w = w - margin_x * 2
        bucket_w = max(28, min(70, available_w / num - 4))
        total_w = bucket_w * num
        spacing = (available_w - total_w) / max(num - 1, 1) if num > 1 else 0
        spacing = max(2, min(spacing, 12))
        total_w = bucket_w * num + spacing * (num - 1)
        start_x = margin_x + (available_w - total_w) / 2

        group_colors = ["#EF4444", "#6C63FF", "#34D399", "#F97316", "#A855F7", "#58D8FF", "#FACC15", "#8B7DFF"]
        self._bucket_rects = []

        for i in range(num):
            contents = self.sim.bucket_contents[i] if i < len(self.sim.bucket_contents) else []
            x = start_x + i * (bucket_w + spacing)
            group_id = 1 + (i % max(1, len(self.sim.groups) - 1))
            gc = QColor(group_colors[group_id % len(group_colors)])

            bucket_h = available_h - 20
            bx, by = x, top_y + 14
            taper = 4

            self._bucket_rects.append(QRectF(bx, by, bucket_w, bucket_h))
            is_selected = (i == self._selected_bucket)

            path = QPainterPath()
            path.moveTo(bx, by)
            path.lineTo(bx + bucket_w, by)
            path.lineTo(bx + bucket_w - taper, by + bucket_h)
            path.lineTo(bx + taper, by + bucket_h)
            path.closeSubpath()

            grad = QLinearGradient(bx, by, bx, by + bucket_h)
            base = QColor(gc)
            base.setAlpha(60 if is_selected else 30)
            grad.setColorAt(0.0, QColor(gc.red(), gc.green(), gc.blue(), 30 if is_selected else 15))
            grad.setColorAt(1.0, base)
            p.fillPath(path, QBrush(grad))

            border = QColor(gc)
            border.setAlpha(220 if is_selected else 120)
            p.setPen(QPen(border, 2.5 if is_selected else 1.5))
            p.setBrush(Qt.NoBrush)
            p.drawPath(path)

            max_visible = 5
            block_h = max(10, min(18, (bucket_h - 8) / max(max_visible, 1)))
            visible = contents[-max_visible:]
            for j, req_label in enumerate(reversed(visible)):
                ry = by + bucket_h - 4 - (j + 1) * (block_h + 2)
                rx = bx + taper + 2
                rw = bucket_w - taper * 2 - 4
                if ry < by + 2:
                    break
                req_path = QPainterPath()
                req_path.addRoundedRect(QRectF(rx, ry, rw, block_h), 3, 3)
                req_color = QColor(gc)
                req_color.setAlpha(160)
                p.fillPath(req_path, req_color)
                p.setPen(QColor(0, 0, 0, 200))
                p.setFont(QFont("Consolas", 6))
                p.drawText(QRectF(rx, ry, rw, block_h), Qt.AlignCenter, req_label[:10])

            if len(contents) > max_visible:
                p.setPen(QColor(255, 255, 255, 140))
                p.setFont(QFont("Segoe UI", 6))
                p.drawText(QRectF(bx, by + 2, bucket_w, 10), Qt.AlignCenter, f"+{len(contents) - max_visible}")

            self._draw_bucket_labels(p, bx, by, bucket_w, bucket_h, top_y, i, group_id, gc, is_selected, contents)

            fill_count = self.sim.batch_fill.get(i, 0) if hasattr(self.sim, 'batch_fill') else 0
            batch_size = self.sim.batch_visual_size if hasattr(self.sim, 'batch_visual_size') else 3
            if fill_count > 0:
                fill_pct = min(1.0, fill_count / max(batch_size, 1))
                bar_w = 3
                bar_x_pos = bx + bucket_w + 1
                bar_full_h = bucket_h
                bar_fill_h = bar_full_h * fill_pct
                p.setPen(Qt.NoPen)
                p.setBrush(QColor(255, 255, 255, 15))
                p.drawRoundedRect(QRectF(bar_x_pos, by, bar_w, bar_full_h), 1, 1)
                fill_color = QColor(C["green"]) if fill_pct >= 1.0 else QColor(gc)
                fill_color.setAlpha(200 if fill_pct >= 1.0 else 140)
                p.setBrush(fill_color)
                p.drawRoundedRect(QRectF(bar_x_pos, by + bar_full_h - bar_fill_h, bar_w, bar_fill_h), 1, 1)
                if fill_pct >= 1.0:
                    p.setPen(QColor(C["green"]))
                    p.setFont(QFont("Segoe UI", 5, QFont.Bold))
                    p.drawText(QRectF(bx, by - 10, bucket_w, 9), Qt.AlignCenter, "CORTE")

            for ev in (self.sim.visual_events if hasattr(self.sim, 'visual_events') else []):
                if ev.get("bucket") != i:
                    continue
                alpha = min(200, ev["ttl"] * 7)
                if ev["type"] == "bucket_in":
                    glow_c = QColor(C["gold"])
                    glow_c.setAlpha(alpha)
                    glow_path = QPainterPath()
                    glow_path.addRoundedRect(QRectF(bx - 2, by - 2, bucket_w + 4, bucket_h + 4), 6, 6)
                    p.setPen(QPen(glow_c, 2))
                    p.setBrush(Qt.NoBrush)
                    p.drawPath(glow_path)
                elif ev["type"] == "batch_cut":
                    flash_c = QColor(C["green"])
                    flash_c.setAlpha(alpha)
                    p.setPen(Qt.NoPen)
                    p.setBrush(flash_c)
                    p.drawRoundedRect(QRectF(bx, by, bucket_w, bucket_h), 4, 4)

        if 0 <= self._selected_bucket < num:
            self._draw_bucket_detail(p, w, h)

        p.end()

    def _draw_bucket_labels(self, p, bx, by, bucket_w, bucket_h, top_y, i, group_id, gc, is_selected, contents):
        p.setPen(QColor(C["text"] if is_selected else C["text2"]))
        p.setFont(QFont("Segoe UI", 7, QFont.Bold))
        p.drawText(QRectF(bx, by + bucket_h + 2, bucket_w, 12), Qt.AlignCenter, f"B{i}")

        p.setPen(gc)
        p.setFont(QFont("Segoe UI", 6))
        p.drawText(QRectF(bx, top_y, bucket_w, 12), Qt.AlignCenter, f"\u2192G{group_id}")

        if contents:
            badge_r = 8
            badge_x = bx + bucket_w - badge_r
            badge_y = by - badge_r + 2
            p.setPen(Qt.NoPen)
            p.setBrush(QColor(gc))
            p.drawEllipse(QPointF(badge_x, badge_y), badge_r, badge_r)
            p.setPen(QColor(0, 0, 0))
            p.setFont(QFont("Segoe UI", 6, QFont.Bold))
            p.drawText(QRectF(badge_x - badge_r, badge_y - badge_r, badge_r * 2, badge_r * 2),
                       Qt.AlignCenter, str(len(contents)))

    def _draw_bucket_detail(self, p, w, h):
        bid = self._selected_bucket
        contents = self.sim.bucket_contents[bid] if bid < len(self.sim.bucket_contents) else []

        leader = "?"
        if hasattr(self.sim, 'epoch_mgr') and self.sim.epoch_mgr:
            ba = self.sim.epoch_mgr.bucket_assignment
            for lid, bkts in ba.items():
                if bid in bkts:
                    leader = f"Node {lid}"
                    break

        group_id = 1 + (bid % max(1, len(self.sim.groups) - 1))
        fill_count = self.sim.batch_fill.get(bid, 0) if hasattr(self.sim, 'batch_fill') else 0
        batch_size = self.sim.batch_visual_size if hasattr(self.sim, 'batch_visual_size') else 3

        lines = [
            f"Bucket {bid}",
            f"Lider: {leader}",
            f"Grupo destino: G{group_id}",
            f"Pedidos na fila: {len(contents)}",
            f"Batch fill: {fill_count}/{batch_size}",
            f"Formula: (clientID + clientSN) mod {self.sim.num_buckets} = {bid}",
        ]
        for item in contents[:6]:
            lines.append(f"  {item}")
        if len(contents) > 6:
            lines.append(f"  ... +{len(contents) - 6} mais")
        if not contents:
            lines.append("  (vazio - aguardando requests)")

        p.setFont(QFont("Segoe UI", 8))
        fm = p.fontMetrics()
        line_h = fm.height() + 2
        popup_w = min(220, w - 10)
        popup_h = line_h * len(lines) + 14
        px = w - popup_w - 6
        py = 24

        rect = QRectF(px, py, popup_w, popup_h)
        bg = QPainterPath()
        bg.addRoundedRect(rect, 8, 8)
        p.fillPath(bg, QColor(10, 18, 32, 245))
        p.setPen(QPen(QColor(C["gold"]), 1.5))
        p.drawPath(bg)

        y = py + 8
        for i, line in enumerate(lines):
            if i == 0:
                p.setPen(QColor(C["gold"]))
                p.setFont(QFont("Segoe UI", 9, QFont.Bold))
            elif line.startswith("  "):
                p.setPen(QColor(C["accent"]))
                p.setFont(QFont("Consolas", 7))
            else:
                p.setPen(QColor(C["text"]))
                p.setFont(QFont("Segoe UI", 8))
            p.drawText(QRectF(px + 10, y, popup_w - 20, line_h), Qt.AlignLeft | Qt.AlignVCenter, line)
            y += line_h
