"""ExecutionPanel — métricas + barra de progresso com nós participantes."""

from PySide6.QtWidgets import QWidget, QSizePolicy
from PySide6.QtCore import Qt, QRectF, QPointF, QTimer
from PySide6.QtGui import (
    QPainter, QPainterPath, QColor, QRadialGradient, QPen, QBrush, QFont
)

from mirbftview.qt.theme import C
from mirbftview.qt.simulation import Simulation, Phase


_PROGRESS_STEPS_SETUP = [
    (Phase.PREPARE,       "Prepare",   C["phase_prepare"]),
    (Phase.PROMISE,       "Promise",   C["phase_promise"]),
    (Phase.CLIENT_SEND,   "Request",   C["orange"]),
    (Phase.BUCKET_ASSIGN, "Bucket",    C["gold"]),
    (Phase.ACCEPT,        "Accept",    C["phase_accept"]),
    (Phase.ACCEPTED,      "Accepted",  C["phase_accepted"]),
    (Phase.COMMIT,        "Commit",    C["phase_commit"]),
    (Phase.COMMIT_NOTIFY, "Notify",    C["green"]),
]

_PROGRESS_STEPS_STEADY = [
    (Phase.CLIENT_SEND,   "Request",   C["orange"]),
    (Phase.BUCKET_ASSIGN, "Bucket",    C["gold"]),
    (Phase.ACCEPT,        "Accept",    C["phase_accept"]),
    (Phase.ACCEPTED,      "Accepted",  C["phase_accepted"]),
    (Phase.COMMIT,        "Commit",    C["phase_commit"]),
    (Phase.COMMIT_NOTIFY, "Notify",    C["green"]),
]

_PHASE_ROLE = {
    Phase.PREPARE:       "send",
    Phase.PROMISE:       "respond",
    Phase.CLIENT_SEND:   None,
    Phase.BUCKET_ASSIGN: None,
    Phase.ACCEPT:        "send",
    Phase.ACCEPTED:      "respond",
    Phase.COMMIT:        "send",
    Phase.COMMIT_NOTIFY: None,
}


