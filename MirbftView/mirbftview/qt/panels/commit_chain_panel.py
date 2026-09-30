"""CommitChainPanel — o log replicado como GRADE: posição SN = linha * nGrupos + coluna.

Igual ao "Partitioning the log" do ISS: cada coluna é um grupo (G0, G1, ...), e o SN de um
grupo g é g, g+nGrupos, g+2*nGrupos... (entrelaçado). Uma posição vazia é uma SN ainda não
decidida (ou de outro grupo que ainda não chegou lá); a coluna de G0 fica vazia porque o
Sequenciador não roda instâncias ISS.
"""

import math

from PySide6.QtWidgets import QWidget, QSizePolicy
from PySide6.QtCore import Qt, QRectF, QPointF, QTimer
from PySide6.QtGui import QPainter, QPainterPath, QColor, QLinearGradient, QPen, QBrush, QFont, QFontMetrics

from mirbftview.qt.theme import C
from mirbftview.qt.simulation import Simulation, MsgType
from mirbftview.qt.canvas._constants import GROUP_COLORS

_TOP = 44          # abaixo do título, da legenda e dos cabeçalhos de coluna
_ROW_H = 62
_ROW_GAP = 8
_GAP = 6


class CommitChainPanel(QWidget):
    """Log replicado em grade (uma coluna por grupo)."""

    def __init__(self, sim: Simulation, parent=None):
        super().__init__(parent)
        self.sim = sim
        self.setAttribute(Qt.WA_TranslucentBackground)
        self.setSizePolicy(QSizePolicy.Expanding, QSizePolicy.Expanding)
        self.setMinimumHeight(110)
        self.setMouseTracking(True)
        self._scroll_y = 0.0          # deslocamento vertical (px, <= 0)
        self._dragging = False
        self._drag_start_y = 0.0
        self._drag_start_offset = 0.0
        self._hovered_sn = -1
        self._clicked_sn = -1
        self._new_block_ttl = 0       # animação de entrada
        self._last_count = 0
        self._cells: dict[int, QRectF] = {}
        # G0 (Sequenciador) trabalhando: sem entrada no log, então a coluna dele anima enquanto ele atua
        self._g0_text = ""          # o que o sequenciador está fazendo agora
        self._g0_until = 0           # tick até o qual a animação continua (fica um instante após a mensagem chegar)
        self._last_gsn_seen = 0
        self._g0_prev: dict = {}       # variável -> último valor desenhado
        self._g0_changed: dict = {}    # variável -> tick da última mudança (destaque)
        self._timer = QTimer(self)
        self._timer.setInterval(60)
        self._timer.timeout.connect(self._on_tick)
        self._timer.start()

    # ─── Modelo ───────────────────────────────────────────────────────────

    def _cols(self) -> int:
        return max(self.sim.epoch_mgr.sn_stride, 1)

    def _by_sn(self) -> dict:
        return {e["sn"]: e for e in self.sim.commit_history}

    def _rows(self, by_sn) -> int:
        cols = self._cols()
        top = max(by_sn) if by_sn else -1
        for hist in self.sim.sn_history.values():
            if hist:
                top = max(top, hist[-1]["sn"])
        return max(top // cols + 1, 1)

    def _row_pitch(self):
        return _ROW_H + _ROW_GAP

    def _view_h(self):
        return max(self.height() - _TOP - 6, _ROW_H)

    def _clamp_scroll(self, rows):
        content = rows * self._row_pitch()
        self._scroll_y = max(min(0.0, self._view_h() - content), min(0.0, self._scroll_y))

    def _scroll_to_sn(self, sn, rows):
        """Mantém visível a linha do SN novo."""
        row = sn // self._cols()
        top = row * self._row_pitch()
        bottom = top + _ROW_H
        if top + self._scroll_y < 0:
            self._scroll_y = -top
        elif bottom + self._scroll_y > self._view_h():
            self._scroll_y = self._view_h() - bottom
        self._clamp_scroll(rows)

    def _update_g0(self):
        """Detecta o Sequenciador agindo: GSN_REQUEST/GSN_RESPONSE em voo (atribuindo o GSN) ou
        META_STREAM em voo (espalhando o META). Ao terminar, a animação ainda dura ~1,2 s."""
        sim = self.sim
        now = sim.tick_count
        metas = sim.meta_stream
        last = metas[-1] if metas else None
        if last is not None and last["gsn"] != self._last_gsn_seen:
            self._last_gsn_seen = last["gsn"]
            self._g0_until = max(self._g0_until, now + 75)
            self._g0_text = f"GSN {last['gsn']} \u2192 " + ",".join(f"G{g}" for g in last["groups"])
        elif last is None:
            self._last_gsn_seen = 0
        for m in sim.messages:
            if m.progress >= 1.0:
                continue
            if m.msg_type in (MsgType.GSN_REQUEST, MsgType.GSN_RESPONSE):
                self._g0_until = max(self._g0_until, now + 40)
                self._g0_text = "atribuindo GSN"
            elif m.msg_type == MsgType.META_STREAM:
                self._g0_until = max(self._g0_until, now + 40)
                if last is not None:
                    self._g0_text = f"META gsn={last['gsn']}"

    def _g0_vars(self):
        """Variáveis do Sequencer (sequencer.go) na ordem de exibição: (chave, texto)."""
        sim = self.sim
        members = sim.groups[0].members if sim.groups and sim.groups[0].members else []
        leader = min(members) if members else 0
        metas = list(sim.meta_stream)
        out = [("nextGSN", str(sim.gsn + 1)),
               ("leader", f"N{leader}  term 0"),
               ("metadata", f"{len(metas)} GSN(s)" if metas else "vazio")]
        for m in reversed(metas[-3:]):
            out.append((f"meta{m['gsn']}", f"{m['gsn']} \u2192 " + ",".join(f"G{g}" for g in m["groups"])))
        dlv = sim.delivery
        if dlv is not None:
            data = [g.id for g in sim.groups if g.id != 0]
            last = dlv._last_delivered_gsn
            for i in range(0, len(data), 2):
                pair = data[i:i + 2]
                out.append((f"lastDel{i}", "   ".join(f"G{g}={last.get(g, 0)}" for g in pair)))
        return out

    def _draw_g0_vars(self, p, rect):
        """Painel do Sequencer na coluna do G0: as variáveis que ele usa e atualiza. O que acabou de
        mudar acende em dourado; enquanto ele atua, o painel pulsa. Não é entrada de log."""
        t = self.sim.tick_count
        gcol = QColor(GROUP_COLORS[0])
        active = self._g0_active()
        beat = 0.5 + 0.5 * math.sin(t * 0.22)

        path = QPainterPath()
        path.addRoundedRect(rect, 6, 6)
        fill = QColor(gcol)
        fill.setAlpha(int(30 + (45 * beat if active else 0)))
        p.fillPath(path, fill)
        if active:
            for k in range(2):
                ph = ((t * 0.03) + k * 0.5) % 1.0
                ring = QColor(gcol)
                ring.setAlpha(int(190 * (1 - ph)))
                p.setPen(QPen(ring, 2.0))
                p.setBrush(Qt.NoBrush)
                grow = 1 + 6 * ph
                rp = QPainterPath()
                rp.addRoundedRect(rect.adjusted(-grow, -grow, grow, grow), 8, 8)
                p.drawPath(rp)
        border = QColor(gcol)
        border.setAlpha(int(170 + 70 * beat) if active else 120)
        pen = QPen(border, 1.5)
        pen.setStyle(Qt.SolidLine if active else Qt.DashLine)
        p.setPen(pen)
        p.setBrush(Qt.NoBrush)
        p.drawPath(path)

        p.save()
        p.setClipRect(rect.adjusted(2, 2, -2, -2))
        x, y = rect.x() + 7, rect.y() + 4
        p.setPen(gcol)
        p.setFont(QFont("Segoe UI", 7, QFont.Bold))
        title = "\u2699 Sequencer" + (f" \u00b7 {self._g0_text}" if active and self._g0_text else "")
        p.drawText(QRectF(x, y, rect.width() - 10, 11), Qt.AlignLeft | Qt.AlignVCenter, title)
        y += 13

        # Detecta mudanças (para acender o que mudou)
        vars_ = self._g0_vars()
        for key, val in vars_:
            if self._g0_prev.get(key) != val:
                if key in self._g0_prev or key == "nextGSN":
                    self._g0_changed[key] = t
                self._g0_prev[key] = val
        line_h = 10
        fm_font = QFont("Consolas", 7)
        p.setFont(fm_font)
        for key, val in vars_:
            if y + line_h > rect.bottom() - 2:
                break
            changed = t - self._g0_changed.get(key, -999) < 70
            if changed:
                glow = QColor(C["gold"])
                glow.setAlpha(int(70 * (1 - (t - self._g0_changed[key]) / 70)) + 25)
                p.setPen(Qt.NoPen)
                p.setBrush(glow)
                p.drawRoundedRect(QRectF(rect.x() + 3, y - 1, rect.width() - 6, line_h + 1), 3, 3)
            name = key if not key.startswith(("meta", "lastDel")) or key == "metadata" else ""
            if key.startswith("meta") and key != "metadata":
                name = "  "
            if key.startswith("lastDel"):
                name = "lastDel" if key == "lastDel0" else ""
            if name:
                p.setPen(QColor(C["text3"]))
                p.drawText(QRectF(x, y, 56, line_h), Qt.AlignLeft | Qt.AlignVCenter, name)
            p.setPen(QColor(C["gold"]) if changed else QColor(C["text"]))
            p.drawText(QRectF(x + (56 if name.strip() else 12), y, rect.width() - 70, line_h),
                       Qt.AlignLeft | Qt.AlignVCenter, val)
            y += line_h
        p.restore()

    def _g0_active(self) -> bool:
        return self.sim.tick_count < self._g0_until

    def _on_tick(self):
        self._update_g0()
        if self._new_block_ttl > 0:
            self._new_block_ttl -= 1
        history = self.sim.commit_history
        if len(history) > self._last_count:
            self._new_block_ttl = 30
            self._last_count = len(history)
            self._scroll_to_sn(history[-1]["sn"], self._rows(self._by_sn()))
        elif len(history) < self._last_count:
            self._last_count = len(history)     # reset
            self._scroll_y = 0.0
        self.update()

    # ─── Mouse ────────────────────────────────────────────────────────────

    def wheelEvent(self, event):
        delta = event.angleDelta().y() or event.angleDelta().x()
        self._scroll_y += delta * 0.5
        self._clamp_scroll(self._rows(self._by_sn()))
        self.update()
        event.accept()

    def _hit_test(self, pos) -> int:
        for sn, rect in self._cells.items():
            if rect.contains(pos):
                return sn
        return -1

    def mousePressEvent(self, event):
        if event.button() == Qt.LeftButton:
            sn = self._hit_test(event.position())
            if sn in self._by_sn():
                self._clicked_sn = sn if self._clicked_sn != sn else -1
                self.update()
            else:
                self._dragging = True
                self._drag_start_y = event.position().y()
                self._drag_start_offset = self._scroll_y
                self.setCursor(Qt.ClosedHandCursor)
        super().mousePressEvent(event)

    def mouseMoveEvent(self, event):
        if self._dragging:
            self._scroll_y = self._drag_start_offset + (event.position().y() - self._drag_start_y)
            self._clamp_scroll(self._rows(self._by_sn()))
            self.update()
        else:
            sn = self._hit_test(event.position())
            if sn != self._hovered_sn:
                self._hovered_sn = sn
                self.setCursor(Qt.PointingHandCursor if sn in self._by_sn() else Qt.ArrowCursor)
                self.update()
        super().mouseMoveEvent(event)

    def mouseReleaseEvent(self, event):
        if event.button() == Qt.LeftButton:
            self._dragging = False
            self.setCursor(Qt.ArrowCursor)
        super().mouseReleaseEvent(event)

    # ─── Paint ────────────────────────────────────────────────────────────

    def paintEvent(self, event):
        p = QPainter(self)
        p.setRenderHint(QPainter.Antialiasing)
        w, h = self.width(), self.height()

        bg = QPainterPath()
        bg.addRoundedRect(QRectF(0, 0, w, h), 10, 10)
        p.fillPath(bg, QColor(28, 46, 74, 100))
        p.setPen(QPen(QColor(255, 255, 255, 25), 1))
        p.drawPath(bg)

        p.setPen(QColor(C["green"]))
        p.setFont(QFont("Segoe UI", 9, QFont.Bold))
        p.drawText(QRectF(10, 4, 350, 16), Qt.AlignLeft, "⛓ Cadeia de Commits (Log Replicado)")
        p.setPen(QColor(C["text3"]))
        p.setFont(QFont("Segoe UI", 6))
        p.drawText(QRectF(10, 18, w - 20, 12), Qt.AlignLeft,
                   "posição = SN (linha × nº de grupos + coluna) | coluna = grupo | vazio = SN ainda não decidida")

        history = self.sim.commit_history
        by_sn = self._by_sn()
        cols = self._cols()
        rows = self._rows(by_sn)
        self._clamp_scroll(rows)

        p.setPen(QColor(C["text2"]))
        p.setFont(QFont("Segoe UI", 7))
        ckpt = history[-1]["checkpoint_idx"] if history else 0
        p.drawText(QRectF(w - 110, 4, 100, 16), Qt.AlignRight, f"Total: {len(history)} | C{ckpt}")

        margin = 10
        cw = max(40.0, (w - 2 * margin - _GAP * (cols - 1)) / cols)
        x_of = lambda col: margin + col * (cw + _GAP)

        # Cabeçalho de colunas (fixo)
        p.setFont(QFont("Segoe UI", 7, QFont.Bold))
        for col in range(cols):
            gc_ = QColor(GROUP_COLORS[col % len(GROUP_COLORS)])
            if col == 0 and self._g0_active():
                beat = 0.5 + 0.5 * math.sin(self.sim.tick_count * 0.22)
                glow = QColor(gc_)
                glow.setAlpha(int(40 + 70 * beat))
                p.setPen(Qt.NoPen)
                p.setBrush(glow)
                p.drawRoundedRect(QRectF(x_of(col) + cw / 2 - 34, 28, 68, 15), 7, 7)
            p.setPen(gc_)
            label = f"G{col}" + (" (seq.)" if col == 0 else "")
            p.drawText(QRectF(x_of(col), 30, cw, 12), Qt.AlignCenter, label)

        # Grade rolável, recortada abaixo do cabeçalho
        p.save()
        p.setClipRect(QRectF(0, _TOP - 2, w, h - _TOP + 2))
        active = {hist[-1]["sn"]: hist[-1]["inst"] for hist in self.sim.sn_history.values() if hist}
        self._cells = {}
        last_sn = history[-1]["sn"] if history else -1
        for sn in range(rows * cols):
            row, col = divmod(sn, cols)
            y = _TOP + self._scroll_y + row * self._row_pitch()
            if y + _ROW_H < _TOP - 4 or y > h:
                continue
            if col == 0:
                continue   # G0 não tem SNs no log: a coluna mostra as variáveis do Sequencer
            rect = QRectF(x_of(col), y, cw, _ROW_H)
            self._cells[sn] = rect
            gcol = QColor(GROUP_COLORS[col % len(GROUP_COLORS)])
            entry = by_sn.get(sn)
            if entry is not None:
                self._draw_block(p, rect, entry, gcol,
                                 is_newest=(sn == last_sn and self._new_block_ttl > 0),
                                 is_hovered=(sn == self._hovered_sn),
                                 is_selected=(sn == self._clicked_sn))
            else:
                self._draw_empty(p, rect, sn, col, gcol, active.get(sn))

        # Setas de encadeamento SN -> SN+1 (na mesma linha; ao fim da linha, vai para a próxima)
        p.setPen(QPen(QColor(255, 255, 255, 60), 1.2))
        for sn in sorted(by_sn):
            a, b = self._cells.get(sn), self._cells.get(sn + 1)
            if a is None or b is None or (sn + 1) not in by_sn:
                continue
            if (sn + 1) % cols:
                p.drawLine(QPointF(a.right() + 1, a.center().y()), QPointF(b.left() - 1, b.center().y()))
            else:
                ymid = a.bottom() + _ROW_GAP / 2
                path = QPainterPath(QPointF(a.center().x(), a.bottom()))
                path.lineTo(a.center().x(), ymid)
                path.lineTo(b.center().x(), ymid)
                path.lineTo(b.center().x(), b.top())
                p.setBrush(Qt.NoBrush)
                p.drawPath(path)
        p.restore()

        self._draw_g0_vars(p, QRectF(x_of(0), _TOP - 2, cw, h - _TOP - 2))

        if self._clicked_sn in by_sn and self._clicked_sn in self._cells:
            self._draw_popup(p, w, h, by_sn[self._clicked_sn], self._cells[self._clicked_sn])
        p.end()

    def _draw_empty(self, p, rect, sn, col, gcol, inst):
        """SN sem entrada no log: tracejado; em andamento (instância aberta) ganha a cor do grupo."""
        path = QPainterPath()
        path.addRoundedRect(rect, 6, 6)
        in_progress = inst is not None
        if col == 0 and self._g0_active():
            self._draw_g0_active(p, rect, sn, gcol)
            return
        if col == 0:
            p.setOpacity(0.35)
        pen = QPen(QColor(gcol.red(), gcol.green(), gcol.blue(), 200 if in_progress else 70), 1.2)
        pen.setStyle(Qt.DashLine)
        p.setPen(pen)
        fill = QColor(gcol)
        fill.setAlpha(28 if in_progress else 0)
        p.setBrush(fill)
        p.drawPath(path)
        p.setPen(QColor(255, 255, 255, 150 if in_progress else 70))
        p.setFont(QFont("Consolas", 8, QFont.Bold))
        p.drawText(QRectF(rect.x(), rect.y() + 6, rect.width(), 14), Qt.AlignCenter, f"SN{sn}")
        p.setFont(QFont("Segoe UI", 6))
        if in_progress:
            p.setPen(gcol)
            p.drawText(QRectF(rect.x(), rect.y() + 22, rect.width(), 12), Qt.AlignCenter, "em andamento")
        p.setOpacity(1.0)

    def _draw_block(self, p, rect, entry, gcol, is_newest, is_hovered, is_selected):
        """SN decidida e gravada no log."""
        lc = gcol
        path = QPainterPath()
        path.addRoundedRect(rect, 6, 6)
        grad = QLinearGradient(rect.x(), rect.y(), rect.x(), rect.bottom())
        c1 = QColor(lc); c1.setAlpha(80 if (is_hovered or is_selected) else 55)
        c2 = QColor(lc); c2.setAlpha(30 if (is_hovered or is_selected) else 18)
        grad.setColorAt(0.0, c1)
        grad.setColorAt(1.0, c2)
        p.fillPath(path, QBrush(grad))
        border = QColor(lc)
        border.setAlpha(230 if is_selected else (190 if is_hovered else 140))
        p.setPen(QPen(border, 2.5 if is_selected else (2.0 if is_hovered else 1.3)))
        p.setBrush(Qt.NoBrush)
        p.drawPath(path)

        if is_newest:
            gc = QColor(lc)
            gc.setAlpha(min(170, self._new_block_ttl * 6))
            gp = QPainterPath()
            gp.addRoundedRect(rect.adjusted(-2, -2, 2, 2), 8, 8)
            p.setPen(QPen(gc, 2))
            p.drawPath(gp)

        if entry["is_cross"]:
            p.setPen(Qt.NoPen)
            p.setBrush(QColor("#D500F9"))
            p.drawEllipse(QPointF(rect.right() - 8, rect.y() + 8), 5, 5)
            p.setPen(QColor(255, 255, 255, 220))
            p.setFont(QFont("Segoe UI", 5, QFont.Bold))
            p.drawText(QRectF(rect.right() - 13, rect.y() + 3, 10, 10), Qt.AlignCenter, "X")

        p.setPen(QColor(C["text"]))
        p.setFont(QFont("Consolas", 9, QFont.Bold))
        p.drawText(QRectF(rect.x(), rect.y() + 3, rect.width(), 14), Qt.AlignCenter, f"SN{entry['sn']}")
        n = entry.get("n", 0)
        self._draw_items(p, rect, entry, lc)
        p.setPen(lc)
        p.setFont(QFont("Segoe UI", 7, QFont.Bold))
        p.drawText(QRectF(rect.x(), rect.y() + 33, rect.width(), 12), Qt.AlignCenter,
                   f"{n} ped. · N{entry['leader']}" if n else f"NOP · N{entry['leader']}")
        p.setPen(QColor(C["text3"]))
        p.setFont(QFont("Consolas", 6))
        p.drawText(QRectF(rect.x(), rect.y() + 47, rect.width(), 10), Qt.AlignCenter, entry["hash"])

    def _draw_g0_active(self, p, rect, sn, gcol):
        """Célula do G0 enquanto o Sequenciador atua: anéis pulsantes, cor viva e o que ele faz.
        Não é entrada de log (o G0 não roda ISS): só mostra que ele está trabalhando."""
        t = self.sim.tick_count
        beat = 0.5 + 0.5 * math.sin(t * 0.22)
        path = QPainterPath()
        path.addRoundedRect(rect, 6, 6)
        fill = QColor(gcol)
        fill.setAlpha(int(25 + 45 * beat))
        p.fillPath(path, fill)
        for k in range(2):
            ph = ((t * 0.03) + k * 0.5) % 1.0
            ring = QColor(gcol)
            ring.setAlpha(int(200 * (1 - ph)))
            p.setPen(QPen(ring, 2.0))
            p.setBrush(Qt.NoBrush)
            grow = 1 + 7 * ph
            rp = QPainterPath()
            rp.addRoundedRect(rect.adjusted(-grow, -grow, grow, grow), 8, 8)
            p.drawPath(rp)
        border = QColor(gcol)
        border.setAlpha(int(150 + 90 * beat))
        pen = QPen(border, 1.6)
        pen.setStyle(Qt.DashLine)
        p.setPen(pen)
        p.drawPath(path)
        p.setPen(QColor(255, 255, 255, 120))
        p.setFont(QFont("Consolas", 8, QFont.Bold))
        p.drawText(QRectF(rect.x(), rect.y() + 5, rect.width(), 14), Qt.AlignCenter, f"SN{sn}")
        if sn == 0:
            p.setPen(gcol)
            p.setFont(QFont("Segoe UI", 7, QFont.Bold))
            p.drawText(QRectF(rect.x() + 2, rect.y() + 22, rect.width() - 4, 14), Qt.AlignCenter,
                       "\u2699 Sequenciador")
            p.setPen(QColor(255, 255, 255, 230))
            p.setFont(QFont("Consolas", 7))
            p.drawText(QRectF(rect.x() + 2, rect.y() + 38, rect.width() - 4, 14), Qt.AlignCenter,
                       self._g0_text or "atuando")
            p.setPen(QColor(C["text3"]))
            p.setFont(QFont("Segoe UI", 6))
            p.drawText(QRectF(rect.x() + 2, rect.y() + 49, rect.width() - 4, 10), Qt.AlignCenter,
                       "(não é entrada do log)")

    def _draw_items(self, p, rect, entry, lc):
        """Os pedidos DENTRO do batch desta SN, como pastilhas com o rótulo (cliente#sn). Cross-op
        em magenta com estrela: o mesmo rótulo aparece nas SNs de todos os grupos que ela toca."""
        items = entry.get("items") or []
        if not items:
            return
        font = QFont("Consolas", 6, QFont.Bold)
        fm = QFontMetrics(font)
        h, gap = 12, 3
        texts = [("★" if it["cross"] else "") + (it["label"][1:] if it["label"].startswith("c") else it["label"])
                 for it in items]
        widths = [fm.horizontalAdvance(t) + 6 for t in texts]
        avail = rect.width() - 10
        shown, used = 0, 0
        for w in widths:
            reserve = 16 if len(items) - shown - 1 > 0 else 0
            if used + w + reserve > avail and shown > 0:
                break
            used += w + gap
            shown += 1
        hidden = len(items) - shown
        total = used - gap + (14 if hidden else 0)
        x = rect.x() + max((rect.width() - total) / 2, 4)
        y = rect.y() + 18
        p.setFont(font)
        for it, t, w in zip(items[:shown], texts[:shown], widths[:shown]):
            cross = it["cross"]
            col = QColor("#D500F9") if cross else QColor(it.get("color") or lc)
            col.setAlpha(230)
            r = QRectF(x, y, w, h)
            p.setPen(QPen(QColor(255, 255, 255, 190), 1.0) if cross else Qt.NoPen)
            p.setBrush(col)
            p.drawRoundedRect(r, 3, 3)
            p.setPen(QColor(255, 255, 255) if cross else QColor(10, 14, 24))
            p.drawText(r, Qt.AlignCenter, t)
            x += w + gap
        if hidden:
            p.setPen(QColor(255, 255, 255, 200))
            p.drawText(QRectF(x, y, 16, h), Qt.AlignLeft | Qt.AlignVCenter, f"+{hidden}")

    def _draw_popup(self, p, panel_w, panel_h, entry, cell):
        """Detalhes do commit selecionado."""
        lines = [
            f"Commit SN={entry['sn']}",
            f"Grupo: G{entry['group']}",
            f"Lider: Node {entry['leader']}",
            f"Checkpoint: {entry['checkpoint_idx']}",
            f"Digest: {entry['hash']}",
            f"Pedidos no batch: {entry.get('n', '?')}",
            *[f"  {it['label']}" + (f"  (cross, GSN {it['gsn']})" if it["cross"] else "")
              for it in (entry.get("items") or [])[:6]],
            f"Cross-group: {'Sim (GSN=' + str(entry['gsn']) + ')' if entry['is_cross'] else 'Nao'}",
            "Quorum: atingido ✓",
        ]
        p.setFont(QFont("Segoe UI", 7))
        fm = p.fontMetrics()
        line_h = fm.height() + 2
        popup_w = max(fm.horizontalAdvance(l) for l in lines) + 20
        popup_h = line_h * len(lines) + 12
        px = max(4, min(cell.x(), panel_w - popup_w - 4))
        py = cell.y() - popup_h - 4
        if py < 2:
            py = min(cell.bottom() + 4, panel_h - popup_h - 2)
            py = max(py, 2)
        rect = QRectF(px, py, popup_w, popup_h)
        bg_path = QPainterPath()
        bg_path.addRoundedRect(rect, 8, 8)
        p.fillPath(bg_path, QColor(10, 18, 32, 245))
        lc = QColor(GROUP_COLORS[entry["group"] % len(GROUP_COLORS)])
        p.setPen(QPen(lc, 1.5))
        p.setBrush(Qt.NoBrush)
        p.drawPath(bg_path)
        ty = py + 6
        for i, line in enumerate(lines):
            if i == 0:
                p.setPen(lc)
                p.setFont(QFont("Segoe UI", 8, QFont.Bold))
            else:
                p.setPen(QColor(C["text"]))
                p.setFont(QFont("Segoe UI", 7))
            p.drawText(QRectF(px + 8, ty, popup_w - 16, line_h), Qt.AlignLeft | Qt.AlignVCenter, line)
            ty += line_h
