"""GroupStatePanel — GroupState.data: a tabela chave->valor de um grupo (ver
orderer/statemachine.go), com um dropdown pra escolher qual grupo ver. Substitui o antigo
GlobalOrderPanel (ADeliver) neste slot -- a pedido: a API de cliente (put/get/transfer) é agora
o foco principal, então o que fica visível por padrão é o dado de verdade que ela grava, não só
o mecanismo de entrega cross-group (esse continua existindo em protocol/delivery.py e dá pra
voltar a mostrar GlobalOrderPanel se precisar).
"""

from PySide6.QtWidgets import QWidget, QSizePolicy, QComboBox
from PySide6.QtCore import Qt, QRectF, QTimer
from PySide6.QtGui import QPainter, QPainterPath, QColor, QPen, QFont, QFontMetrics

from mirbftview.qt.theme import C
from mirbftview.qt.canvas._constants import GROUP_COLORS
from mirbftview.qt.simulation import Simulation

_TITLE = "\U0001f4e6 GroupState.data"


class GroupStatePanel(QWidget):
    """Uma linha por chave, no grupo escolhido -- mesma estrutura do mapa real em
    orderer/statemachine.go:GroupState.data (map[string][]byte), um mapa POR GRUPO."""

    def __init__(self, sim: Simulation, parent=None):
        super().__init__(parent)
        self.sim = sim
        self.setAttribute(Qt.WA_TranslucentBackground)
        self.setSizePolicy(QSizePolicy.Expanding, QSizePolicy.Expanding)
        self.setMinimumHeight(100)
        self._group_id = 1
        self._group_ids: list[int] = []

        self._combo = QComboBox(self)
        self._combo.setFixedHeight(20)
        self._combo.setStyleSheet(
            "QComboBox { background: rgba(10,16,30,0.75); color: #E6EDF7; font-size: 8pt; "
            "font-weight: bold; padding: 1px 6px; border: 1px solid rgba(255,255,255,0.25); "
            "border-radius: 5px; }"
            "QComboBox QAbstractItemView { background: rgb(14,22,40); color: #E6EDF7; "
            "selection-background-color: rgba(108,99,255,0.55); }"
        )
        self._combo.currentIndexChanged.connect(self._on_group_changed)

        self._timer = QTimer(self)
        self._timer.setInterval(200)
        self._timer.timeout.connect(self._on_timer)
        self._timer.start()
        self._sync_groups()

    def _data_group_ids(self) -> list[int]:
        return [g.id for g in self.sim.groups if g.id != 0]

    def _sync_groups(self):
        ids = self._data_group_ids()
        if ids == self._group_ids:
            return
        self._group_ids = ids
        self._combo.blockSignals(True)
        self._combo.clear()
        for gid in ids:
            self._combo.addItem(f"Grupo {gid}", gid)
        if self._group_id not in ids:
            self._group_id = ids[0] if ids else 1
        self._combo.setCurrentIndex(ids.index(self._group_id) if self._group_id in ids else -1)
        self._combo.blockSignals(False)
        self._layout_combo()

    def _on_group_changed(self, idx):
        gid = self._combo.itemData(idx)
        if gid is not None:
            self._group_id = gid
            self.update()

    def _layout_combo(self):
        tw = QFontMetrics(QFont("Segoe UI", 9, QFont.Bold)).horizontalAdvance(_TITLE)
        self._combo.setGeometry(10 + tw + 8, 5, 90, 20)

    def resizeEvent(self, event):
        super().resizeEvent(event)
        self._layout_combo()

    def _on_timer(self):
        self._sync_groups()
        self.update()

    def paintEvent(self, event):
        p = QPainter(self)
        p.setRenderHint(QPainter.Antialiasing)
        w, h = self.width(), self.height()

        bg = QPainterPath()
        bg.addRoundedRect(QRectF(0, 0, w, h), 10, 10)
        p.fillPath(bg, QColor(28, 46, 74, 100))
        p.setPen(QPen(QColor(255, 255, 255, 25), 1))
        p.drawPath(bg)

        gc = QColor(GROUP_COLORS[self._group_id % len(GROUP_COLORS)])
        p.setPen(gc)
        p.setFont(QFont("Segoe UI", 9, QFont.Bold))
        p.drawText(QRectF(10, 4, 220, 16), Qt.AlignLeft, _TITLE)
        p.setPen(QColor(C["text3"]))
        p.setFont(QFont("Segoe UI", 6))
        p.drawText(QRectF(10, 28, w - 20, 11), Qt.AlignLeft,
                   "map[string][]byte, um por grupo -- gravado/lido por put/get/transfer (APIPUT/APITRANSFER), nunca pelo benchmark GET/TX")

        data = self.sim.group_kv.get(self._group_id, {})
        top = 46
        if not data:
            p.setPen(QColor(C["text3"]))
            p.setFont(QFont("Segoe UI", 8))
            p.drawText(QRectF(10, top, w - 20, h - top - 8), Qt.AlignLeft | Qt.AlignTop,
                       f"(vazio)\n\nSN comitada != dado gravado -- GET/TX do gerador automático\n"
                       f"nunca escrevem aqui (mesma regra do Go: só payload APIPUT/APITRANSFER).\n"
                       f"Duplo-clique na bolinha de um cliente e digite put/transfer\n"
                       f"pra gravar algo de verdade no G{self._group_id}.")
            p.end()
            return

        col1_w = min(max(80, w * 0.42), w - 40)
        row_h = 22
        p.setPen(QColor(C["text2"]))
        p.setFont(QFont("Segoe UI", 7, QFont.Bold))
        p.drawText(QRectF(10, top, col1_w, 16), Qt.AlignLeft, "chave")
        p.drawText(QRectF(10 + col1_w, top, w - 20 - col1_w, 16), Qt.AlignLeft, "valor")
        p.setPen(QPen(gc, 1))
        p.drawLine(10, top + 18, w - 10, top + 18)

        y = top + 22
        fm_key = QFontMetrics(QFont("Consolas", 8, QFont.Bold))
        fm_val = QFontMetrics(QFont("Consolas", 8))
        keys_sorted = sorted(data.keys())
        for idx, key in enumerate(keys_sorted):
            if y + row_h > h - 6:
                p.setPen(QColor(C["text3"]))
                p.setFont(QFont("Segoe UI", 7))
                p.drawText(QRectF(10, y, w - 20, row_h), Qt.AlignLeft | Qt.AlignVCenter,
                           f"... +{len(keys_sorted) - idx} mais")
                break
            value = data[key]
            p.setPen(QColor(gc))
            p.setFont(QFont("Consolas", 8, QFont.Bold))
            kelided = fm_key.elidedText(key, Qt.ElideRight, int(col1_w - 10))
            p.drawText(QRectF(10, y, col1_w - 10, row_h), Qt.AlignLeft | Qt.AlignVCenter, kelided)
            p.setPen(QColor(C["text"]))
            p.setFont(QFont("Consolas", 8))
            velided = fm_val.elidedText(repr(value), Qt.ElideRight, int(w - 20 - col1_w))
            p.drawText(QRectF(10 + col1_w, y, w - 20 - col1_w, row_h), Qt.AlignLeft | Qt.AlignVCenter, velided)
            y += row_h

        p.end()
