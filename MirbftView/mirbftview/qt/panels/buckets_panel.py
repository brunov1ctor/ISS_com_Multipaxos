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
                   f"Regra: bucket = (clientID + clientSN) mod {num} (buckets sao POR GRUPO)")

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

        # Nota: este painel mostra um pool compartilhado de indices de bucket
        # (0..num-1) por simplicidade visual. Na implementacao real, cada
        # grupo de dados tem seu PROPRIO conjunto de buckets (a formula
        # (clientID+clientSN) mod numBuckets e aplicada dentro do grupo para
        # onde a chave da requisicao ja foi roteada) — o grupo de cada
        # requisicao aparece no seu proprio evento/bolha, nao no indice do
        # bucket em si, que nao tem "dono" fixo aqui.
        bucket_color = QColor(C["accent"])
        self._bucket_rects = []

        for i in range(num):
            contents = self.sim.bucket_contents[i] if i < len(self.sim.bucket_contents) else []
            x = start_x + i * (bucket_w + spacing)
            gc = bucket_color

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

            self._draw_bucket_labels(p, bx, by, bucket_w, bucket_h, i, gc, is_selected, contents)

            # Glow on bucket_in event
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

            # Thought bubble para este bucket (waitForRequests / CutBatch)
            bubble = self.sim.bucket_bubbles.get(i) if hasattr(self.sim, 'bucket_bubbles') else None
            if bubble and bubble["ttl"] > 0:
                self._draw_bucket_bubble(p, bx, by, bucket_w, bucket_h, bubble)

        if 0 <= self._selected_bucket < num:
            self._draw_bucket_detail(p, w, h)

        p.end()

    def _draw_bucket_bubble(self, p, bx, by, bucket_w, bucket_h, bubble):
        """Desenha thought bubble estilo balão de pensamento acima do bucket."""
        text = bubble["text"]
        color_key = bubble["color"]
        ttl = bubble["ttl"]
        alpha = min(240, ttl * 8)
        if alpha <= 0:
            return

        color = C.get(color_key, color_key)
        lines = text.split("\n")
        p.setFont(QFont("Consolas", 6))
        fm = p.fontMetrics()
        line_h = fm.height() + 1
        text_w = max(fm.horizontalAdvance(l) for l in lines) + 12
        bubble_w = max(70, min(140, text_w))
        bubble_h = line_h * len(lines) + 8

        # Posiciona acima do bucket
        bbx = bx + bucket_w / 2 - bubble_w / 2
        bby = by - bubble_h - 16

        # Clamp dentro do widget
        if bbx < 2:
            bbx = 2
        if bbx + bubble_w > self.width() - 2:
            bbx = self.width() - bubble_w - 2

        # Fundo do balão
        bg_path = QPainterPath()
        bg_path.addRoundedRect(QRectF(bbx, bby, bubble_w, bubble_h), 6, 6)
        bg_color = QColor(12, 20, 38, alpha)
        p.fillPath(bg_path, bg_color)

        # Borda
        border_c = QColor(color)
        border_c.setAlpha(alpha)
        p.setPen(QPen(border_c, 1.2))
        p.setBrush(Qt.NoBrush)
        p.drawPath(bg_path)

        # Bolinhas de pensamento (conectam balão ao bucket)
        p.setPen(Qt.NoPen)
        dot_c = QColor(color)
        dot_c.setAlpha(alpha)
        p.setBrush(dot_c)
        anchor_x = bx + bucket_w / 2
        anchor_y = by
        for frac, r in [(0.25, 2.0), (0.55, 3.0), (0.82, 2.0)]:
            cx = anchor_x + (bbx + bubble_w / 2 - anchor_x) * frac * 0.4
            cy = anchor_y + (bby + bubble_h - anchor_y) * frac
            p.drawEllipse(QPointF(cx, cy), r, r)

        # Texto
        ty = bby + 4
        for idx, line in enumerate(lines):
            if idx == 0:
                tc = QColor(color)
                tc.setAlpha(alpha)
                p.setPen(tc)
                p.setFont(QFont("Consolas", 6, QFont.Bold))
            else:
                tc = QColor(C["text"])
                tc.setAlpha(alpha)
                p.setPen(tc)
                p.setFont(QFont("Consolas", 6))
            p.drawText(QRectF(bbx + 5, ty, bubble_w - 10, line_h),
                       Qt.AlignLeft | Qt.AlignVCenter, line)
            ty += line_h

    def _draw_bucket_labels(self, p, bx, by, bucket_w, bucket_h, i, gc, is_selected, contents):
        p.setPen(QColor(C["text"] if is_selected else C["text2"]))
        p.setFont(QFont("Segoe UI", 7, QFont.Bold))
        p.drawText(QRectF(bx, by + bucket_h + 2, bucket_w, 12), Qt.AlignCenter, f"B{i}")

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

        fill_count = self.sim.batch_fill.get(bid, 0) if hasattr(self.sim, 'batch_fill') else 0
        batch_size = self.sim.batch_visual_size if hasattr(self.sim, 'batch_visual_size') else 3

        lines = [
            f"Bucket {bid}",
            f"Pedidos na fila: {len(contents)}",
            f"Batch fill: {fill_count}/{batch_size}",
            f"Formula: (clientID + clientSN) mod {self.sim.num_buckets} = {bid}",
            f"(cada grupo tem seu proprio conjunto de buckets;",
            f" o grupo de cada pedido aparece no evento dele)",
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
