"""GlobalOrderPanel — Ordem Global Atômica (ADeliver).

Uma coluna por cross-op, na ordem do GSN; uma linha por grupo de dados, com um ponto só nos GSNs
que tocam aquele grupo (é a groupGSNQueue dele). O estado de cada ponto é o que o ADeliver
consulta: META conhecido, decidido mas retido (BufferCommit) ou entregue; a bandeira de cada linha
é o lastDeliveredGSN do grupo. Só cross-ops aparecem aqui: pedidos de um grupo só não passam pelo ADeliver.
"""

import math
from PySide6.QtWidgets import QWidget, QSizePolicy
from PySide6.QtCore import Qt, QRectF, QPointF, QTimer
from PySide6.QtGui import QPainter, QPainterPath, QColor, QPen, QBrush, QFont

from mirbftview.qt.theme import C
from mirbftview.qt.simulation import Simulation
from mirbftview.qt.canvas._constants import GROUP_COLORS

_MAX_COLS = 6
_LABEL_W = 62
_CROSS = QColor("#D500F9")
_AMBER = QColor("#FFB020")
_GOLD = QColor("#FFD84D")


class GlobalOrderPanel(QWidget):
    """Colunas por GSN, linhas por grupo."""

    def __init__(self, sim: Simulation, parent=None):
        super().__init__(parent)
        self.sim = sim
        self.setAttribute(Qt.WA_TranslucentBackground)
        self.setSizePolicy(QSizePolicy.Expanding, QSizePolicy.Expanding)
        self.setMinimumHeight(100)
        self._timer = QTimer(self)
        self._timer.setInterval(120)
        self._timer.timeout.connect(self.update)
        self._timer.start()

    # ─── Dados ────────────────────────────────────────────────────────────

    def _model(self):
        """Colunas (GSNs), estado de cada (GSN, grupo) e as bandeiras de lastDeliveredGSN."""
        s = self.sim
        dlv = s.delivery
        data_groups = [g.id for g in s.groups if g.id != 0]
        metas = {m["gsn"]: list(m["groups"]) for m in s.meta_stream}
        gsns = sorted(metas)[-_MAX_COLS:]
        last = dict(dlv._last_delivered_gsn) if dlv else {}
        pending = {g: {e.gsn: e for e in (dlv._pending.get(g, []) if dlv else [])} for g in data_groups}
        sn_of = {(h["gsn"], h["group"]): h["sn"] for h in (dlv.delivery_history if dlv else [])
                 if h.get("type") == "cross"}
        missing = set(dlv._meta_missing) if dlv else set()

        state = {}
        for gsn in gsns:
            for g in metas[gsn]:
                if g not in data_groups:
                    continue
                e = pending[g].get(gsn)
                if gsn <= last.get(g, 0):
                    st_, sn = "ok", sn_of.get((gsn, g))
                elif e is not None and e.committed:
                    st_, sn = ("wait_meta" if (gsn, g) in missing else "wait"), e.sn
                elif (gsn, g) in missing:
                    st_, sn = "nometa", None
                else:
                    st_, sn = "meta", None
                state[(gsn, g)] = (st_, sn)
        return data_groups, gsns, metas, state, last, pending

    # ─── Desenho ──────────────────────────────────────────────────────────

    def paintEvent(self, event):
        p = QPainter(self)
        p.setRenderHint(QPainter.Antialiasing)
        w, h = self.width(), self.height()

        bg = QPainterPath()
        bg.addRoundedRect(QRectF(0, 0, w, h), 10, 10)
        p.fillPath(bg, QColor(28, 46, 74, 100))
        p.setPen(QPen(QColor(255, 255, 255, 25), 1))
        p.drawPath(bg)

        p.setPen(QColor("#A855F7"))
        p.setFont(QFont("Segoe UI", 9, QFont.Bold))
        p.drawText(QRectF(10, 4, 320, 16), Qt.AlignLeft, "Ordem Global Atômica (ADeliver)")
        p.setPen(QColor(C["text3"]))
        p.setFont(QFont("Segoe UI", 6))
        p.drawText(QRectF(10, 19, w - 20, 11), Qt.AlignLeft,
                   "coluna = GSN (ordem global)  ·  linha = fila de GSNs do grupo (groupGSNQueue)")

        data_groups, gsns, metas, state, last, pending = self._model()
        if not gsns:
            p.setPen(QColor(C["text3"]))
            p.setFont(QFont("Segoe UI", 8))
            p.drawText(QRectF(10, 32, w - 20, h - 36), Qt.AlignCenter,
                       "Nenhuma cross-op ainda.\nSó pedidos que tocam mais de um grupo passam pelo ADeliver.")
            p.end()
            return

        top = 50
        legend_h = 18
        rows = 1 + len(data_groups)                      # SEQ + grupos de dados
        row_h = max(18.0, min(34.0, (h - top - legend_h - 10) / rows))
        r = min(9.0, row_h * 0.33)
        ys = {"SEQ": top + row_h * 0.5}
        for i, g in enumerate(data_groups):
            ys[g] = top + row_h * (i + 1.5)
        col_w = max(54.0, min(118.0, (w - _LABEL_W - 16) / max(len(gsns), 1)))
        x_of = {gsn: _LABEL_W + col_w * (i + 0.5) for i, gsn in enumerate(gsns)}

        # linhas de fundo e rótulos
        seq_c = QColor(GROUP_COLORS[0])
        for key, y in ys.items():
            gc = seq_c if key == "SEQ" else QColor(GROUP_COLORS[key % len(GROUP_COLORS)])
            lane = QPainterPath()
            lane.addRoundedRect(QRectF(_LABEL_W - 4, y - row_h / 2 + 2, w - _LABEL_W - 6, row_h - 4), 4, 4)
            fill = QColor(gc); fill.setAlpha(14)
            p.fillPath(lane, fill)
            edge = QColor(gc); edge.setAlpha(45)
            p.setPen(QPen(edge, 0.6))
            p.setBrush(Qt.NoBrush)
            p.drawPath(lane)
            p.setPen(gc)
            p.setFont(QFont("Segoe UI", 8, QFont.Bold))
            p.drawText(QRectF(4, y - 8, _LABEL_W - 10, 16), Qt.AlignVCenter | Qt.AlignRight,
                       "SEQ" if key == "SEQ" else f"G{key}")

        # colunas: GSN, losango do sequenciador e linha ligando os grupos tocados
        for gsn in gsns:
            x = x_of[gsn]
            touched = [g for g in metas[gsn] if g in ys]
            p.setPen(QColor(C["text"]))
            p.setFont(QFont("Segoe UI", 7, QFont.Bold))
            p.drawText(QRectF(x - 30, top - 13, 60, 12), Qt.AlignCenter, f"GSN {gsn}")
            y0, y1 = ys["SEQ"], max([ys[g] for g in touched] + [ys["SEQ"]])
            line_c = QColor(_CROSS); line_c.setAlpha(95)
            p.setPen(QPen(line_c, 1.6))
            p.drawLine(QPointF(x, y0), QPointF(x, y1))
            d = r * 0.85
            diamond = QPainterPath()
            diamond.moveTo(x, y0 - d); diamond.lineTo(x + d, y0); diamond.lineTo(x, y0 + d); diamond.lineTo(x - d, y0)
            diamond.closeSubpath()
            p.setPen(QPen(QColor(255, 255, 255, 200), 0.8))
            p.setBrush(_CROSS)
            p.drawPath(diamond)

        # pontos (um por GSN e grupo tocado)
        t = self.sim.tick_count
        beat = 0.5 + 0.5 * math.sin(t * 0.2)
        for (gsn, g), (st_, sn) in state.items():
            if g not in ys or gsn not in x_of:
                continue
            x, y = x_of[gsn], ys[g]
            self._draw_point(p, x, y, r, st_, sn, beat)

        # setas de bloqueio
        for (gsn, g), (st_, sn) in state.items():
            if st_ not in ("wait", "wait_meta") or g not in ys or gsn not in x_of:
                continue
            x, y = x_of[gsn], ys[g]
            if st_ == "wait_meta":
                label = f"falta o META do GSN {gsn}"
                pred = None
            else:
                prev = [e.gsn for e in pending.get(g, {}).values() if e.gsn < gsn]
                pred = min(prev) if prev else None
                label = f"espera o GSN {pred}" if pred else "retida"
            amber = QColor(_AMBER)
            if pred is not None and pred in x_of:
                x2 = x_of[pred]
                ay = y + r + 3
                path = QPainterPath(QPointF(x - r, ay))
                path.quadTo(QPointF((x + x2) / 2, ay + 8), QPointF(x2 + r, ay))
                p.setPen(QPen(amber, 1.6))
                p.setBrush(Qt.NoBrush)
                p.drawPath(path)
                head = QPainterPath()
                head.moveTo(x2 + r, ay); head.lineTo(x2 + r + 6, ay - 3); head.lineTo(x2 + r + 6, ay + 3)
                head.closeSubpath()
                p.fillPath(head, amber)
            p.setPen(amber)
            p.setFont(QFont("Segoe UI", 6, QFont.Bold))
            p.drawText(QRectF(x - col_w / 2, y + r + 3, col_w, 10), Qt.AlignCenter, label)

        # bandeira lastDeliveredGSN de cada grupo
        for g in data_groups:
            y = ys[g]
            gsn = last.get(g, 0)
            if gsn and gsn in x_of:
                fx = x_of[gsn] + col_w / 2 - 2
            elif gsn and gsn < gsns[0]:
                fx = _LABEL_W + 2
            elif gsn and gsn > gsns[-1]:
                fx = x_of[gsns[-1]] + col_w / 2 - 2
            else:
                fx = _LABEL_W + 2
            p.setPen(QPen(_GOLD, 2.2, Qt.SolidLine, Qt.RoundCap))
            p.drawLine(QPointF(fx, y - row_h * 0.38), QPointF(fx, y + row_h * 0.38))
            p.setFont(QFont("Segoe UI", 5, QFont.Bold))
            p.drawText(QRectF(fx + 3, y - row_h * 0.46, 70, 9), Qt.AlignLeft | Qt.AlignVCenter,
                       f"lastDelivered={gsn}")

        self._draw_legend(p, w, h)
        p.end()

    def _draw_point(self, p, x, y, r, st_, sn, beat):
        if st_ == "ok":
            p.setPen(QPen(QColor(255, 255, 255, 210), 1.0))
            p.setBrush(QColor(C["green"]))
            p.drawEllipse(QPointF(x, y), r, r)
            p.setPen(QPen(QColor(4, 34, 26), max(1.6, r * 0.25), Qt.SolidLine, Qt.RoundCap, Qt.RoundJoin))
            k = r * 0.45
            tick = QPainterPath(QPointF(x - k, y))
            tick.lineTo(x - k * 0.25, y + k * 0.7)
            tick.lineTo(x + k, y - k * 0.7)
            p.setBrush(Qt.NoBrush)
            p.drawPath(tick)
            if sn is not None:
                self._sn_label(p, x + r + 3, y, f"SN {sn}")
        elif st_ in ("wait", "wait_meta"):
            halo = QColor(_AMBER); halo.setAlpha(int(40 + 60 * beat))
            p.setPen(Qt.NoPen)
            p.setBrush(halo)
            p.drawEllipse(QPointF(x, y), r + 3 + 2 * beat, r + 3 + 2 * beat)
            p.setPen(QPen(QColor(255, 255, 255, 230), 1.4))
            p.setBrush(_AMBER)
            p.drawEllipse(QPointF(x, y), r, r)
            p.setPen(Qt.NoPen)
            p.setBrush(QColor(58, 37, 0))
            bw, bh = r * 0.28, r * 0.9
            p.drawRoundedRect(QRectF(x - r * 0.42, y - bh / 2, bw, bh), 1, 1)
            p.drawRoundedRect(QRectF(x + r * 0.14, y - bh / 2, bw, bh), 1, 1)
            if sn is not None:
                self._sn_label(p, x + r + 3, y, f"SN {sn}")
        elif st_ == "nometa":
            pen = QPen(_AMBER, 1.4)
            pen.setStyle(Qt.DotLine)
            p.setPen(pen)
            p.setBrush(Qt.NoBrush)
            p.drawEllipse(QPointF(x, y), r, r)
            p.setPen(QColor(_AMBER))
            p.setFont(QFont("Segoe UI", 6))
            p.drawText(QRectF(x + r + 3, y - 6, 60, 12), Qt.AlignLeft | Qt.AlignVCenter, "sem META")
        else:  # meta: o GSN é conhecido; a cópia ainda não foi decidida neste grupo
            pen = QPen(QColor(C["text3"]), 1.4)
            pen.setStyle(Qt.DashLine)
            p.setPen(pen)
            p.setBrush(Qt.NoBrush)
            p.drawEllipse(QPointF(x, y), r, r)

    @staticmethod
    def _sn_label(p, x, y, text):
        p.setPen(QColor(C["text"]))
        p.setFont(QFont("Segoe UI", 6))
        p.drawText(QRectF(x, y - 6, 44, 12), Qt.AlignLeft | Qt.AlignVCenter, text)

    def _draw_legend(self, p, w, h):
        y = h - 10
        x = 12
        items = [("meta", "só o META"), ("wait", "decidida, retida"), ("nometa", "sem META"), ("ok", "entregue")]
        p.setFont(QFont("Segoe UI", 6))
        for kind, text in items:
            self._draw_point(p, x + 4, y, 4.0, kind, None, 0.5)
            p.setPen(QColor(C["text3"]))
            p.drawText(QRectF(x + 11, y - 6, 80, 12), Qt.AlignLeft | Qt.AlignVCenter, text)
            x += 11 + p.fontMetrics().horizontalAdvance(text) + 14
        p.setPen(QPen(_GOLD, 2.0, Qt.SolidLine, Qt.RoundCap))
        p.drawLine(QPointF(x + 2, y - 4), QPointF(x + 2, y + 4))
        p.setPen(QColor(C["text3"]))
        p.drawText(QRectF(x + 8, y - 6, 120, 12), Qt.AlignLeft | Qt.AlignVCenter, "lastDeliveredGSN")
