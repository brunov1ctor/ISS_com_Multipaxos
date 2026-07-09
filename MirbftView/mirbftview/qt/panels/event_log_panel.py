"""EventLogPanel — log textual de eventos do protocolo."""

from PySide6.QtWidgets import QWidget, QVBoxLayout, QSizePolicy, QPlainTextEdit
from PySide6.QtCore import Qt, QTimer

from mirbftview.qt.theme import C
from mirbftview.qt.simulation import Simulation
from mirbftview.qt.panels._helpers import _title


class EventLogPanel(QWidget):
    """Painel de log textual — mostra eventos do protocolo em tempo real."""

    def __init__(self, sim: Simulation, parent=None):
        super().__init__(parent)
        self.sim = sim
        self.setAttribute(Qt.WA_TranslucentBackground)
        self.setSizePolicy(QSizePolicy.Expanding, QSizePolicy.Expanding)
        layout = QVBoxLayout(self)
        layout.setContentsMargins(10, 10, 10, 10)
        layout.setSpacing(4)

        layout.addWidget(_title("Log do Protocolo", C["accent"]))

        self._log_area = QPlainTextEdit()
        self._log_area.setReadOnly(True)
        self._log_area.setStyleSheet(
            f"color: {C['text']}; font-size: 8pt; font-family: Consolas; "
            f"border: none; background: transparent; selection-background-color: {C['primary']};"
        )
        self._log_area.setSizePolicy(QSizePolicy.Expanding, QSizePolicy.Expanding)
        layout.addWidget(self._log_area, 1)

        self._last_count = 0
        self._timer = QTimer(self)
        self._timer.setInterval(120)
        self._timer.timeout.connect(self._refresh)
        self._timer.start()

    def _refresh(self):
        events = self.sim.event_log
        if len(events) == self._last_count:
            return
        self._last_count = len(events)
        lines = [self._format_event(ev) for ev in reversed(events[-40:])]
        self._log_area.setPlainText("\n".join(lines))

    @staticmethod
    def _format_event(ev) -> str:
        parts = [f"[{ev.phase.name}] {ev.title}"]
        if ev.detail:
            parts.extend(f"  {dl}" for dl in ev.detail.split("\n")[:2])
        return "\n".join(parts)
