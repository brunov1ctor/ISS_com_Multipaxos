"""SnTablePanel — janelinha "SNs por grupo": quando cada instância (SN) nasce e em que fase está.

Uma linha por grupo de dados, com os últimos SNs (o mais novo à direita). Um SN nasce quando a
instância anterior do grupo foi anunciada ao log e o laço do grupo abre a seguinte (antes do
PREPARE do novo líder, e bem antes do corte do batch). O SN novo pisca por alguns instantes.
"""

from PySide6.QtWidgets import QWidget, QSizePolicy
from PySide6.QtCore import Qt, QRectF, QPointF, QTimer, QSize
from PySide6.QtGui import QPainter, QPainterPath, QColor, QPen, QFont

from mirbftview.qt.theme import C
from mirbftview.qt.simulation import Simulation, Phase
from mirbftview.qt.canvas._constants import GROUP_COLORS

_NEW_TICKS = 70          # quanto tempo (ticks da simulação) o SN recém-criado pisca
_CHIP_W, _CHIP_H, _GAP = 50, 26, 4
_LABEL_W = 34
_PHASE_W = 96
_TPS = 62               # ticks por segundo simulado (igual a engine.phases.TICKS_PER_SEC)

_PHASE_NAMES = {
    Phase.PREPARE: "Prepare", Phase.PROMISE: "Promise", Phase.BATCH_CUT: "Corte",
    Phase.ACCEPT: "Accept", Phase.ACCEPTED: "Accepted", Phase.COMMIT: "Commit",
    Phase.ADELIVER: "Deliver", Phase.COMMIT_NOTIFY: "Notify", Phase.DONE: "Concluída",
    Phase.RETRANSMIT: "Retransmit", Phase.VIEW_CHANGE: "View change", Phase.CHECKPOINT: "Checkpoint",
}