class ExecutionPanel(QWidget):
    """Painel de execução — métricas + barra de progresso com participantes."""

    def __init__(self, sim: Simulation, parent=None):
        super().__init__(parent)
        self.sim = sim
        self.setAttribute(Qt.WA_TranslucentBackground)
        self.setSizePolicy(QSizePolicy.Expanding, QSizePolicy.Expanding)
        self.setMinimumHeight(120)
        self._timer = QTimer(self)
        self._timer.setInterval(80)
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
            ("Falta Ckpt", str(remaining), C["orange"]),
        ]
        badge_w = (w - 20 - 6 * (len(metrics) - 1)) / len(metrics)
        for i, (label, val, color) in enumerate(metrics):
            bx = 10 + i * (badge_w + 6)
            self._draw_metric_badge(p, bx, badge_y, badge_w, badge_h, label, val, color)

        # Barra de progresso
        progress_y = badge_y + badge_h + 14
        self._draw_progress_bars(p, w, h, progress_y)

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

    def _draw_progress_bars(self, p, panel_w, panel_h, start_y):
        active = self.sim.active_requests
        if not active:
            p.setPen(QColor(C["text3"]))
            p.setFont(QFont("Segoe UI", 8))
            p.drawText(QRectF(10, start_y, panel_w - 20, 20), Qt.AlignCenter, "Aguardando requests...")
            return

        is_steady = getattr(self.sim, 'prepared', False)
        steps = _PROGRESS_STEPS_STEADY if is_steady else _PROGRESS_STEPS_SETUP
        n = len(steps)

        margin_x = 10
        right_margin = 50
        available_w = panel_w - margin_x - right_margin
        step_w = available_w / n

        # Altura responsiva por barra
        available_h = panel_h - start_y - 18  # espaço para labels embaixo
        total_bars = min(len(active), 4)
        spacing = 4
        bar_h_each = max(28, min(44, (available_h - (total_bars - 1) * spacing - 14) / total_bars))

        for bar_idx, req in enumerate(active[:total_bars]):
            bar_y = start_y + bar_idx * (bar_h_each + spacing)
            if bar_y + bar_h_each > panel_h - 14:
                break
            req_color = req.color if hasattr(req, 'color') and req.color else self._get_color(bar_idx)

            current_idx = -1
            for i, (step_phase, _, _) in enumerate(steps):
                if step_phase == req.phase:
                    current_idx = i
                    break
            if current_idx == -1 and req.phase == Phase.DONE:
                current_idx = n

            # Track
            track_y = bar_y + bar_h_each / 2
            p.setPen(QPen(QColor(255, 255, 255, 25), 2))
            p.drawLine(QPointF(margin_x, track_y), QPointF(margin_x + available_w, track_y))

            # Progresso preenchido
            if current_idx > 0:
                fill_w = step_w * current_idx
                p.setPen(QPen(QColor(req_color), 3))
                p.drawLine(QPointF(margin_x, track_y), QPointF(margin_x + fill_w, track_y))

            # Bolinhas + participantes
            for i, (step_phase, label, _) in enumerate(steps):
                cx = margin_x + step_w * i + step_w / 2
                cy = track_y
                is_done = i < current_idx
                is_current = i == current_idx

                radius = 5 if is_current else 3
                if is_current:
                    glow = QRadialGradient(QPointF(cx, cy), 10)
                    gc = QColor(req_color)
                    gc.setAlpha(100)
                    glow.setColorAt(0.0, gc)
                    glow.setColorAt(1.0, QColor(0, 0, 0, 0))
                    p.setPen(Qt.NoPen)
                    p.setBrush(QBrush(glow))
                    p.drawEllipse(QPointF(cx, cy), 10, 10)

                p.setPen(Qt.NoPen)
                if is_done or is_current:
                    p.setBrush(QColor(req_color))
                else:
                    p.setBrush(QColor(C["text3"]))
                p.drawEllipse(QPointF(cx, cy), radius, radius)

                if is_current:
                    self._draw_participants(p, req, step_phase, cx, track_y, step_w, bar_h_each)

            # Label fase (direita)
            if 0 <= current_idx < n:
                phase_label = steps[current_idx][1]
            elif current_idx >= n:
                phase_label = "Pronto"
            else:
                phase_label = "?"
            p.setPen(QColor(req_color))
            p.setFont(QFont("Segoe UI", 7, QFont.Bold))
            p.drawText(
                QRectF(margin_x + available_w + 4, bar_y, right_margin - 8, bar_h_each),
                Qt.AlignLeft | Qt.AlignVCenter, phase_label
            )

        # Labels das fases (embaixo)
        label_y = start_y + total_bars * (bar_h_each + spacing)
        if label_y < panel_h - 4:
            p.setPen(QColor(C["text3"]))
            p.setFont(QFont("Segoe UI", 6))
            for i, (_, label, _) in enumerate(steps):
                cx = margin_x + step_w * i + step_w / 2
                p.drawText(QRectF(cx - step_w / 2, label_y, step_w, 12), Qt.AlignCenter, label)

    def _draw_participants(self, p, req, step_phase, cx, track_y, step_w, bar_h_each):
        role = _PHASE_ROLE.get(step_phase)
        if not role:
            return

        group = None
        for g in self.sim.groups:
            if g.id == req.group_id:
                group = g
                break
        if not group:
            return

        members = group.members
        leader = req.leader

        if role == "send":
            all_nodes = [leader] + [nid for nid in members if nid != leader]
        else:
            all_nodes = [nid for nid in members if nid != leader]

        quorum_needed = req.quorum
        if step_phase == Phase.PROMISE:
            received = req.promises_received
        elif step_phase == Phase.ACCEPTED:
            received = req.accepted_received
        else:
            received = len(all_nodes)

        n_nodes = len(all_nodes)
        if n_nodes == 0:
            return

        max_node_w = min(step_w * 0.85, n_nodes * 20)
        node_spacing = max_node_w / max(n_nodes, 1)
        start_x = cx - max_node_w / 2
        dot_r = max(3, min(4, step_w / (n_nodes * 5)))
        font_size = max(5, min(6, int(step_w / 14)))

        ny = track_y - bar_h_each * 0.28

        for j, nid in enumerate(all_nodes):
            nx = start_x + j * node_spacing + node_spacing / 2
            is_leader_node = (nid == leader)

            if role == "send":
                nc = QColor(C["gold"]) if is_leader_node else QColor(C["green"])
            else:
                responded = (j < received)
                nc = QColor(C["green"]) if responded else QColor(C["text3"])

            p.setPen(Qt.NoPen)
            p.setBrush(nc)
            p.drawEllipse(QPointF(nx, ny), dot_r, dot_r)

            p.setPen(nc)
            p.setFont(QFont("Segoe UI", font_size, QFont.Bold))
            lbl = f"L{nid}" if is_leader_node and role == "send" else f"N{nid}"
            p.drawText(QRectF(nx - 12, ny - dot_r - 9, 24, 9), Qt.AlignCenter, lbl)

        # Info abaixo do track
        info_y = track_y + bar_h_each * 0.15
        p.setPen(QColor(C["text2"]))
        p.setFont(QFont("Segoe UI", font_size))
        if role == "respond":
            txt = f"{min(received, len(all_nodes))}/{quorum_needed} quorum"
        else:
            txt = f"L{leader} \u2192 {len(all_nodes) - 1} nos"
        p.drawText(QRectF(cx - step_w * 0.4, info_y, step_w * 0.8, 10), Qt.AlignCenter, txt)

    @staticmethod
    def _get_color(idx: int) -> str:
        from mirbftview.qt.canvas._constants import MSG_COLOR_POOL
        return MSG_COLOR_POOL[idx % len(MSG_COLOR_POOL)]
