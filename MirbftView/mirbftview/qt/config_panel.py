"""Painel de Configuração — parametriza a simulação como generate-config.sh."""

from PySide6.QtWidgets import (
    QWidget, QVBoxLayout, QHBoxLayout, QLabel, QPushButton,
    QSpinBox, QComboBox, QScrollArea, QFrame, QTextEdit, QSizePolicy
)
from PySide6.QtCore import Qt, Signal

from mirbftview.qt.theme import C
from mirbftview.qt.simulation import Simulation, Node, Client, Group


class _NoScrollSpinBox(QSpinBox):
    """SpinBox que ignora wheel event (evita alterar valor ao scrollar)."""
    def wheelEvent(self, event):
        event.ignore()


class _NoScrollComboBox(QComboBox):
    """ComboBox que ignora wheel event."""
    def wheelEvent(self, event):
        event.ignore()


def _section(text):
    lbl = QLabel(text)
    lbl.setStyleSheet(f"color: {C['primary']}; font-size: 10pt; font-weight: bold; border: none; background: transparent; margin-top: 8px;")
    return lbl


def _label(text):
    lbl = QLabel(text)
    lbl.setStyleSheet(f"color: {C['text2']}; font-size: 8pt; border: none; background: transparent;")
    return lbl


def _row(label_text, widget):
    row = QHBoxLayout()
    row.setSpacing(8)
    lbl = _label(label_text)
    lbl.setMinimumWidth(90)
    lbl.setMaximumWidth(140)
    lbl.setSizePolicy(QSizePolicy.Preferred, QSizePolicy.Fixed)
    row.addWidget(lbl)
    widget.setMinimumHeight(32)
    widget.setSizePolicy(QSizePolicy.Expanding, QSizePolicy.Fixed)
    widget.setStyleSheet(
        f"QSpinBox, QComboBox {{"
        f"  min-height: 28px; font-size: 10pt; padding: 2px 6px;"
        f"  background: rgba(10,16,30,0.7); color: {C['text']};"
        f"  border: 1px solid rgba(255,255,255,0.15); border-radius: 6px;"
        f"}}"
        f"QSpinBox::up-button, QSpinBox::down-button {{"
        f"  width: 24px; height: 14px;"
        f"}}"
    )
    row.addWidget(widget)
    return row