class SnTablePanel(QWidget):
    """Tabela Grupo | SNs, com o momento em que cada SN é criado."""

    def __init__(self, sim: Simulation, parent=None):
        super().__init__(parent)
        self.sim = sim
        self.setAttribute(Qt.WA_TranslucentBackground)
        self.setSizePolicy(QSizePolicy.Preferred, QSizePolicy.Expanding)
        self.setMinimumWidth(250)
        self._timer = QTimer(self)
        self._timer.setInterval(80)
        self._timer.timeout.connect(self.update)
        self._timer.start()

    def minimumSizeHint(self):
        rows = max(len([g for g in self.sim.groups if g.id != 0]), 1)
        return QSize(_LABEL_W + _PHASE_W + 2 * (_CHIP_W + _GAP) + 24, 36 + rows * (_CHIP_H + 6) + 8)

    @staticmethod
    def _phase_text(inst) -> str:
        if inst.phase == Phase.BATCH_CUT and getattr(inst, "_waiting", False):
            return "Esperando pedidos"
        return _PHASE_NAMES.get(inst.phase, inst.phase.name.title() if inst.phase else "?")

    def paintEvent(self, event):
        p = QPainter(self)
        p.setRenderHint(QPainter.Antialiasing)
        w, h = self.width(), self.height()
        sim = self.sim
        now = sim.tick_count

        p.setPen(QColor(C["accent"]))
        p.setFont(QFont("Segoe UI", 10, QFont.Bold))
        p.drawText(QRectF(10, 4, w - 20, 18), Qt.AlignLeft, "SNs por grupo")
        p.setPen(QColor(C["text3"]))
        p.setFont(QFont("Segoe UI", 7))
        p.drawText(QRectF(10, 21, w - 20, 12), Qt.AlignLeft,
                   "SN novo = instância aberta (antes do PREPARE e do corte)")

        groups = [g for g in sim.groups if g.id != 0]
        if not groups or not sim.sn_history:
            p.setPen(QColor(C["text3"]))
            p.drawText(QRectF(10, 44, w - 20, 16), Qt.AlignLeft, "Aguardando início...")
            p.end()
            return

        top = 38
        row_h = max(_CHIP_H + 4, min(40, (h - top - 6) / max(len(groups), 1)))
        chips_area = w - 10 - _LABEL_W - _PHASE_W - 8
        max_chips = max(int(chips_area // (_CHIP_W + _GAP)), 1)

        for r, g in enumerate(groups):
            y = top + r * row_h
            gcolor = QColor(GROUP_COLORS[g.id % len(GROUP_COLORS)])
            hist = sim.sn_history.get(g.id, [])

            p.setPen(gcolor)
            p.setFont(QFont("Segoe UI", 9, QFont.Bold))
            p.drawText(QRectF(10, y, _LABEL_W, row_h), Qt.AlignVCenter | Qt.AlignLeft, f"G{g.id}")

            shown = hist[-max_chips:]
            if len(hist) > len(shown):
                p.setPen(QColor(C["text3"]))
                p.setFont(QFont("Segoe UI", 8))
                p.drawText(QRectF(10 + _LABEL_W - 6, y, 10, row_h), Qt.AlignVCenter | Qt.AlignLeft, "…")

            x0 = 10 + _LABEL_W + 8
            cy = y + (row_h - _CHIP_H) / 2
            for k, entry in enumerate(shown):
                x = x0 + k * (_CHIP_W + _GAP)
                is_last = entry is hist[-1]
                age = now - entry["born"]
                fresh = age < _NEW_TICKS
                rect = QRectF(x, cy, _CHIP_W, _CHIP_H)

                path = QPainterPath()
                path.addRoundedRect(rect, 6, 6)
                fill = QColor(gcolor)
                fill.setAlpha(80 if is_last else 28)
                p.fillPath(path, fill)
                border = QColor(gcolor)
                border.setAlpha(235 if is_last else 110)
                p.setPen(QPen(border, 2.2 if is_last else 1.2))
                p.setBrush(Qt.NoBrush)
                p.drawPath(path)

                if fresh:
                    # Anel que expande e some: "acabou de ser criado"
                    t = age / _NEW_TICKS
                    ring = QColor(gcolor)
                    ring.setAlpha(int(220 * (1 - t)))
                    p.setPen(QPen(ring, 2))
                    grow = 2 + 8 * t
                    rr = QPainterPath()
                    rr.addRoundedRect(rect.adjusted(-grow, -grow, grow, grow), 8, 8)
                    p.drawPath(rr)

                p.setPen(QColor(C["text"] if is_last else C["text2"]))
                p.setFont(QFont("Segoe UI", 8, QFont.Bold))
                p.drawText(QRectF(x, cy + 1, _CHIP_W, 13), Qt.AlignCenter, f"SN {entry['sn']}")
                p.setFont(QFont("Segoe UI", 6))
                p.setPen(QColor(C["text3"]))
                p.drawText(QRectF(x, cy + 14, _CHIP_W, 11), Qt.AlignCenter, f"líder N{entry['leader']}")

            # Fase da instância mais nova do grupo
            if hist:
                inst = hist[-1]["inst"]
                p.setPen(gcolor)
                p.setFont(QFont("Segoe UI", 7, QFont.Bold))
                px = w - _PHASE_W - 6
                p.drawText(QRectF(px, y, _PHASE_W, row_h * 0.55), Qt.AlignLeft | Qt.AlignBottom,
                           self._phase_text(inst))
                n = len(inst.members)
                waiting = inst.phase == Phase.BATCH_CUT and getattr(inst, "_waiting", False)
                p.setPen(QColor(C["text3"]))
                p.setFont(QFont("Segoe UI", 6))
                if waiting:
                    limit = getattr(inst, "_wait_limit", 0) or 1
                    elapsed = min(getattr(inst, "_wait_elapsed", 0.0), limit)
                    left = (limit - elapsed) / _TPS
                    p.setPen(QColor(C["gold"]))
                    p.drawText(QRectF(px, y + row_h * 0.55, _PHASE_W, row_h * 0.3),
                               Qt.AlignLeft | Qt.AlignTop,
                               f"\u23f1 {left:0.0f}s / {limit / _TPS:0.0f}s \u00b7 {getattr(inst, '_wait_queued', 0)}/{getattr(inst, '_wait_size', 0)}")
                    bar = QRectF(px, y + row_h * 0.55 + 11, _PHASE_W - 8, 3)
                    p.setPen(Qt.NoPen)
                    p.setBrush(QColor(255, 255, 255, 30))
                    p.drawRoundedRect(bar, 1.5, 1.5)
                    p.setBrush(QColor(gcolor))
                    p.drawRoundedRect(QRectF(bar.x(), bar.y(), bar.width() * (elapsed / limit), 3), 1.5, 1.5)
                else:
                    p.drawText(QRectF(px, y + row_h * 0.55, _PHASE_W, row_h * 0.45),
                               Qt.AlignLeft | Qt.AlignTop,
                               f"{n} pedido(s) no batch" if n else "batch ainda vazio")
        p.end()
