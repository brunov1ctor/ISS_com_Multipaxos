"""InfoPanel — painel 'O que está acontecendo'."""

from PySide6.QtWidgets import (
    QWidget, QVBoxLayout, QScrollArea, QSizePolicy, QLabel, QPushButton
)
from PySide6.QtCore import Qt, QTimer

from mirbftview.qt.theme import C
from mirbftview.qt.simulation import Simulation, Phase
from mirbftview.qt.panels._helpers import _title, _body


# Mapa de fases para descrições curtas em linguagem simples
_PHASE_DESCRIPTIONS = {
    Phase.PREPARE: "Lideres pedem permissao ao grupo",
    Phase.PROMISE: "Membros concordam com o lider",
    Phase.CLIENT_SEND: "Clientes enviam pedidos ao sistema",
    Phase.BUCKET_ASSIGN: "Pedidos entram na fila (bucket)",
    Phase.ACCEPT: "Lideres propoem o pacote para votacao",
    Phase.ACCEPTED: "Membros aceitam a proposta",
    Phase.COMMIT: "Decisao final — pedido confirmado!",
    Phase.COMMIT_NOTIFY: "Proxy responde ao cliente",
    Phase.ADELIVER: "Entrega atomica (ordem global)",
    Phase.GSN_ASSIGN: "Numeracao global para pedido multi-grupo",
    Phase.CHECKPOINT: "Salvando progresso (checkpoint)",
    Phase.EPOCH_TRANSITION: "Troca de turno (novo epoch)",
    Phase.VIEW_CHANGE: "Lider caiu — elegendo substituto",
    Phase.RETRANSMIT: "Timeout — retransmitindo mensagem",
}

_PHASE_ICONS = {
    Phase.PREPARE: "\u270b",
    Phase.PROMISE: "\u2705",
    Phase.CLIENT_SEND: "\U0001f4e8",
    Phase.BUCKET_ASSIGN: "\U0001faa3",
    Phase.ACCEPT: "\U0001f4e6",
    Phase.ACCEPTED: "\U0001f44d",
    Phase.COMMIT: "\U0001f389",
    Phase.COMMIT_NOTIFY: "\U0001f4ec",
    Phase.ADELIVER: "\U0001f513",
    Phase.GSN_ASSIGN: "\U0001f522",
    Phase.CHECKPOINT: "\U0001f3c1",
    Phase.EPOCH_TRANSITION: "\U0001f504",
    Phase.VIEW_CHANGE: "\u26a0\ufe0f",
    Phase.RETRANSMIT: "\u23f1\ufe0f",
}