class ConfigPanel(QWidget):
    """Painel para configurar parâmetros do protocolo e topologia."""

    config_applied = Signal()

    def __init__(self, sim: Simulation, parent=None):
        super().__init__(parent)
        self.sim = sim
        self.setAttribute(Qt.WA_TranslucentBackground)

        scroll = QScrollArea(self)
        scroll.setWidgetResizable(True)
        scroll.setHorizontalScrollBarPolicy(Qt.ScrollBarAlwaysOff)
        scroll.setStyleSheet("background: transparent; border: none;")

        container = QWidget()
        container.setAttribute(Qt.WA_TranslucentBackground)
        layout = QVBoxLayout(container)
        layout.setContentsMargins(12, 12, 12, 12)
        layout.setSpacing(6)

        # ── Topologia ─────────────────────────────────────────────────────────
        layout.addWidget(_section("🌐 Topologia"))

        self._num_nodes = _NoScrollSpinBox()
        self._num_nodes.setRange(2, 128)
        self._num_nodes.setValue(5)  # systemSizes=5
        layout.addLayout(_row("Nº de Nós:", self._num_nodes))

        self._num_clients = _NoScrollSpinBox()
        self._num_clients.setRange(1, 32)
        self._num_clients.setValue(4)  # clients1=4
        layout.addLayout(_row("Nº de Clientes:", self._num_clients))

        self._num_groups = _NoScrollSpinBox()
        self._num_groups.setRange(1, 8)
        self._num_groups.setValue(4)
        layout.addLayout(_row("Nº de Grupos (dados):", self._num_groups))

        self._nodes_per_group = _NoScrollSpinBox()
        self._nodes_per_group.setRange(2, 16)
        self._nodes_per_group.setValue(3)
        layout.addLayout(_row("Nós por Grupo:", self._nodes_per_group))

        # ── Orderer ───────────────────────────────────────────────────────────
        layout.addWidget(_section("⚙️ Orderer / Consenso"))

        self._orderer = _NoScrollComboBox()
        self._orderer.addItems(["MultiPaxosMulticast", "MultiPaxos", "Pbft", "HotStuff", "Raft"])
        self._orderer.setCurrentText("MultiPaxosMulticast")
        layout.addLayout(_row("Orderer:", self._orderer))

        self._leader_policy = _NoScrollComboBox()
        self._leader_policy.addItems(["Simple", "Single", "Blacklist", "Backoff"])
        layout.addLayout(_row("Leader Policy:", self._leader_policy))

        self._batch_size = _NoScrollSpinBox()
        self._batch_size.setRange(1, 65536)
        self._batch_size.setValue(4096)  # batchsizes=4096
        layout.addLayout(_row("Batch Size:", self._batch_size))

        self._batch_timeout = _NoScrollSpinBox()
        self._batch_timeout.setRange(10, 60000)
        self._batch_timeout.setValue(1000)  # minBatchTimeout=1000ms
        self._batch_timeout.setSuffix(" ms")
        layout.addLayout(_row("Batch Timeout:", self._batch_timeout))

        self._num_buckets = _NoScrollSpinBox()
        self._num_buckets.setRange(1, 1024)
        self._num_buckets.setValue(16)  # minBuckets=16 (bucketsPerLeader=16)
        layout.addLayout(_row("Nº Buckets:", self._num_buckets))

        self._segment_length = _NoScrollSpinBox()
        self._segment_length.setRange(1, 1024)
        self._segment_length.setValue(16)  # segmentLengths=16
        layout.addLayout(_row("Segment Length:", self._segment_length))

        # ── Checkpoint ────────────────────────────────────────────────────────
        layout.addWidget(_section("🏁 Checkpoint"))

        self._checkpoint_interval = _NoScrollSpinBox()
        self._checkpoint_interval.setRange(1, 1024)
        self._checkpoint_interval.setValue(80)  # epoch = segmentLength*numPeers = 16*5 = 80
        layout.addLayout(_row("Intervalo (commits):", self._checkpoint_interval))

        self._checkpointer = _NoScrollComboBox()
        self._checkpointer.addItems(["Simple", "Signing"])
        layout.addLayout(_row("Checkpointer:", self._checkpointer))

        # ── Workload ──────────────────────────────────────────────────────────
        layout.addWidget(_section("📊 Workload"))

        self._cross_op_pct = _NoScrollSpinBox()
        self._cross_op_pct.setRange(0, 100)
        self._cross_op_pct.setValue(30)
        self._cross_op_pct.setSuffix(" %")
        layout.addLayout(_row("Cross-group ops:", self._cross_op_pct))

        self._payload_size = _NoScrollSpinBox()
        self._payload_size.setRange(0, 10000)
        self._payload_size.setValue(500)
        self._payload_size.setSuffix(" bytes")
        layout.addLayout(_row("Payload Size:", self._payload_size))

        # ── Rede ──────────────────────────────────────────────────────────────
        layout.addWidget(_section("🔗 Rede"))

        self._view_change_timeout = _NoScrollSpinBox()
        self._view_change_timeout.setRange(1000, 120000)
        self._view_change_timeout.setValue(60000)  # viewChangeTimeouts=60000ms
        self._view_change_timeout.setSuffix(" ms")
        layout.addLayout(_row("ViewChange Timeout:", self._view_change_timeout))

        self._failures = _NoScrollSpinBox()
        self._failures.setRange(0, 16)
        self._failures.setValue(0)  # failureCounts=(0)
        layout.addLayout(_row("Falhas simuladas:", self._failures))

        self._sign_requests = _NoScrollComboBox()
        self._sign_requests.addItems(["true", "false"])
        layout.addLayout(_row("Assinar Requests:", self._sign_requests))

        # ── Groups YAML ───────────────────────────────────────────────────────
        layout.addWidget(_section("📝 Groups (YAML)"))

        self._groups_yaml = QTextEdit()
        self._groups_yaml.setMinimumHeight(60)
        self._groups_yaml.setMaximumHeight(120)
        self._groups_yaml.setSizePolicy(QSizePolicy.Expanding, QSizePolicy.Preferred)
        self._groups_yaml.setStyleSheet(
            f"background: rgba(10,16,30,0.70); color: {C['accent']}; "
            f"border: 1px solid rgba(255,255,255,0.10); border-radius: 8px; "
            f"font-family: Consolas; font-size: 8pt; padding: 4px;"
        )
        self._groups_yaml.setPlainText("groups:\n  1: [0, 1, 2]\n  2: [2, 3, 4]\n  3: [0, 4, 1]\n  4: [1, 3, 4]")
        layout.addWidget(self._groups_yaml)

        # ── Apply button ──────────────────────────────────────────────────────
        layout.addSpacing(16)
        self._btn_apply = QPushButton("✓ Aplicar Configuração")
        self._btn_apply.setMinimumHeight(44)
        self._btn_apply.setStyleSheet(
            f"QPushButton {{"
            f"  background: {C['primary']}; color: #fff; font-size: 11pt; font-weight: bold;"
            f"  border: none; border-radius: 10px; padding: 8px 16px;"
            f"}}"
            f"QPushButton:hover {{ background: {C['accent']}; }}"
            f"QPushButton:pressed {{ background: {C['green']}; }}"
        )
        self._btn_apply.clicked.connect(self._apply)
        layout.addWidget(self._btn_apply)

        self._status = QLabel("")
        self._status.setStyleSheet(f"color: {C['green']}; font-size: 8pt;")
        layout.addWidget(self._status)

        layout.addStretch()

        scroll.setWidget(container)
        outer = QVBoxLayout(self)
        outer.setContentsMargins(0, 0, 0, 0)
        outer.addWidget(scroll)

    def _apply(self):
        """Aplica a configuração à simulação."""
        num_nodes = self._num_nodes.value()
        num_clients = self._num_clients.value()
        num_groups = self._num_groups.value()
        nodes_per_group = min(self._nodes_per_group.value(), num_nodes)

        # Validação de topologia
        if nodes_per_group < 2:
            self._status.setText("⚠ Mínimo 2 nós por grupo para quorum")
            return
        if num_nodes < 3:
            self._status.setText("⚠ Mínimo 3 nós para tolerância a faltas (f=1)")
            return
        if self._num_buckets.value() < num_nodes:
            self._status.setText(f"⚠ Buckets ({self._num_buckets.value()}) deve ser >= nós ({num_nodes})")
            return

        # Rebuild nodes
        self.sim.nodes = [Node(i, f"N{i}", [0]) for i in range(num_nodes)]

        # Rebuild clients
        self.sim.clients = [Client(i, f"Client {i}") for i in range(num_clients)]

        # Rebuild groups: G0 = sequencer (all nodes), G1..Gn = data groups
        self.sim.groups = [Group(0, "Sequenciador (G0)", list(range(num_nodes)))]
        for g in range(1, num_groups + 1):
            members = []
            for j in range(nodes_per_group):
                nid = ((g - 1) * nodes_per_group + j) % num_nodes
                members.append(nid)
            members = list(set(members))
            if len(members) < 2:
                members = list(range(min(2, num_nodes)))
            self.sim.groups.append(Group(g, f"Dados G{g}", members))
            for nid in members:
                if g not in self.sim.nodes[nid].groups:
                    self.sim.nodes[nid].groups.append(g)

        # Apply config via new API
        self.sim.apply_config(
            num_buckets=self._num_buckets.value(),
            segment_length=self._segment_length.value(),
            batch_size=self._batch_size.value(),
            batch_timeout_ticks=self._batch_timeout.value() // 16,  # ms → ticks (~16ms/tick)
            checkpoint_interval=self._checkpoint_interval.value(),
            cross_op_pct=self._cross_op_pct.value() / 100.0,
            view_change_timeout=self._view_change_timeout.value() // 16,
        )

        self.sim.info_text = (
            f"✓ Configuração aplicada!\n"
            f"Nós: {num_nodes} | Clientes: {num_clients} | Grupos: {num_groups}\n"
            f"Nós/grupo: {nodes_per_group} | Buckets: {self._num_buckets.value()}\n"
            f"Orderer: {self._orderer.currentText()}\n"
            f"Leader Policy: {self._leader_policy.currentText()}\n"
            f"Batch: {self._batch_size.value()} reqs / {self._batch_timeout.value()}ms timeout\n"
            f"Segment: {self._segment_length.value()} | Checkpoint: cada {self._checkpoint_interval.value()}\n"
            f"Cross-ops: {self._cross_op_pct.value()}%\n"
            f"\nPressione ▶ Iniciar para rodar com esta configuração."
        )

        self._status.setText(f"✓ Aplicado: {num_nodes} nós, {num_groups} grupos, {self._num_buckets.value()} buckets")
        self.config_applied.emit()
