"""CommitChainPanel — cadeia de blocos didática mostrando o log replicado."""

from PySide6.QtWidgets import QWidget, QSizePolicy
from PySide6.QtCore import Qt, QRectF, QPointF, QTimer
from PySide6.QtGui import (
    QPainter, QPainterPath, QColor, QLinearGradient, QPen, QBrush, QFont
)

from mirbftview.qt.theme import C
from mirbftview.qt.simulation import Simulation


class CommitChainPanel(QWidget):
    """Cadeia de commits didática — blocos encadeados com detalhes visuais."""

    def __init__(self, sim: Simulation, parent=None):
        super().__init__(parent)
        self.sim = sim
        self.setAttribute(Qt.WA_TranslucentBackground)
        self.setSizePolicy(QSizePolicy.Expanding, QSizePolicy.Expanding)
        self.setMinimumHeight(110)
        self.setMouseTracking(True)
        self._scroll_offset = 0.0
        self._dragging = False
        self._drag_start_x = 0.0
        self._drag_start_offset = 0.0
        self._hovered_block = -1
        self._clicked_block = -1
        self._new_block_ttl = 0  # animação de entrada
        self._last_count = 0
        self._timer = QTimer(self)
        self._timer.setInterval(60)
        self._timer.timeout.connect(self._on_tick)
        self._timer.start()

    def _on_tick(self):
        if self._new_block_ttl > 0:
            self._new_block_ttl -= 1
        # Detecta novo commit
        history = self.sim.commit_history
        if len(history) > self._last_count:
            self._new_block_ttl = 30
            self._last_count = len(history)
            # Auto-scroll para o final
            self._scroll_to_end()
        self.update()

    def _scroll_to_end(self):
        history = self.sim.commit_history
        if not history:
            return
        cell_w = self._cell_w()
        total_w = len(history) * cell_w + 10
        visible_w = self.width() - 20
        if total_w > visible_w:
            self._scroll_offset = -(total_w - visible_w)

    def _cell_w(self):
        return 68 + 8 + 16  # block_w + gap + arrow_w

    # ─── Eventos de mouse ─────────────────────────────────────────────────

    def wheelEvent(self, event):
        delta = event.angleDelta().y() or event.angleDelta().x()
        self._scroll_offset += delta * 0.5
        self._clamp_scroll()
        self.update()
        event.accept()

    def mousePressEvent(self, event):
        if event.button() == Qt.LeftButton:
            idx = self._hit_test(event.position())
            if idx >= 0:
                self._clicked_block = idx if self._clicked_block != idx else -1
                self.update()
            else:
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
        else:
            idx = self._hit_test(event.position())
            if idx != self._hovered_block:
                self._hovered_block = idx
                self.setCursor(Qt.PointingHandCursor if idx >= 0 else Qt.ArrowCursor)
                self.update()
        super().mouseMoveEvent(event)

    def mouseReleaseEvent(self, event):
        if event.button() == Qt.LeftButton:
            self._dragging = False
            self.setCursor(Qt.ArrowCursor)
        super().mouseReleaseEvent(event)

    def _hit_test(self, pos) -> int:
        history = self.sim.commit_history
        if not history:
            return -1
        cell_w = self._cell_w()
        block_w = 68
        top_y = 38
        block_h = self.height() - top_y - 14
        start_x = 10 + self._scroll_offset
        for i in range(len(history)):
            x = start_x + i * cell_w
            if QRectF(x, top_y, block_w, block_h).contains(pos):
                return i
        return -1

    def _clamp_scroll(self):
        history = self.sim.commit_history
        if not history:
            self._scroll_offset = 0
            return
        cell_w = self._cell_w()
        total_content_w = len(history) * cell_w + 10
        visible_w = self.width()
        min_scroll = min(0, visible_w - total_content_w - 20)
        self._scroll_offset = max(min_scroll, min(0, self._scroll_offset))

    # ─── Paint ────────────────────────────────────────────────────────────

    def paintEvent(self, event):
        p = QPainter(self)
        p.setRenderHint(QPainter.Antialiasing)
        w, h = self.width(), self.height()

        # Background
        bg = QPainterPath()
        bg.addRoundedRect(QRectF(0, 0, w, h), 10, 10)
        p.fillPath(bg, QColor(28, 46, 74, 100))
        p.setPen(QPen(QColor(255, 255, 255, 25), 1))
        p.drawPath(bg)

        # Título
        p.setPen(QColor(C["green"]))
        p.setFont(QFont("Segoe UI", 9, QFont.Bold))
        p.drawText(QRectF(10, 4, 350, 16), Qt.AlignLeft, "\u26d3 Cadeia de Commits (Log Replicado)")

        # Legenda
        p.setPen(QColor(C["text3"]))
        p.setFont(QFont("Segoe UI", 6))
        p.drawText(QRectF(10, 18, w - 20, 12), Qt.AlignLeft,
                   "SN=sequência | G=grupo | N=líder | hash=digest | setas=encadeamento")

        history = self.sim.commit_history
        if not history:
            p.setPen(QColor(C["text3"]))
            p.setFont(QFont("Segoe UI", 8))
            p.drawText(QRectF(10, 38, w - 20, h - 42), Qt.AlignCenter, "Nenhum commit ainda...")
            p.end()
            return

        top_y = 38
        block_h = h - top_y - 14
        block_w = 68
        gap = 8
        arrow_w = 16
        cell_w = block_w + gap + arrow_w
        start_x = 10 + self._scroll_offset

        # Contador
        p.setPen(QColor(C["text2"]))
        p.setFont(QFont("Segoe UI", 7))
        p.drawText(QRectF(w - 100, 4, 90, 16), Qt.AlignRight,
                   f"Total: {len(history)} | C{history[-1]['checkpoint_idx']}")

        # Detecta mudanças de checkpoint para separadores (não muda líder/buckets,
        # só marca onde o log poderia ser truncado)
        prev_ckpt = -1

        for i, entry in enumerate(history):
            x = start_x + i * cell_w
            if x + block_w < -10 or x > w + 10:
                prev_ckpt = entry["checkpoint_idx"]
                continue

            # Separador de checkpoint
            if entry["checkpoint_idx"] != prev_ckpt and prev_ckpt >= 0 and x > 0:
                sep_x = x - gap / 2 - arrow_w / 2
                p.setPen(QPen(QColor(C["gold"]), 1, Qt.DashLine))
                p.drawLine(QPointF(sep_x, top_y - 2), QPointF(sep_x, top_y + block_h + 2))
                p.setPen(QColor(C["gold"]))
                p.setFont(QFont("Segoe UI", 5, QFont.Bold))
                p.drawText(QRectF(sep_x - 15, top_y - 10, 30, 9), Qt.AlignCenter,
                           f"C{entry['checkpoint_idx']}")
            prev_ckpt = entry["checkpoint_idx"]

            # Cor do bloco
            lc = QColor(entry.get("color", "#FFFFFF")) if entry.get("color") else QColor(C["text2"])

            # Animação de entrada (último bloco)
            is_newest = (i == len(history) - 1) and self._new_block_ttl > 0
            is_hovered = (i == self._hovered_block)
            is_selected = (i == self._clicked_block)

            self._draw_block(p, x, top_y, block_w, block_h, entry, lc,
                             is_newest, is_hovered, is_selected)

            # Seta de encadeamento
            if i < len(history) - 1:
                next_entry = history[i + 1]
                self._draw_arrow(p, x + block_w + 2, top_y + block_h / 2,
                                 arrow_w + gap - 4, entry["hash"], w)

        # Popup de detalhes
        if self._clicked_block >= 0 and self._clicked_block < len(history):
            self._draw_popup(p, w, h, history[self._clicked_block], start_x)

        # Glow no último commit (evento visual)
        for ev in (self.sim.visual_events if hasattr(self.sim, 'visual_events') else []):
            if ev["type"] == "commit" and history:
                alpha = min(180, ev["ttl"] * 6)
                glow_x = start_x + (len(history) - 1) * cell_w
                if 0 < glow_x < w:
                    glow_c = QColor(C["green"])
                    glow_c.setAlpha(alpha)
                    glow_rect = QRectF(glow_x - 3, top_y - 3, block_w + 6, block_h + 6)
                    glow_path = QPainterPath()
                    glow_path.addRoundedRect(glow_rect, 8, 8)
                    p.setPen(QPen(glow_c, 2.5))
                    p.setBrush(Qt.NoBrush)
                    p.drawPath(glow_path)
                break

        p.end()

    def _draw_block(self, p, x, y, bw, bh, entry, lc,
                    is_newest, is_hovered, is_selected):
        """Desenha um bloco de commit."""
        block_path = QPainterPath()
        block_path.addRoundedRect(QRectF(x, y, bw, bh), 6, 6)

        # Fundo gradiente
        grad = QLinearGradient(x, y, x, y + bh)
        c1 = QColor(lc)
        c1.setAlpha(70 if (is_hovered or is_selected) else 45)
        c2 = QColor(lc)
        c2.setAlpha(25 if (is_hovered or is_selected) else 15)
        grad.setColorAt(0.0, c1)
        grad.setColorAt(1.0, c2)
        p.fillPath(block_path, QBrush(grad))

        # Borda
        border_c = QColor(lc)
        border_c.setAlpha(220 if is_selected else (180 if is_hovered else 120))
        pen_w = 2.5 if is_selected else (2.0 if is_hovered else 1.2)
        p.setPen(QPen(border_c, pen_w))
        p.setBrush(Qt.NoBrush)
        p.drawPath(block_path)

        # Animação de entrada: escala/brilho
        if is_newest:
            glow_alpha = min(150, self._new_block_ttl * 5)
            gc = QColor(lc)
            gc.setAlpha(glow_alpha)
            glow_path = QPainterPath()
            glow_path.addRoundedRect(QRectF(x - 2, y - 2, bw + 4, bh + 4), 8, 8)
            p.setPen(QPen(gc, 2))
            p.setBrush(Qt.NoBrush)
            p.drawPath(glow_path)

        # Cross-group indicator
        if entry["is_cross"]:
            p.setPen(Qt.NoPen)
            p.setBrush(QColor("#D500F9"))
            p.drawEllipse(QPointF(x + bw - 7, y + 7), 4, 4)
            p.setPen(QColor(255, 255, 255, 200))
            p.setFont(QFont("Segoe UI", 5, QFont.Bold))
            p.drawText(QRectF(x + bw - 11, y + 3, 8, 8), Qt.AlignCenter, "X")

        # Conteúdo do bloco
        cy = y + 3

        # SN (destaque)
        p.setPen(QColor(C["text"]))
        p.setFont(QFont("Consolas", 9, QFont.Bold))
        p.drawText(QRectF(x, cy, bw, 14), Qt.AlignCenter, f"SN{entry['sn']}")
        cy += 14

        # Grupo
        p.setPen(lc)
        p.setFont(QFont("Segoe UI", 7, QFont.Bold))
        p.drawText(QRectF(x, cy, bw, 11), Qt.AlignCenter, f"G{entry['group']}")
        cy += 11

        # Líder
        p.setPen(QColor(C["text2"]))
        p.setFont(QFont("Segoe UI", 6))
        p.drawText(QRectF(x, cy, bw, 10), Qt.AlignCenter, f"N{entry['leader']}")
        cy += 10

        # Hash
        p.setPen(QColor(C["text3"]))
        p.setFont(QFont("Consolas", 6))
        p.drawText(QRectF(x, cy, bw, 9), Qt.AlignCenter, entry["hash"])
        cy += 10

        # Quorum (sempre atingido se commitou)
        if bh > 58:
            p.setPen(QColor(C["green"]))
            p.setFont(QFont("Segoe UI", 6))
            p.drawText(QRectF(x, cy, bw, 9), Qt.AlignCenter, "\u2713 quorum")
            cy += 10

        # Checkpoint
        if bh > 68:
            p.setPen(QColor(C["text3"]))
            p.setFont(QFont("Segoe UI", 5))
            p.drawText(QRectF(x, bh + y - 10, bw, 9), Qt.AlignCenter, f"C{entry['checkpoint_idx']}")

    def _draw_arrow(self, p, ax, ay, arrow_w, prev_hash, max_w):
        """Seta de encadeamento com mini hash."""
        if ax >= max_w or ax + arrow_w < 0:
            return
        # Linha
        p.setPen(QPen(QColor(C["text3"]), 1.2))
        p.drawLine(QPointF(ax, ay), QPointF(ax + arrow_w - 4, ay))

        # Ponta da seta
        p.setBrush(QColor(C["text3"]))
        p.setPen(Qt.NoPen)
        arrow_path = QPainterPath()
        arrow_path.moveTo(ax + arrow_w - 4, ay - 3)
        arrow_path.lineTo(ax + arrow_w, ay)
        arrow_path.lineTo(ax + arrow_w - 4, ay + 3)
        arrow_path.closeSubpath()
        p.fillPath(arrow_path, QColor(C["text3"]))

        # Mini hash na seta (encadeamento)
        p.setPen(QColor(C["text3"]))
        p.setFont(QFont("Consolas", 5))
        p.drawText(QRectF(ax, ay - 10, arrow_w, 8), Qt.AlignCenter, prev_hash[:4])

    def _draw_popup(self, p, panel_w, panel_h, entry, start_x):
        """Popup com detalhes expandidos do commit selecionado."""
        cell_w = self._cell_w()
        block_x = start_x + self._clicked_block * cell_w

        lines = [
            f"Commit SN={entry['sn']}",
            f"Grupo: G{entry['group']}",
            f"Lider: Node {entry['leader']}",
            f"Checkpoint: {entry['checkpoint_idx']}",
            f"Digest: {entry['hash']}",
            f"Cross-group: {'Sim (GSN=' + str(entry['gsn']) + ')' if entry['is_cross'] else 'Nao'}",
            f"Quorum: atingido \u2713",
        ]

        p.setFont(QFont("Segoe UI", 7))
        fm = p.fontMetrics()
        line_h = fm.height() + 2
        popup_w = max(fm.horizontalAdvance(l) for l in lines) + 20
        popup_h = line_h * len(lines) + 12

        # Posição: acima do bloco
        px = max(4, min(block_x, panel_w - popup_w - 4))
        py = 38 - popup_h - 6
        if py < 2:
            py = panel_h - popup_h - 4

        rect = QRectF(px, py, popup_w, popup_h)
        bg_path = QPainterPath()
        bg_path.addRoundedRect(rect, 8, 8)
        p.fillPath(bg_path, QColor(10, 18, 32, 240))

        lc = QColor(entry.get("color", C["text2"])) if entry.get("color") else QColor(C["text2"])
        p.setPen(QPen(lc, 1.5))
        p.setBrush(Qt.NoBrush)
        p.drawPath(bg_path)

        ty = py + 6
        for i, line in enumerate(lines):
            if i == 0:
                p.setPen(lc)
                p.setFont(QFont("Segoe UI", 8, QFont.Bold))
            else:
                p.setPen(QColor(C["text"]))
                p.setFont(QFont("Segoe UI", 7))
            p.drawText(QRectF(px + 8, ty, popup_w - 16, line_h),
                       Qt.AlignLeft | Qt.AlignVCenter, line)
            ty += line_h
