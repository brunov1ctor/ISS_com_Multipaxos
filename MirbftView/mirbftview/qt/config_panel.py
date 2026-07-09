"""Painel de Configuracao — parametriza a simulacao como generate-config.sh."""

import json
import os

from PySide6.QtWidgets import (
    QWidget, QVBoxLayout, QHBoxLayout, QLabel, QPushButton,
    QSpinBox, QComboBox, QScrollArea, QFrame, QLineEdit, QSizePolicy,
)
from PySide6.QtCore import Qt, Signal

from mirbftview.qt.theme import C
from mirbftview.qt.simulation import Simulation, Node, Client, Group


_PRESETS_DIR = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", "presets")

_BUILTIN_PRESETS = {
    "Minimal (1 grupo, 3 nos)": {
        "num_nodes": 3,
        "num_clients": 1,
        "num_groups": 1,
        "nodes_per_group": 3,
        "orderer": "MultiPaxosMulticast",
        "leader_policy": "Simple",
        "batch_size": 1,
        "batch_timeout": 1000,
        "num_buckets": 3,
        "segment_length": 4,
        "checkpoint_interval": 4,
        "cross_op_pct": 0,
        "view_change_timeout": 60000,
    },
    "Padrao ISS (4 grupos, 5 nos)": {
        "num_nodes": 5,
        "num_clients": 4,
        "num_groups": 4,
        "nodes_per_group": 3,
        "orderer": "MultiPaxosMulticast",
        "leader_policy": "Simple",
        "batch_size": 4096,
        "batch_timeout": 1000,
        "num_buckets": 16,
        "segment_length": 16,
        "checkpoint_interval": 80,
        "cross_op_pct": 30,
        "view_change_timeout": 60000,
    },

}


class _NoScrollSpinBox(QSpinBox):
    def wheelEvent(self, event):
        event.ignore()


