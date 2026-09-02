"""ControlBar — controles de simulação."""

from PySide6.QtWidgets import (
    QWidget, QHBoxLayout, QPushButton, QLabel, QSlider, QSizePolicy,
    QMenu, QWidgetAction, QCheckBox
)
from PySide6.QtCore import Qt, Signal

from mirbftview.qt.theme import C
from mirbftview.qt.simulation import Simulation, Phase


class ControlBar(QWidget):
    """Controles: Iniciar, Pausar, Próximo, Reset, Config + velocidade."""

    config_toggled = Signal()
    reset_requested = Signal()
    log_toggled = Signal()

    def __init__(self, sim: Simulation, parent=None):
        super().__init__(parent)
        self.sim = sim
        self.setAttribute(Qt.WA_TranslucentBackground)
        self.setMinimumHeight(40)
        self.setSizePolicy(QSizePolicy.Expanding, QSizePolicy.Preferred)

        layout = QHBoxLayout(self)
        layout.setContentsMargins(8, 4, 8, 4)
        layout.setSpacing(6)

        title = QLabel("MirBFT View")
        title.setStyleSheet(f"color: {C['primary']}; font-size: 12pt; font-weight: bold;")
        layout.addWidget(title)
        layout.addSpacing(8)

        self._btn_play = self._make_btn("\u25b6 Iniciar", "primary", self._toggle_play)
        layout.addWidget(self._btn_play)
        self._btn_next = self._make_btn("\u23ed Pr\u00f3ximo", None, self._next)
        layout.addWidget(self._btn_next)
        self._btn_reset = self._make_btn("\u21ba Reset", None, self._reset)
        layout.addWidget(self._btn_reset)
        layout.addSpacing(8)

        lbl = QLabel("Velocidade:")
        lbl.setStyleSheet(f"color: {C['text3']}; font-size: 8pt;")
        layout.addWidget(lbl)

        self._slider = QSlider(Qt.Horizontal)
        self._slider.setRange(5, 150)
        self._slider.setValue(30)
        self._slider.setMinimumWidth(60)
        self._slider.setMaximumWidth(150)
        self._slider.setSizePolicy(QSizePolicy.Expanding, QSizePolicy.Fixed)
        self._slider.valueChanged.connect(self._on_speed)
        layout.addWidget(self._slider)

        self._speed_lbl = QLabel("0.3x")
        self._speed_lbl.setStyleSheet(f"color: {C['accent']}; font-size: 9pt; font-weight: bold;")
        self._speed_lbl.setMinimumWidth(30)
        layout.addWidget(self._speed_lbl)
        layout.addStretch()

        self._btn_scenarios = QPushButton("Cen\u00e1rios")
        self._btn_scenarios.clicked.connect(self._show_scenarios_menu)
        layout.addWidget(self._btn_scenarios)

        self._btn_log = QPushButton("Log")
        self._btn_log.clicked.connect(lambda: self.log_toggled.emit())
        layout.addWidget(self._btn_log)

        self._btn_config = self._make_btn("\u2699 Config", None, lambda: self.config_toggled.emit())
        layout.addWidget(self._btn_config)

        self._mode_lbl = QLabel("Modo: Passo a passo")
        self._mode_lbl.setStyleSheet(f"color: {C['text3']}; font-size: 8pt;")
        layout.addWidget(self._mode_lbl)

    def _make_btn(self, text, obj_name, callback):
        b = QPushButton(text)
        if obj_name:
            b.setObjectName(obj_name)
        b.clicked.connect(callback)
        return b

    def _toggle_play(self):
        if self.sim.phase in (Phase.IDLE, Phase.DONE):
            self.sim.start()
            self._btn_play.setText("\u23f8 Pausar")
            self._mode_lbl.setText("Modo: Cont\u00ednuo")
        elif self.sim.paused:
            self.sim.toggle_pause()
            self._btn_play.setText("\u23f8 Pausar")
            self._mode_lbl.setText("Modo: Cont\u00ednuo")
        else:
            self.sim.toggle_pause()
            self._btn_play.setText("\u25b6 Retomar")
            self._mode_lbl.setText("PAUSADO")

    def _next(self):
        if self.sim.phase in (Phase.IDLE, Phase.DONE):
            self.sim.start()
        self.sim._paused = True
        self.sim._step_mode = True
        self.sim.step_next()
        self._btn_play.setText("\u25b6 Retomar")
        self._mode_lbl.setText("Modo: Passo a passo")

    def _reset(self):
        self.sim.reset()
        self._btn_play.setText("\u25b6 Iniciar")
        self._mode_lbl.setText("Resetado")
        self.reset_requested.emit()

    def _on_speed(self, val):
        speed = val / 100.0
        self.sim.set_speed(speed)
        self._speed_lbl.setText(f"{speed:.1f}x")

    def _show_scenarios_menu(self):
        menu = QMenu(self)
        menu.setStyleSheet(
            f"QMenu {{ background: rgba(10,18,36,0.95); border: 1px solid rgba(255,255,255,0.15); "
            f"border-radius: 8px; padding: 6px; }}"
            f"QMenu::item {{ color: {C['text']}; padding: 4px 12px; }}"
            f"QCheckBox {{ color: {C['text']}; font-size: 9pt; spacing: 6px; padding: 4px 8px; }}"
            f"QToolTip {{ background: rgba(10,18,36,0.98); color: {C['text']}; "
            f"border: 1px solid rgba(255,255,255,0.2); padding: 6px; font-size: 8pt; }}"
        )
        scenarios = [
            ("single_request", "Uma mensagem por vez (didatico)",
             "Restringe o regime estacionario a 1 grupo de dados por vez,\n"
             "em rodizio, em vez dos N grupos em paralelo. So afeta o\n"
             "trafego apos o bootstrap inicial (que sempre prepara todos\n"
             "os grupos de uma vez, como no Start() real). Tambem alterna\n"
             "cross-group e single-group a cada novo envio, em vez de\n"
             "sortear pela taxa configurada, para mostrar sempre os dois\n"
             "fluxos em sequencia."),
            ("timeout", "Timeout / Retransmissao",
             "Com chance aleatoria, simula o lider nao recebendo quorum\n"
             "de ACCEPTED a tempo e retransmitindo o ACCEPT (acceptRtxEvery\n"
             "no codigo real). Mecanismo real de tolerancia a mensagens\n"
             "perdidas ou atrasadas — reenviar e seguro, duplicatas sao\n"
             "ignoradas pelo protocolo."),
            ("node_failure", "Falha de no",
             "Com chance aleatoria, simula a queda do lider atual antes do\n"
             "ACCEPT e dispara um View Change: os demais membros elegem um\n"
             "novo lider com ballot maior. Mecanismo real de tolerancia a\n"
             "falhas do MultiPaxos."),
            ("cross_group", "Forcar cross-group",
             "Forca toda nova requisicao a ser cross-group (tocar 2 grupos\n"
             "distintos), em vez de sortear aleatoriamente pela taxa\n"
             "configurada (cross_op_pct). Util para ver sempre o fluxo\n"
             "completo com GSN/META/ADeliver, sem esperar o sorteio."),
            ("checkpoint_force", "Checkpoint a cada commit",
             "Forca um CHECKPOINT depois de toda unica decisao, em vez de\n"
             "esperar o intervalo normal (checkpoint_interval, por padrao\n"
             "a cada 80 commits). O checkpoint em si e real e periodico;\n"
             "isso so exagera a frequencia para voce ver o evento sem\n"
             "esperar. No MultiPaxosMulticastOrderer, um checkpoint so\n"
             "permite truncar o log — nao troca lider nem redistribui\n"
             "buckets (grupos sao estaticos)."),
            ("adeliver_block", "ADeliver bloqueado",
             "Forca artificialmente, numa operacao cross-group, o bloqueio\n"
             "real do ADeliver: um grupo so entrega um GSN depois de ja ter\n"
             "entregue todo GSN anterior que tambem o toca. No simulador,\n"
             "com poucos grupos e poucas operacoes, esse bloqueio natural\n"
             "e raro — o cenario garante que voce veja o log 'BLOQUEADO'\n"
             "sem depender de coincidencia."),
            ("batch_resurrect", "Batch Resurrect",
             "Com chance aleatoria, simula uma instancia desistindo de\n"
             "esperar quorum para uma posicao do log (mecanismo real de\n"
             "Eventual Progress): ela commita um valor nulo e devolve a\n"
             "fila do bucket as requisicoes ja cortadas em lote mas ainda\n"
             "nao decididas, para serem re-propostas depois."),
        ]
        for key, label, tooltip in scenarios:
            cb = QCheckBox(label)
            cb.setChecked(self.sim.get_scenario(key))
            cb.setToolTip(tooltip)
            cb.toggled.connect(lambda checked, k=key, lbl=label: self.sim.set_scenario(k, checked, lbl))
            action = QWidgetAction(menu)
            action.setDefaultWidget(cb)
            menu.addAction(action)
        menu.exec(self._btn_scenarios.mapToGlobal(self._btn_scenarios.rect().bottomLeft()))
