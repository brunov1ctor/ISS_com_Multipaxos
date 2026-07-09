"""EventLogPanel — log compacto de eventos do protocolo."""

from PySide6.QtWidgets import QWidget, QVBoxLayout, QSizePolicy, QTextEdit
from PySide6.QtCore import Qt, QTimer

from mirbftview.qt.theme import C
from mirbftview.qt.simulation import Simulation, Phase
from mirbftview.qt.panels._helpers import _title


_PHASE_SHORT = {
    Phase.PREPARE:        "PREP",
    Phase.PROMISE:        "PROM",
    Phase.CLIENT_SEND:    "REQ",
    Phase.BUCKET_ASSIGN:  "BKT",
    Phase.BATCH_CUT:      "CUT",
    Phase.GSN_ASSIGN:     "GSN",
    Phase.ACCEPT:         "ACC",
    Phase.ACCEPTED:       "ACKD",
    Phase.COMMIT:         "CMT",
    Phase.COMMIT_NOTIFY:  "NTFY",
    Phase.ADELIVER:       "ADLV",
    Phase.CHECKPOINT:     "CKPT",
    Phase.EPOCH_TRANSITION: "EPCH",
    Phase.VIEW_CHANGE:    "VIEW",
    Phase.RETRANSMIT:     "RTXM",
    Phase.DONE:           "DONE",
}


class EventLogPanel(QWidget):
    """Painel de log compacto — uma linha por evento com cor e info inline."""

    def __init__(self, sim: Simulation, parent=None):
        super().__init__(parent)
        self.sim = sim
        self.setAttribute(Qt.WA_TranslucentBackground)
        self.setSizePolicy(QSizePolicy.Expanding, QSizePolicy.Expanding)
        layout = QVBoxLayout(self)
        layout.setContentsMargins(10, 10, 10, 10)
        layout.setSpacing(4)

        layout.addWidget(_title("Log do Protocolo", C["accent"]))

        self._log_area = QTextEdit()
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
        html_parts = [self._format_line(i, ev) for i, ev in enumerate(reversed(events))]
        self._log_area.setHtml(
            f'<table cellspacing="0" cellpadding="1" style="font-size:8pt; font-family:Consolas;">'
            + "".join(html_parts)
            + '</table>'
        )

    @staticmethod
    def _format_line(idx, ev) -> str:
        color = ev.req_color if ev.req_color else C["text2"]
        phase_short = _PHASE_SHORT.get(ev.phase, ev.phase.name[:4])

        # Extrai info compacta do detalhe
        info = EventLogPanel._extract_info(ev)

        # Linha de fundo alternada sutil
        bg = "rgba(255,255,255,0.03)" if idx % 2 == 0 else "transparent"

        return (
            f'<tr style="background:{bg};">'
            f'<td style="color:{color}; font-weight:bold; padding-right:6px; white-space:nowrap;">{phase_short}</td>'
            f'<td style="color:{C["text"]}; padding-right:6px; white-space:nowrap;">{info}</td>'
            f'</tr>'
        )

    @staticmethod
    def _extract_info(ev) -> str:
        """Extrai uma descrição compacta inline do evento."""
        detail = ev.detail or ""
        phase = ev.phase

        if phase == Phase.PREPARE:
            # "Lider Node X | SN=Y | Ballot=Z\nGrupo GN membros=[...]"
            parts = detail.split("\n")
            leader = ""
            group = ""
            for p in parts:
                if "Lider Node" in p:
                    leader = p.split("|")[0].strip()
                if "Grupo G" in p:
                    g = p.split("membros")[0].strip()
                    group = g.replace("Grupo ", "")
            return f"{group} {leader}"

        elif phase == Phase.PROMISE:
            # "Promises: 3/2 QUORUM"
            if "QUORUM" in detail:
                return detail.split("\n")[0].strip()
            return "quorum"

        elif phase == Phase.CLIENT_SEND:
            # "Cliente X -> Node Y (proxy)\nPayload..."
            parts = detail.split("\n")
            line1 = parts[0].strip() if parts else ""
            group_line = ""
            for p in parts:
                if "Grupo:" in p:
                    group_line = p.split("|")[0].strip().replace("Grupo: ", "")
            return f"{group_line} {line1}" if group_line else line1

        elif phase == Phase.BUCKET_ASSIGN:
            # "Request -> Bucket X"
            parts = detail.split("\n")
            return parts[0].strip() if parts else ""

        elif phase == Phase.ACCEPT:
            # "Lider propoe batch digest=...\nSN=X | Ballot=Y | GZ"
            parts = detail.split("\n")
            sn_line = ""
            for p in parts:
                if "SN=" in p and "| G" in p:
                    sn_line = p.strip()
            return sn_line if sn_line else parts[0].strip()[:40]

        elif phase == Phase.ACCEPTED:
            # "Accepted: 3/2 QUORUM\nSN=X confirmado"
            parts = detail.split("\n")
            sn = ""
            quorum = ""
            for p in parts:
                if "QUORUM" in p:
                    quorum = p.strip()
                if "confirmado" in p:
                    sn = p.strip()
            return f"{quorum} {sn}".strip()

        elif phase == Phase.COMMIT:
            # "SN=X committed em GY\nTotal: Z"
            parts = detail.split("\n")
            return parts[0].strip() if parts else ""

        elif phase == Phase.COMMIT_NOTIFY:
            # "Proxy Node X responde a Cliente Y"
            parts = detail.split("\n")
            return parts[0].strip() if parts else ""

        elif phase == Phase.ADELIVER:
            parts = detail.split("\n")
            return parts[0].strip()[:45] if parts else ""

        elif phase == Phase.GSN_ASSIGN:
            parts = detail.split("\n")
            return parts[0].strip() if parts else ""

        elif phase == Phase.DONE:
            parts = detail.split("\n")
            return parts[0].strip() if parts else ""

        elif phase == Phase.CHECKPOINT:
            parts = detail.split("\n")
            return parts[0].strip() if parts else ""

        else:
            parts = detail.split("\n")
            return parts[0].strip()[:40] if parts else ev.title[:30]