class _NoScrollComboBox(QComboBox):
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
    """Painel para configurar parametros do protocolo e topologia."""

    config_applied = Signal()

    def __init__(self, sim: Simulation, parent=None):
        super().__init__(parent)
        self.sim = sim
        self._hidden_presets = set()
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

        # Presets
        layout.addWidget(_section("Presets"))

        self._presets_container = QWidget()
        self._presets_container.setAttribute(Qt.WA_TranslucentBackground)
        self._presets_layout = QVBoxLayout(self._presets_container)
        self._presets_layout.setContentsMargins(0, 0, 0, 0)
        self._presets_layout.setSpacing(4)
        layout.addWidget(self._presets_container)

        self._rebuild_preset_cards()

        # Topologia
        layout.addWidget(_section("Topologia"))

        self._num_nodes = _NoScrollSpinBox()
        self._num_nodes.setRange(2, 128)
        self._num_nodes.setValue(5)
        layout.addLayout(_row("No de Nos:", self._num_nodes))

        self._num_clients = _NoScrollSpinBox()
        self._num_clients.setRange(1, 32)
        self._num_clients.setValue(4)
        layout.addLayout(_row("No de Clientes:", self._num_clients))

        self._num_groups = _NoScrollSpinBox()
        self._num_groups.setRange(1, 8)
        self._num_groups.setValue(4)
        layout.addLayout(_row("No de Grupos (dados):", self._num_groups))

        self._nodes_per_group = _NoScrollSpinBox()
        self._nodes_per_group.setRange(2, 16)
        self._nodes_per_group.setValue(3)
        layout.addLayout(_row("Nos por Grupo:", self._nodes_per_group))

        # Orderer
        layout.addWidget(_section("Orderer / Consenso"))

        self._orderer = _NoScrollComboBox()
        self._orderer.addItems(["MultiPaxosMulticast", "MultiPaxos", "Pbft", "HotStuff", "Raft"])
        self._orderer.setCurrentText("MultiPaxosMulticast")
        layout.addLayout(_row("Orderer:", self._orderer))

        self._leader_policy = _NoScrollComboBox()
        self._leader_policy.addItems(["Simple", "Single", "Blacklist", "Backoff"])
        layout.addLayout(_row("Leader Policy:", self._leader_policy))

        self._batch_size = _NoScrollSpinBox()
        self._batch_size.setRange(1, 65536)
        self._batch_size.setValue(4096)
        layout.addLayout(_row("Batch Size:", self._batch_size))

        self._batch_timeout = _NoScrollSpinBox()
        self._batch_timeout.setRange(10, 60000)
        self._batch_timeout.setValue(1000)
        self._batch_timeout.setSuffix(" ms")
        layout.addLayout(_row("Batch Timeout:", self._batch_timeout))

        self._num_buckets = _NoScrollSpinBox()
        self._num_buckets.setRange(1, 1024)
        self._num_buckets.setValue(16)
        layout.addLayout(_row("No Buckets:", self._num_buckets))

        self._segment_length = _NoScrollSpinBox()
        self._segment_length.setRange(1, 1024)
        self._segment_length.setValue(16)
        layout.addLayout(_row("Segment Length:", self._segment_length))

        # Checkpoint
        layout.addWidget(_section("Checkpoint"))

        self._checkpoint_interval = _NoScrollSpinBox()
        self._checkpoint_interval.setRange(1, 1024)
        self._checkpoint_interval.setValue(80)
        layout.addLayout(_row("Intervalo (commits):", self._checkpoint_interval))

        # Workload
        layout.addWidget(_section("Workload"))

        self._cross_op_pct = _NoScrollSpinBox()
        self._cross_op_pct.setRange(0, 100)
        self._cross_op_pct.setValue(30)
        self._cross_op_pct.setSuffix(" %")
        layout.addLayout(_row("Cross-group ops:", self._cross_op_pct))

        # Rede
        layout.addWidget(_section("Rede"))

        self._view_change_timeout = _NoScrollSpinBox()
        self._view_change_timeout.setRange(1000, 120000)
        self._view_change_timeout.setValue(60000)
        self._view_change_timeout.setSuffix(" ms")
        layout.addLayout(_row("ViewChange Timeout:", self._view_change_timeout))

        # Apply
        layout.addSpacing(16)
        self._btn_apply = QPushButton("Aplicar Configuracao")
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

        self._btn_save = QPushButton("+ Salvar config atual como preset")
        self._btn_save.setMinimumHeight(30)
        self._btn_save.setStyleSheet(
            f"QPushButton {{ background: rgba(255,255,255,0.06); color: {C['text2']}; "
            f"font-size: 8pt; border: 1px dashed rgba(255,255,255,0.2); border-radius: 6px; padding: 4px 10px; }}"
            f"QPushButton:hover {{ background: rgba(255,255,255,0.12); color: {C['text']}; }}"
        )
        self._btn_save.clicked.connect(self._show_save_input)
        layout.addWidget(self._btn_save)

        self._save_row = QWidget()
        self._save_row.setVisible(False)
        sr_layout = QHBoxLayout(self._save_row)
        sr_layout.setContentsMargins(0, 0, 0, 0)
        sr_layout.setSpacing(6)
        self._preset_name_input = QLineEdit()
        self._preset_name_input.setPlaceholderText("Nome do preset...")
        self._preset_name_input.setMinimumHeight(30)
        self._preset_name_input.setStyleSheet(
            f"background: rgba(10,16,30,0.7); color: {C['text']}; font-size: 8pt;"
            f" border: 1px solid rgba(255,255,255,0.15); border-radius: 6px; padding: 4px 8px;"
        )
        self._preset_name_input.returnPressed.connect(self._save_preset)
        sr_layout.addWidget(self._preset_name_input)
        btn_confirm = QPushButton("OK")
        btn_confirm.setMinimumHeight(30)
        btn_confirm.setStyleSheet(
            f"QPushButton {{ background: rgba(52,211,153,0.15); color: {C['green']}; "
            f"font-size: 8pt; font-weight: bold; border: none; border-radius: 6px; padding: 4px 10px; }}"
            f"QPushButton:hover {{ background: rgba(52,211,153,0.3); }}"
        )
        btn_confirm.clicked.connect(self._save_preset)
        sr_layout.addWidget(btn_confirm)
        layout.addWidget(self._save_row)

        self._status = QLabel("")
        self._status.setStyleSheet(f"color: {C['green']}; font-size: 8pt;")
        layout.addWidget(self._status)

        layout.addStretch()

        scroll.setWidget(container)
        outer = QVBoxLayout(self)
        outer.setContentsMargins(0, 0, 0, 0)
        outer.addWidget(scroll)

    def _apply(self):
        num_nodes = self._num_nodes.value()
        num_clients = self._num_clients.value()
        num_groups = self._num_groups.value()
        nodes_per_group = min(self._nodes_per_group.value(), num_nodes)

        if nodes_per_group < 2:
            self._status.setText("Minimo 2 nos por grupo para quorum")
            return
        if num_nodes < 3:
            self._status.setText("Minimo 3 nos para tolerancia a faltas")
            return

        self.sim.nodes = [Node(i, f"N{i}", [0]) for i in range(num_nodes)]
        self.sim.clients = [Client(i, f"Client {i}") for i in range(num_clients)]

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

        self.sim.apply_config(
            num_buckets=self._num_buckets.value(),
            segment_length=self._segment_length.value(),
            batch_size=self._batch_size.value(),
            batch_timeout_ticks=self._batch_timeout.value() // 16,
            checkpoint_interval=self._checkpoint_interval.value(),
            cross_op_pct=self._cross_op_pct.value() / 100.0,
            view_change_timeout=self._view_change_timeout.value() // 16,
        )

        self.sim.info_text = (
            f"Configuracao aplicada!\n"
            f"Nos: {num_nodes} | Clientes: {num_clients} | Grupos: {num_groups}\n"
            f"Nos/grupo: {nodes_per_group} | Buckets: {self._num_buckets.value()}\n"
            f"Pressione Iniciar para rodar."
        )

        self._status.setText(f"Aplicado: {num_nodes} nos, {num_groups} grupos, {self._num_buckets.value()} buckets")
        self.config_applied.emit()

    # --- Presets ---

    def _get_current_config(self) -> dict:
        return {
            "num_nodes": self._num_nodes.value(),
            "num_clients": self._num_clients.value(),
            "num_groups": self._num_groups.value(),
            "nodes_per_group": self._nodes_per_group.value(),
            "orderer": self._orderer.currentText(),
            "leader_policy": self._leader_policy.currentText(),
            "batch_size": self._batch_size.value(),
            "batch_timeout": self._batch_timeout.value(),
            "num_buckets": self._num_buckets.value(),
            "segment_length": self._segment_length.value(),
            "checkpoint_interval": self._checkpoint_interval.value(),
            "cross_op_pct": self._cross_op_pct.value(),
            "view_change_timeout": self._view_change_timeout.value(),
        }

    def _apply_config_dict(self, cfg: dict):
        self._num_nodes.setValue(cfg.get("num_nodes", 5))
        self._num_clients.setValue(cfg.get("num_clients", 4))
        self._num_groups.setValue(cfg.get("num_groups", 4))
        self._nodes_per_group.setValue(cfg.get("nodes_per_group", 3))
        self._orderer.setCurrentText(cfg.get("orderer", "MultiPaxosMulticast"))
        self._leader_policy.setCurrentText(cfg.get("leader_policy", "Simple"))
        self._batch_size.setValue(cfg.get("batch_size", 4096))
        self._batch_timeout.setValue(cfg.get("batch_timeout", 1000))
        self._num_buckets.setValue(cfg.get("num_buckets", 16))
        self._segment_length.setValue(cfg.get("segment_length", 16))
        self._checkpoint_interval.setValue(cfg.get("checkpoint_interval", 80))
        self._cross_op_pct.setValue(cfg.get("cross_op_pct", 30))
        self._view_change_timeout.setValue(cfg.get("view_change_timeout", 60000))

    def _load_preset(self, name: str):
        if name in _BUILTIN_PRESETS:
            self._apply_config_dict(_BUILTIN_PRESETS[name])
            self._status.setText(f"Preset '{name}' carregado")
        else:
            path = os.path.join(_PRESETS_DIR, f"{name}.json")
            if os.path.exists(path):
                with open(path, "r", encoding="utf-8") as f:
                    cfg = json.load(f)
                self._apply_config_dict(cfg)
                self._status.setText(f"Preset '{name}' carregado")

    def _show_save_input(self):
        self._save_row.setVisible(True)
        self._preset_name_input.setFocus()

    def _save_preset(self):
        name = self._preset_name_input.text().strip()
        if not name:
            self._status.setText("Digite um nome para o preset")
            return
        os.makedirs(_PRESETS_DIR, exist_ok=True)
        path = os.path.join(_PRESETS_DIR, f"{name}.json")
        cfg = self._get_current_config()
        with open(path, "w", encoding="utf-8") as f:
            json.dump(cfg, f, indent=2)
        self._preset_name_input.clear()
        self._save_row.setVisible(False)
        self._status.setText(f"Preset '{name}' salvo")
        self._rebuild_preset_cards()

    def _delete_preset(self, name: str):
        if name in _BUILTIN_PRESETS:
            self._hidden_presets.add(name)
        else:
            path = os.path.join(_PRESETS_DIR, f"{name}.json")
            if os.path.exists(path):
                os.remove(path)
        self._status.setText(f"Preset '{name}' excluido")
        self._rebuild_preset_cards()

    def _rebuild_preset_cards(self):
        while self._presets_layout.count():
            item = self._presets_layout.takeAt(0)
            if item.widget():
                item.widget().deleteLater()
        for name, cfg in _BUILTIN_PRESETS.items():
            if name not in self._hidden_presets:
                self._presets_layout.addWidget(self._make_card(name, cfg, True))
        if os.path.isdir(_PRESETS_DIR):
            for fname in sorted(os.listdir(_PRESETS_DIR)):
                if fname.endswith(".json"):
                    pname = fname[:-5]
                    try:
                        with open(os.path.join(_PRESETS_DIR, fname), "r", encoding="utf-8") as f:
                            cfg = json.load(f)
                        self._presets_layout.addWidget(self._make_card(pname, cfg, False))
                    except Exception:
                        pass

    def _edit_preset(self, name: str):
        """Sobrescreve um preset salvo com a config atual."""
        if name in _BUILTIN_PRESETS:
            return
        path = os.path.join(_PRESETS_DIR, f"{name}.json")
        cfg = self._get_current_config()
        with open(path, "w", encoding="utf-8") as f:
            json.dump(cfg, f, indent=2)
        self._status.setText(f"Preset '{name}' atualizado")
        self._rebuild_preset_cards()

    def _make_card(self, name: str, cfg: dict, is_builtin: bool) -> QWidget:
        card = QWidget()
        card.setObjectName("presetCard")
        card.setCursor(Qt.PointingHandCursor)
        card.setStyleSheet(
            f"QWidget#presetCard {{ background: {C['card']}; border: 1px solid {C['border']}; border-radius: 10px; }}"
            f"QWidget#presetCard:hover {{ background: {C['card_hover']}; border-color: rgba(255,255,255,0.2); }}"
        )
        vl = QVBoxLayout(card)
        vl.setContentsMargins(10, 8, 10, 8)
        vl.setSpacing(4)

        # Header row: name + X
        header = QHBoxLayout()
        header.setSpacing(6)
        title = QLabel(name)
        title.setStyleSheet(f"color: {C['text']}; font-size: 9pt; font-weight: bold; border: none; background: transparent;")
        title.setAttribute(Qt.WA_TransparentForMouseEvents)
        header.addWidget(title, 1)

        btn_del = QLabel("X")
        btn_del.setCursor(Qt.PointingHandCursor)
        btn_del.setFixedSize(22, 22)
        btn_del.setAlignment(Qt.AlignCenter)
        btn_del.setStyleSheet(
            f"color: {C['red']}; font-size: 9pt; font-weight: bold; border: none; background: transparent;"
        )
        header.addWidget(btn_del)
        vl.addLayout(header)

        # mousePressEvent: check if click is on X
        def _on_card_click(ev, n=name, del_widget=btn_del):
            global_pos = ev.globalPosition().toPoint() if hasattr(ev, 'globalPosition') else ev.globalPos()
            del_rect = del_widget.rect()
            del_pos = del_widget.mapFromGlobal(global_pos)
            if del_rect.contains(del_pos):
                self._delete_preset(n)
            else:
                self._load_preset(n)
        card.mousePressEvent = _on_card_click

        # Details row
        details = f"{cfg.get('num_nodes','?')} nos | {cfg.get('num_groups','?')} grupos | {cfg.get('orderer','?')} | batch {cfg.get('batch_size','?')}"
        det_lbl = QLabel(details)
        det_lbl.setStyleSheet(f"color: {C['text3']}; font-size: 7pt; border: none; background: transparent;")
        det_lbl.setWordWrap(True)
        det_lbl.setAttribute(Qt.WA_TransparentForMouseEvents)
        vl.addWidget(det_lbl)



        return card
