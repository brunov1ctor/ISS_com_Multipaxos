"""ExecutionPanel — métricas + barra de progresso com nós participantes."""

from PySide6.QtWidgets import QWidget, QSizePolicy
from PySide6.QtCore import Qt, QRectF, QPointF, QTimer, QSize
from PySide6.QtGui import (
    QPainter, QPainterPath, QColor, QRadialGradient, QPen, QBrush, QFont
)

from mirbftview.qt.theme import C
from mirbftview.qt.simulation import Simulation, Phase
from mirbftview.qt.canvas._constants import GROUP_COLORS


# Linha do tempo única. Um PEDIDO de cliente ocupa as 3 primeiras colunas (chega ao proxy,
# ganha GSN se for cross-group e cai nos buckets); depois disso ele só espera no bucket. Uma
# INSTÂNCIA de Paxos de um grupo ocupa as demais: PREPARE, PROMISE, corte do batch, ACCEPT...
_STEPS = [
    (Phase.CLIENT_SEND,   "Pedido",   C["orange"]),
    (Phase.GSN_ASSIGN,    "GSN",      C["phase_prepare"]),
    (Phase.BUCKET_ASSIGN, "Bucket",   C["gold"]),
    (Phase.PREPARE,       "Prepare",  C["phase_prepare"]),
    (Phase.PROMISE,       "Promise",  C["phase_promise"]),
    (Phase.BATCH_CUT,     "Corte",    C["gold"]),
    (Phase.ACCEPT,        "Accept",   C["phase_accept"]),
    (Phase.ACCEPTED,      "Accepted", C["phase_accepted"]),
    (Phase.COMMIT,        "Commit",   C["phase_commit"]),
    (Phase.ADELIVER,      "Deliver",  C["accent"]),
    (Phase.COMMIT_NOTIFY, "Notify",   C["green"]),
]
_REQUEST_COLS = (0, 3)      # [inicio, fim) das colunas de um pedido
_INSTANCE_COLS = (3, 11)    # [inicio, fim) das colunas de uma instância

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
        self._timer = QTimer(self)
        self._timer.setInterval(80)
        self._timer.timeout.connect(self.update)
        self._timer.start()

    def minimumSizeHint(self):
        # Métricas (~70 px) + até 4 barras de 28 px + rótulos das fases; largura
        # para as bolinhas das fases (até 8) + coluna de fase/grupo à direita.
        return QSize(560, 390)

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
        n_groups = len([g for g in s.groups if g.id != 0]) if s.groups else 0
        # Checkpoint agora é por grupo (ver phase_checkpoint): cada grupo tem seu próprio
        # intervalo (checkpoint_interval / nGrupos) e sua própria contagem. "Falta Ckpt" mostra
        # o grupo mais perto de completar o seu, não um progresso global único.
        group_interval = max(s._checkpoint_interval // max(n_groups, 1), 1)
        counts = s.group_commit_count.values() if s.group_commit_count else []
        remaining = min((group_interval - (c % group_interval) for c in counts), default=group_interval)
        metrics = [
            ("Checkpoints", str(s._checkpoints_done), C["accent"]),
            ("Commits", str(s._committed), C["green"]),
            ("Grupos", str(n_groups), C["gold"]),
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
        # Pedidos a caminho primeiro, depois as instâncias (uma por grupo).
        items = list(self.sim.active_requests)
        items.sort(key=lambda r: (0 if r.kind == "request" else 1, r.group_id, r.sn))
        if not items:
            p.setPen(QColor(C["text3"]))
            p.setFont(QFont("Segoe UI", 8))
            p.drawText(QRectF(10, start_y, panel_w - 20, 20), Qt.AlignCenter, "Aguardando inicio...")
            return

        steps = _STEPS
        n = len(steps)
        margin_x = 10
        right_margin = 84
        available_w = panel_w - margin_x - right_margin
        step_w = available_w / n

        available_h = panel_h - start_y - 18  # espaço para os rótulos embaixo
        spacing = 4
        max_bars = 10
        total_bars = min(len(items), max_bars)
        bar_h_each = max(26, min(44, (available_h - (total_bars - 1) * spacing - 14) / total_bars))

        for bar_idx, req in enumerate(items[:total_bars]):
            bar_y = start_y + bar_idx * (bar_h_each + spacing)
            if bar_y + bar_h_each > panel_h - 14:
                break
            req_color = req.color if req.color else self._get_color(bar_idx)
            lo, hi = _REQUEST_COLS if req.kind == "request" else _INSTANCE_COLS

            current_idx = -1
            for i in range(lo, hi):
                if steps[i][0] == req.phase:
                    current_idx = i
                    break
            if current_idx == -1 and req.phase == Phase.DONE:
                current_idx = hi

            # Trilho só nas colunas deste tipo de item
            track_y = bar_y + bar_h_each / 2
            x_lo = margin_x + step_w * lo + step_w / 2
            x_hi = margin_x + step_w * (hi - 1) + step_w / 2
            p.setPen(QPen(QColor(255, 255, 255, 25), 2))
            p.drawLine(QPointF(x_lo, track_y), QPointF(x_hi, track_y))

            # Progresso preenchido
            if current_idx > lo:
                x_cur = margin_x + step_w * min(current_idx, hi - 1) + step_w / 2
                p.setPen(QPen(QColor(req_color), 3))
                p.drawLine(QPointF(x_lo, track_y), QPointF(x_cur, track_y))

            # Bolinhas + participantes
            for i in range(lo, hi):
                step_phase, label, _ = steps[i]
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
                p.setBrush(QColor(req_color) if (is_done or is_current) else QColor(C["text3"]))
                p.drawEllipse(QPointF(cx, cy), radius, radius)

                if is_current and req.kind == "instance":
                    self._draw_participants(p, req, step_phase, cx, track_y, step_w, bar_h_each)

            # Rótulo à direita: fase atual, e quem é (pedido ou grupo·SN)
            if lo <= current_idx < hi:
                phase_label = steps[current_idx][1]
                if req.kind == "instance" and current_idx == 5 and getattr(req, "_waiting", False):
                    phase_label = "Espera"
            elif current_idx >= hi:
                phase_label = "Pronto"
            else:
                phase_label = "?"
            label_x = margin_x + available_w + 4
            half_h = bar_h_each / 2
            p.setPen(QColor(req_color))
            p.setFont(QFont("Segoe UI", 7, QFont.Bold))
            p.drawText(QRectF(label_x, bar_y, right_margin - 8, half_h),
                       Qt.AlignLeft | Qt.AlignBottom, phase_label)
            p.setPen(QColor(GROUP_COLORS[req.group_id % len(GROUP_COLORS)]))
            p.setFont(QFont("Segoe UI", 7, QFont.Bold))
            p.drawText(QRectF(label_x, bar_y + half_h, right_margin - 8, half_h),
                       Qt.AlignLeft | Qt.AlignTop, self._group_text(req))

        # Rótulos das fases (embaixo)
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
    def _group_text(req) -> str:
        # Instância: "G3·SN 8 ×2" (grupo, posição do log e nº de pedidos no batch).
        # Pedido: "Cliente 1 #0" -> "c1#0", com os grupos que toca se for cross-group.
        if req.kind == "instance":
            n = len(req.members)
            return f"G{req.group_id}\u00b7SN {req.sn}" + (f" \u00d7{n}" if n else "")
        txt = f"c{req.client_id}#{req.client_sn}"
        if req.is_cross_group and len(req.touched_groups) > 1:
            txt += " TX"
        return txt

    @staticmethod
    def _get_color(idx: int) -> str:
        from mirbftview.qt.canvas._constants import MSG_COLOR_POOL
        return MSG_COLOR_POOL[idx % len(MSG_COLOR_POOL)]