class InfoPanel(QWidget):
    """Painel principal — mostra fase atual + todas as requests ativas com dropdown."""

    def __init__(self, sim: Simulation, parent=None):
        super().__init__(parent)
        self.sim = sim
        self.setAttribute(Qt.WA_TranslucentBackground)
        self.setSizePolicy(QSizePolicy.Expanding, QSizePolicy.Expanding)
        layout = QVBoxLayout(self)
        layout.setContentsMargins(10, 10, 10, 10)
        layout.setSpacing(6)

        layout.addWidget(_title("O que esta acontecendo"))

        # Banner de fase
        self._phase_banner = QLabel()
        self._phase_banner.setWordWrap(True)
        self._phase_banner.setStyleSheet(
            f"color: {C['text']}; font-size: 10pt; font-weight: bold; "
            f"background: rgba(255,255,255,0.04); border-radius: 6px; padding: 6px 8px;"
        )
        layout.addWidget(self._phase_banner)

        # Scroll area para requests ativas
        scroll = QScrollArea()
        scroll.setWidgetResizable(True)
        scroll.setHorizontalScrollBarPolicy(Qt.ScrollBarAlwaysOff)
        scroll.setStyleSheet("background: transparent; border: none;")

        self._container = QWidget()
        self._container.setAttribute(Qt.WA_TranslucentBackground)
        self._container_layout = QVBoxLayout(self._container)
        self._container_layout.setContentsMargins(0, 0, 0, 0)
        self._container_layout.setSpacing(2)
        self._container_layout.addStretch()
        scroll.setWidget(self._container)
        layout.addWidget(scroll, 1)

        self._expanded: set[int] = set()  # group_ids expandidos
        self._items: list[QWidget] = []
        self._last_snapshot = ""
        self._timer = QTimer(self)
        self._timer.setInterval(150)
        self._timer.timeout.connect(self._refresh)
        self._timer.start()

    def _refresh(self):
        active = self.sim.active_requests
        phase = self.sim.phase

        if active:
            snapshot = f"{phase.name}:{len(active)}:{active[0].phase.name if active[0].phase else ''}"
        else:
            snapshot = f"{phase.name}:0"
        if snapshot == self._last_snapshot:
            return
        self._last_snapshot = snapshot
        self._rebuild(active, phase)

    def _rebuild(self, active, phase):
        for item in self._items:
            item.setParent(None)
            item.deleteLater()
        self._items = []

        # Banner de fase
        if phase and phase not in (Phase.IDLE, Phase.DONE):
            icon = _PHASE_ICONS.get(phase, "")
            desc = _PHASE_DESCRIPTIONS.get(phase, phase.name)
            self._phase_banner.setText(f"{icon} {desc}")
            self._phase_banner.setVisible(True)
        elif phase == Phase.DONE:
            self._phase_banner.setText("\u2728 Ciclo completo!")
            self._phase_banner.setVisible(True)
        else:
            self._phase_banner.setText("")
            self._phase_banner.setVisible(False)

        # Se não há requests ativas, mostra info_text
        if not active:
            lbl = _body(self.sim.info_text)
            self._container_layout.insertWidget(0, lbl)
            self._items.append(lbl)
            return

        # Lista de requests ativas — cada uma clicável com dropdown
        for req in active:
            item = self._make_item(req)
            self._container_layout.insertWidget(len(self._items), item)
            self._items.append(item)

    def _make_item(self, req) -> QWidget:
        group_id = req.group_id
        phase = req.phase
        phase_name = phase.name.replace('_', ' ').title() if phase else "?"
        color = req.color if req.color else C["text"]
        cross = " [cross]" if req.is_cross_group else ""
        is_open = group_id in self._expanded

        widget = QWidget()
        widget.setAttribute(Qt.WA_TranslucentBackground)
        vl = QVBoxLayout(widget)
        vl.setContentsMargins(0, 0, 0, 0)
        vl.setSpacing(0)

        arrow = "\u25bc" if is_open else "\u25b6"
        btn = QPushButton(f" {arrow}  G{group_id} | N{req.leader} | {phase_name}{cross}")
        btn.setStyleSheet(
            f"QPushButton {{ text-align: left; color: {color}; font-size: 9pt; "
            f"font-family: Consolas; background: rgba(255,255,255,0.04); "
            f"border: none; border-left: 3px solid {color}; border-radius: 4px; padding: 4px 6px; }}"
            f"QPushButton:hover {{ background: rgba(255,255,255,0.08); }}"
        )
        btn.setCursor(Qt.PointingHandCursor)
        btn.clicked.connect(lambda checked, gid=group_id: self._toggle(gid))
        vl.addWidget(btn)

        if is_open:
            detail = _body(self.sim.info_text)
            detail.setStyleSheet(
                f"color: {C['text2']}; font-size: 8pt; border: none; "
                f"background: rgba(255,255,255,0.02); padding: 4px 12px; border-radius: 4px;"
            )
            vl.addWidget(detail)

        return widget

    def _toggle(self, group_id: int):
        if group_id in self._expanded:
            self._expanded.discard(group_id)
        else:
            self._expanded.add(group_id)
        self._last_snapshot = ""
        self._refresh()
