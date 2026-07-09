"""ExecutionPanel — métricas + pipeline gráfico."""

from PySide6.QtWidgets import QWidget, QSizePolicy
from PySide6.QtCore import Qt, QRectF, QPointF, QTimer
from PySide6.QtGui import (
    QPainter, QPainterPath, QColor, QLinearGradient, QPen, QBrush, QFont
)

from mirbftview.qt.theme import C
from mirbftview.qt.simulation import Simulation, Phase, RequestInfo


class ExecutionPanel(QWidget):
    """Painel de execução — métricas do sistema + pipeline visual."""

    _PHASE_COLORS = {
        Phase.CLIENT_SEND: "#F97316",
        Phase.BUCKET_ASSIGN: "#FACC15",
        Phase.BATCH_CUT: "#FACC15",
        Phase.GSN_ASSIGN: "#A855F7",
        Phase.PREPARE: "#58D8FF",
        Phase.PROMISE: "#58D8FF",
        Phase.ACCEPT: "#6C63FF",
        Phase.ACCEPTED: "#8B7DFF",
        Phase.COMMIT: "#34D399",
        Phase.COMMIT_NOTIFY: "#34D399",
        Phase.ADELIVER: "#34D399",
        Phase.CHECKPOINT: "#EF4444",
        Phase.EPOCH_TRANSITION: "#EF4444",
        Phase.VIEW_CHANGE: "#EF4444",
        Phase.RETRANSMIT: "#F97316",
    }

    _PHASE_ORDER = [
        Phase.PREPARE, Phase.PROMISE, Phase.CLIENT_SEND, Phase.BUCKET_ASSIGN,
        Phase.ACCEPT, Phase.ACCEPTED, Phase.COMMIT, Phase.COMMIT_NOTIFY, Phase.ADELIVER,
    ]

    def __init__(self, sim: Simulation, parent=None):
        super().__init__(parent)
        self.sim = sim
        self.setAttribute(Qt.WA_TranslucentBackground)
        self.setSizePolicy(QSizePolicy.Expanding, QSizePolicy.Expanding)
        self.setMinimumHeight(100)
        self._timer = QTimer(self)
        self._timer.setInterval(100)
        self._timer.timeout.connect(self.update)
        self._timer.start()

    def paintEvent(self, event):
        p = QPainter(self)
        p.setRenderHint(QPainter.Antialiasing)
        w, h = self.width(), self.height()
        s = self.sim

        p.setPen(QColor(C["green"]))
        p.setFont(QFont("Segoe UI", 10, QFont.Bold))
        p.drawText(QRectF(10, 4, w, 18), Qt.AlignLeft, "Execucao")

        # Métricas
        badge_y = 24
        badge_h = 32
        n_leaders = len(s.epoch_mgr.current_leaders) if s.epoch_mgr else 0
        remaining = s._checkpoint_interval - (s._committed % s._checkpoint_interval) if s._committed > 0 else s._checkpoint_interval
        metrics = [
            ("Epoch", str(s._epoch), C["accent"]),
            ("Commits", str(s._committed), C["green"]),
            ("Lideres", str(n_leaders), C["gold"]),
            ("Prox.Ckpt", str(remaining), C["orange"]),
        ]
        badge_w = (w - 20 - 6 * (len(metrics) - 1)) / len(metrics)
        for i, (label, val, color) in enumerate(metrics):
            bx = 10 + i * (badge_w + 6)
            self._draw_metric_badge(p, bx, badge_y, badge_w, badge_h, label, val, color)

        # Pipeline
        pipe_y = badge_y + badge_h + 10
        pipe_h = max(16, min(22, (h - pipe_y - 10) / max(len(s.active_requests), 1) - 3))
        active = s.active_requests

        if active:
            p.setPen(QColor(C["text3"]))
            p.setFont(QFont("Segoe UI", 7))
            p.drawText(QRectF(10, pipe_y - 12, w - 20, 12), Qt.AlignLeft, "Pipeline de Requests")
            for idx, req in enumerate(active[:6]):
                ry = pipe_y + idx * (pipe_h + 3)
                if ry + pipe_h > h - 4:
                    break
                self._draw_pipeline_bar(p, 10, ry, w - 20, pipe_h, req)
        else:
            p.setPen(QColor(C["text3"]))
            p.setFont(QFont("Segoe UI", 8))
            p.drawText(QRectF(10, pipe_y, w - 20, 30), Qt.AlignCenter, "Aguardando requests...")

        p.end()

    def _draw_metric_badge(self, p, x, y, w, h, label, value, color):
        path = QPainterPath()
        path.addRoundedRect(QRectF(x, y, w, h), 6, 6)
        p.fillPath(path, QColor(28, 46, 74, 100))
        p.setPen(QPen(QColor(color).darker(140), 1))
        p.setBrush(Qt.NoBrush)
        p.drawPath(path)

        p.setPen(QColor(color))
        p.setFont(QFont("Segoe UI", 11, QFont.Bold))
        p.drawText(QRectF(x, y + 2, w, h * 0.6), Qt.AlignCenter, value)

        p.setPen(QColor(C["text3"]))
        p.setFont(QFont("Segoe UI", 6))
        p.drawText(QRectF(x, y + h * 0.55, w, h * 0.4), Qt.AlignCenter, label)

    def _draw_pipeline_bar(self, p, x, y, w, h, req: RequestInfo):
        num_phases = len(self._PHASE_ORDER)
        phase = req.phase or Phase.PREPARE
        cur_idx = 0
        for i, ph in enumerate(self._PHASE_ORDER):
            if ph == phase:
                cur_idx = i
                break

        # Background
        bg = QPainterPath()
        bg.addRoundedRect(QRectF(x, y, w, h), 4, 4)
        p.fillPath(bg, QColor(255, 255, 255, 12))

        # Fill
        fill_w = w * (cur_idx + 1) / num_phases
        fill = QPainterPath()
        fill.addRoundedRect(QRectF(x, y, fill_w, h), 4, 4)
        color = QColor(req.color) if req.color else QColor(self._PHASE_COLORS.get(phase, C["text3"]))
        grad = QLinearGradient(x, y, x + fill_w, y)
        c1 = QColor(color)
        c1.setAlpha(140)
        c2 = QColor(color)
        c2.setAlpha(80)
        grad.setColorAt(0.0, c1)
        grad.setColorAt(1.0, c2)
        p.fillPath(fill, QBrush(grad))

        # Border
        p.setPen(QPen(QColor(color.red(), color.green(), color.blue(), 100), 1))
        p.setBrush(Qt.NoBrush)
        p.drawPath(bg)

        # Labels
        p.setPen(QColor(C["text"]))
        p.setFont(QFont("Consolas", 7, QFont.Bold))
        p.drawText(QRectF(x + 4, y, 40, h), Qt.AlignVCenter | Qt.AlignLeft, f"G{req.group_id}")

        p.setPen(QColor(255, 255, 255, 200))
        p.setFont(QFont("Segoe UI", 7))
        phase_name = phase.name.replace("_", " ").title()
        p.drawText(QRectF(x + 44, y, w - 88, h), Qt.AlignVCenter | Qt.AlignCenter, phase_name)

        p.setPen(QColor(C["text2"]))
        p.setFont(QFont("Segoe UI", 6))
        p.drawText(QRectF(x + w - 44, y, 40, h), Qt.AlignVCenter | Qt.AlignRight, f"N{req.leader}")
