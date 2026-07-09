"""MirBFT View — Visualização educacional do protocolo."""

import sys
from PySide6.QtWidgets import (
    QApplication, QMainWindow, QWidget, QVBoxLayout, QHBoxLayout,
    QSplitter, QSizePolicy, QScrollArea,
)
from PySide6.QtCore import Qt, QTimer, QPointF
from PySide6.QtGui import QResizeEvent

from mirbftview.qt.theme import STYLESHEET, C
from mirbftview.qt.widgets import GlassPanel, AmbientBackground
from mirbftview.qt.canvas import NetworkCanvas
from mirbftview.qt.panels import InfoPanel, BucketsPanel, ExecutionPanel, CommitChainPanel, EventLogPanel, GlobalOrderPanel
from mirbftview.qt.config_panel import ConfigPanel
from mirbftview.qt.control_bar import ControlBar
from mirbftview.qt.simulation import Simulation


class MirBFTViewWindow(QMainWindow):
    def __init__(self):
        super().__init__()
        self.setWindowTitle("MirBFT View — Visualização Educacional do Protocolo MultiPaxos/ISS")
        self.setMinimumSize(1100, 650)
        self.resize(1400, 820)

        self.sim = Simulation()
        self._build_ui()

    def _build_ui(self):
        central = AmbientBackground()
        self.setCentralWidget(central)
        self.setStyleSheet("QMainWindow, QMainWindow > QWidget { background: transparent; }")

        root = QVBoxLayout(central)
        root.setContentsMargins(10, 10, 10, 10)
        root.setSpacing(8)

        # ── Top: Controls ─────────────────────────────────────────────────────
        ctrl_shell = GlassPanel(radius=12, border_opacity=40)
        ctrl_layout = QVBoxLayout(ctrl_shell)
        ctrl_layout.setContentsMargins(0, 0, 0, 0)
        self._control_bar = ControlBar(self.sim)
        self._control_bar.config_toggled.connect(self._toggle_config)
        self._control_bar.reset_requested.connect(self._on_reset)
        self._control_bar.log_toggled.connect(self._toggle_log)
        ctrl_layout.addWidget(self._control_bar)
        root.addWidget(ctrl_shell)

        # ── Middle: Graph + Info panels ───────────────────────────────────────
        h_splitter = QSplitter(Qt.Horizontal)
        h_splitter.setHandleWidth(6)
        h_splitter.setStyleSheet("QSplitter{background:transparent;} QSplitter::handle{background:transparent;}")

        # Left: Network graph with config overlay
        graph_shell = GlassPanel(radius=16, border_opacity=40)
        graph_shell.setSizePolicy(QSizePolicy.Expanding, QSizePolicy.Expanding)
        graph_layout = QVBoxLayout(graph_shell)
        graph_layout.setContentsMargins(0, 0, 0, 0)
        self._canvas = NetworkCanvas(self.sim)
        self._canvas.setSizePolicy(QSizePolicy.Expanding, QSizePolicy.Expanding)
        graph_layout.addWidget(self._canvas)

        # Config panel overlay (child of canvas so it stays on top)
        self._config_panel = ConfigPanel(self.sim)
        self._config_panel.config_applied.connect(self._on_config_applied)
        self._config_shell = GlassPanel(radius=12, border_opacity=50, parent=self._canvas)
        self._config_shell.setStyleSheet("background: rgba(4,8,20,0.92); border-radius: 12px;")
        cfg_inner = QVBoxLayout(self._config_shell)
        cfg_inner.setContentsMargins(0, 0, 0, 0)
        cfg_scroll = QScrollArea()
        cfg_scroll.setWidgetResizable(True)
        cfg_scroll.setHorizontalScrollBarPolicy(Qt.ScrollBarAlwaysOff)
        cfg_scroll.setStyleSheet("background: transparent; border: none;")
        cfg_scroll.setWidget(self._config_panel)
        cfg_inner.addWidget(cfg_scroll)
        self._config_shell.setVisible(False)

        # Log panel overlay (right side of canvas)
        self._log_panel = EventLogPanel(self.sim)
        self._log_shell = GlassPanel(radius=12, border_opacity=50, parent=self._canvas)
        self._log_shell.setStyleSheet("background: rgba(4,8,20,0.92); border-radius: 12px;")
        log_inner = QVBoxLayout(self._log_shell)
        log_inner.setContentsMargins(0, 0, 0, 0)
        log_scroll = QScrollArea()
        log_scroll.setWidgetResizable(True)
        log_scroll.setHorizontalScrollBarPolicy(Qt.ScrollBarAlwaysOff)
        log_scroll.setStyleSheet("background: transparent; border: none;")
        log_scroll.setWidget(self._log_panel)
        log_inner.addWidget(log_scroll)
        self._log_shell.setVisible(False)

        h_splitter.addWidget(graph_shell)
        self._graph_shell = graph_shell

        # Right: Info panels in vertical splitter
        right_splitter = QSplitter(Qt.Vertical)
        right_splitter.setHandleWidth(5)
        right_splitter.setStyleSheet("QSplitter{background:transparent;} QSplitter::handle{background:rgba(255,255,255,20); border-radius:2px;}")
        right_splitter.setMinimumWidth(240)

        right_splitter.addWidget(self._wrap(InfoPanel(self.sim)))
        right_splitter.addWidget(self._wrap(ExecutionPanel(self.sim)))
        right_splitter.addWidget(self._wrap(BucketsPanel(self.sim)))
        right_splitter.setSizes([200, 200, 150])
        right_splitter.setStretchFactor(0, 2)
        right_splitter.setStretchFactor(1, 2)
        right_splitter.setStretchFactor(2, 1)

        h_splitter.addWidget(right_splitter)

        h_splitter.setSizes([700, 350])
        h_splitter.setStretchFactor(0, 3)
        h_splitter.setStretchFactor(1, 1)

        # ── Vertical splitter: middle area + bottom commit chain ──────────────
        v_splitter = QSplitter(Qt.Vertical)
        v_splitter.setHandleWidth(5)
        v_splitter.setStyleSheet("QSplitter{background:transparent;} QSplitter::handle{background:rgba(255,255,255,20); border-radius:2px;}")
        v_splitter.addWidget(h_splitter)

        self._commit_chain = CommitChainPanel(self.sim)
        self._commit_chain.setMinimumHeight(60)
        self._commit_chain.setSizePolicy(QSizePolicy.Expanding, QSizePolicy.Preferred)

        self._global_order = GlobalOrderPanel(self.sim)
        self._global_order.setMinimumHeight(60)
        self._global_order.setSizePolicy(QSizePolicy.Expanding, QSizePolicy.Preferred)

        # Bottom: commit chain + global order side by side
        bottom_splitter = QSplitter(Qt.Horizontal)
        bottom_splitter.setHandleWidth(5)
        bottom_splitter.setStyleSheet("QSplitter{background:transparent;} QSplitter::handle{background:rgba(255,255,255,20); border-radius:2px;}")
        bottom_splitter.addWidget(self._commit_chain)
        bottom_splitter.addWidget(self._global_order)
        bottom_splitter.setSizes([500, 400])

        v_splitter.addWidget(bottom_splitter)

        v_splitter.setSizes([600, 120])
        v_splitter.setStretchFactor(0, 5)
        v_splitter.setStretchFactor(1, 1)
        root.addWidget(v_splitter, 1)

        # Refresh timer
        self._tl_timer = QTimer(self)
        self._tl_timer.setInterval(80)
        self._tl_timer.timeout.connect(self._commit_chain.update)
        self._tl_timer.start()

    def _wrap(self, panel: QWidget) -> GlassPanel:
        shell = GlassPanel(radius=12, border_opacity=30)
        layout = QVBoxLayout(shell)
        layout.setContentsMargins(0, 0, 0, 0)
        layout.addWidget(panel)
        return shell

    def _toggle_config(self):
        visible = self._config_shell.isVisible()
        self._config_shell.setVisible(not visible)
        if not visible:
            self._position_config()

    def _toggle_log(self):
        visible = self._log_shell.isVisible()
        self._log_shell.setVisible(not visible)
        if not visible:
            self._position_log()

    def _position_config(self):
        h = self._canvas.height() - 20
        w = min(340, int(self._canvas.width() * 0.4))
        self._config_shell.setFixedSize(w, h)
        self._config_shell.move(10, 10)
        self._config_shell.raise_()

    def _position_log(self):
        h = self._canvas.height() - 20
        w = min(380, int(self._canvas.width() * 0.4))
        x = self._canvas.width() - w - 10
        self._log_shell.setFixedSize(w, h)
        self._log_shell.move(x, 10)
        self._log_shell.raise_()

    def resizeEvent(self, event: QResizeEvent):
        super().resizeEvent(event)
        if self._config_shell.isVisible():
            self._position_config()
        if self._log_shell.isVisible():
            self._position_log()

    def _on_config_applied(self):
        # Force canvas to recompute positions for new topology
        self._canvas._node_pos.clear()
        self._canvas._client_pos.clear()
        self._canvas._zoom = 1.0
        self._canvas._pan_offset = QPointF(0, 0)
        self._canvas._inspect_popup = None
        self._canvas._compute_positions()
        self._canvas.update()

    def _on_reset(self):
        # Reset canvas zoom/pan/positions
        self._canvas._node_pos.clear()
        self._canvas._client_pos.clear()
        self._canvas._zoom = 1.0
        self._canvas._pan_offset = QPointF(0, 0)
        self._canvas._inspect_popup = None
        self._canvas._compute_positions()
        self._canvas.update()


def run():
    app = QApplication(sys.argv)
    app.setStyleSheet(STYLESHEET)
    app.setEffectEnabled(Qt.UI_AnimateTooltip, False)
    window = MirBFTViewWindow()
    window.show()
    sys.exit(app.exec())


if __name__ == "__main__":
    run()
