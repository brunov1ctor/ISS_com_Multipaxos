"""BucketsPanel — visualização gráfica dos buckets."""

import math
from PySide6.QtWidgets import QWidget, QSizePolicy, QTextEdit, QComboBox
from PySide6.QtCore import Qt, QRectF, QPointF, QTimer, QSize
from PySide6.QtGui import (
    QPainter, QPainterPath, QColor, QLinearGradient, QPen, QBrush, QFont, QFontMetrics
)

from mirbftview.qt.theme import C
from mirbftview.qt.canvas._constants import GROUP_COLORS
from mirbftview.engine.phases import BUCKET_ENTER_TICKS, BUCKET_LEAVE_TICKS, CROSS_PULSE_TICKS, TICKS_PER_SEC
from mirbftview.qt.simulation import Simulation, Phase


class BucketsPanel(QWidget):
    """Visualização gráfica dos buckets — baldes com requests dentro."""

    def __init__(self, sim: Simulation, parent=None):
        super().__init__(parent)
        self.sim = sim
        self.setAttribute(Qt.WA_TranslucentBackground)
        self.setSizePolicy(QSizePolicy.Expanding, QSizePolicy.Expanding)
        self.setMouseTracking(True)
        self._bucket_rects: list[QRectF] = []
        self._selected_bucket: int = -1
        self._timer = QTimer(self)
        self._timer.setInterval(100)
        self._timer.timeout.connect(self._on_timer)
        self._timer.start()

        # Detalhe do bucket: widget real (texto selecionável e copiável com Ctrl+C),
        # ao contrário do popup pintado com QPainter, que não permitia copiar.
        self._detail_text = ""
        self._detail = QTextEdit(self)
        self._detail.setReadOnly(True)
        self._detail.setTextInteractionFlags(Qt.TextSelectableByMouse | Qt.TextSelectableByKeyboard)
        self._detail.setFont(QFont("Consolas", 9))
        self._detail.setLineWrapMode(QTextEdit.WidgetWidth)
        self._detail.setHorizontalScrollBarPolicy(Qt.ScrollBarAlwaysOff)
        self._detail.setStyleSheet(
            "QTextEdit { background: rgba(10,18,32,245); color: #D4D4D4; "
            f"border: 1px solid {C['gold']}; border-radius: 6px; padding: 4px; "
            "selection-background-color: rgba(88,216,255,0.35); }"
        )
        self._detail.hide()

        # Dropdown de NÓ: cada nó tem os SEUS buckets (fila local de pedidos pendentes), e
        # eles podem diferir entre membros do mesmo grupo (ordem de chegada, o líder esvazia
        # no corte do batch e os seguidores só no commit). Aqui se escolhe de qual nó ver.
        self._node_id = 0
        self._node_ids: list[int] = []
        self._node_combo = QComboBox(self)
        self._node_combo.setFixedHeight(20)
        self._node_combo.setStyleSheet(
            "QComboBox { background: rgba(10,16,30,0.75); color: #E6EDF7; font-size: 8pt; "
            "font-weight: bold; padding: 1px 6px; border: 1px solid rgba(255,255,255,0.25); "
            "border-radius: 5px; }"
            "QComboBox QAbstractItemView { background: rgb(14,22,40); color: #E6EDF7; "
            "selection-background-color: rgba(108,99,255,0.55); }"
        )
        self._node_combo.currentIndexChanged.connect(self._on_node_changed)
        self._sync_nodes()

    _TITLE = "\U0001faa3 Buckets do n\u00f3:"

    def _sync_nodes(self):
        ids = [n.id for n in self.sim.nodes]
        if ids == self._node_ids:
            return
        self._node_ids = ids
        self._node_combo.blockSignals(True)
        self._node_combo.clear()
        for n in self.sim.nodes:
            self._node_combo.addItem(n.name, n.id)
        if self._node_id not in ids:
            self._node_id = ids[0] if ids else 0
        self._node_combo.setCurrentIndex(ids.index(self._node_id) if ids else -1)
        self._node_combo.blockSignals(False)
        self._layout_combo()

    def _on_node_changed(self, idx):
        nid = self._node_combo.itemData(idx)
        if nid is not None:
            self._node_id = nid
            self._detail_text = ""
            self._refresh_detail()
            self.update()

    def _layout_combo(self):
        """Dropdown logo depois do título."""
        tw = QFontMetrics(QFont("Segoe UI", 10, QFont.Bold)).horizontalAdvance(self._TITLE)
        self._node_combo.setGeometry(10 + tw + 8, 3, 92, 20)

    def resizeEvent(self, event):
        super().resizeEvent(event)
        self._layout_combo()

    def _node(self):
        return next((n for n in self.sim.nodes if n.id == self._node_id), None)

    def _on_timer(self):
        self._sync_nodes()
        self._refresh_detail()
        self.update()

    def mousePressEvent(self, event):
        if event.button() == Qt.LeftButton:
            pos = event.position()
            clicked = -1
            for i, rect in enumerate(self._bucket_rects):
                if rect.contains(pos):
                    clicked = i
                    break
            if clicked == self._selected_bucket:
                self._selected_bucket = -1
            else:
                self._selected_bucket = clicked
            self._detail_text = ""
            self._refresh_detail()
            self.update()
        super().mousePressEvent(event)

    def mouseMoveEvent(self, event):
        pos = event.position()
        hovering = any(r.contains(pos) for r in self._bucket_rects)
        self.setCursor(Qt.PointingHandCursor if hovering else Qt.ArrowCursor)
        super().mouseMoveEvent(event)

    # Layout em grade: UMA LINHA POR GRUPO, com os buckets que ele possui
    # (g, g+nGrupos, ...). A linha e a cor já dizem quem é o dono; com muitos
    # buckets a grade cresce em colunas e o painel rola.
    _LABEL_W = 34
    _MARGIN_X = 10
    _GAP = 4
    _MIN_CELL_W = 36
    _MIN_CELL_H = 26

    def _grid_shape(self):
        em = self.sim.epoch_mgr
        rows = em.sn_stride
        cols = max(len(em.buckets_of_group(0)), 1)  # G0 é o que tem mais buckets
        return rows, cols

    def minimumSizeHint(self):
        rows, cols = self._grid_shape()
        w = self._MARGIN_X * 2 + self._LABEL_W + cols * (self._MIN_CELL_W + self._GAP)
        h = 26 + 22 + rows * (self._MIN_CELL_H + self._GAP) + 4
        return QSize(w, h)

    def paintEvent(self, event):
        p = QPainter(self)
        p.setRenderHint(QPainter.Antialiasing)
        w, h = self.width(), self.height()
        num = self.sim.num_buckets
        if num == 0:
            p.end()
            return

        self._sync_nodes()
        node = self._node()
        if node is None:
            p.end()
            return
        lists = self.sim.bucket_lists(node.id)

        p.setPen(QColor(C["gold"]))
        p.setFont(QFont("Segoe UI", 10, QFont.Bold))
        p.drawText(QRectF(10, 4, w, 18), Qt.AlignLeft, self._TITLE)

        p.setPen(QColor(C["text3"]))
        p.setFont(QFont("Segoe UI", 7))
        p.drawText(QRectF(10, h - 16, w - 20, 14), Qt.AlignLeft,
                   "Linha = grupo dono | b = grupo + nGrupos*((cliente+sn) mod buckets do grupo)")

        em = self.sim.epoch_mgr
        rows, cols = self._grid_shape()
        gap = self._GAP
        top_y = 26
        avail_w = w - self._MARGIN_X * 2 - self._LABEL_W
        avail_h = (h - 22) - top_y
        cw = max(self._MIN_CELL_W, min(84, (avail_w - gap * (cols - 1)) / cols))
        ch = max(self._MIN_CELL_H, min(46, (avail_h - gap * (rows - 1)) / rows))

        self._bucket_rects = [QRectF() for _ in range(num)]
        bubbles = []
        for g in range(rows):
            y = top_y + g * (ch + gap)
            gcolor = QColor(GROUP_COLORS[g % len(GROUP_COLORS)])

            p.setPen(gcolor)
            p.setFont(QFont("Segoe UI", 8, QFont.Bold))
            p.drawText(QRectF(self._MARGIN_X, y, self._LABEL_W, ch), Qt.AlignVCenter | Qt.AlignLeft, f"G{g}")

            for col, b in enumerate(em.buckets_of_group(g)):
                rect = QRectF(self._MARGIN_X + self._LABEL_W + col * (cw + gap), y, cw, ch)
                self._bucket_rects[b] = rect
                self._draw_bucket_cell(p, rect, b, gcolor, lists, node)
                bubble = self.sim.bucket_bubbles.get(b)
                # O balão (CutBatch/waitForRequests) é do LÍDER: só aparece na visão dele.
                if bubble and bubble["ttl"] > 0 and bubble.get("node", node.id) == node.id:
                    bubbles.append((rect, bubble))

        # Balões por cima de todas as células (uma linha de cima não cobre o de baixo).
        for rect, bubble in bubbles:
            self._draw_bucket_bubble(p, rect.x(), rect.y(), rect.width(), rect.height(), bubble)

        p.end()

    def _draw_bucket_cell(self, p, rect, i, gc, lists, node):
        contents = lists[i] if i < len(lists) else []
        selected = (i == self._selected_bucket)
        now = self.sim.tick_count
        nid = node.id
        owner = self.sim.epoch_mgr.bucket_owner(i)
        # O nó tem TODOS os buckets na memória, mas só recebe pedidos dos grupos dele.
        member = owner == 0 or (owner < len(self.sim.groups) and nid in self.sim.groups[owner].members)

        p.save()
        if not member:
            p.setOpacity(0.28)

        path = QPainterPath()
        path.addRoundedRect(rect, 6, 6)
        fill = QColor(gc)
        fill.setAlpha(70 if selected else 34)
        p.fillPath(path, fill)
        border = QColor(gc)
        border.setAlpha(230 if selected else 130)
        pen = QPen(border, 2.5 if selected else 1.3)
        if not member:
            pen.setStyle(Qt.DashLine)
        p.setPen(pen)
        p.setBrush(Qt.NoBrush)
        p.drawPath(path)

        # Nome do bucket
        p.setPen(QColor(C["text"] if selected else C["text2"]))
        p.setFont(QFont("Segoe UI", 7, QFont.Bold))
        p.drawText(QRectF(rect.x() + 5, rect.y() + 2, rect.width() - 20, 12),
                   Qt.AlignLeft | Qt.AlignVCenter, f"B{i}")

        chip_h, chip_gap = (12 if rect.height() >= 34 else 9), 2
        chip_font = QFont("Consolas", 6, QFont.Bold)
        fm = QFontMetrics(chip_font)
        x0 = rect.x() + 5
        y0 = rect.bottom() - chip_h - 4
        born = self.sim.bucket_born
        meta_of = self.sim.bucket_meta
        cross_col = QColor("#FF3DF2")

        def chip_text(label):
            # "c0#12" -> "0#12"; cross-op ganha uma estrela
            t = label[1:] if label.startswith("c") else label
            return ("\u2605" if meta_of.get(label, {}).get("cross") else "") + t

        # Quais pastilhas cabem (largura de cada uma = largura do texto)
        widths = [fm.horizontalAdvance(chip_text(l)) + 6 for l in contents]
        shown_from = len(contents)
        used = 0
        avail = rect.width() - 10 - 14
        while shown_from > 0 and used + widths[shown_from - 1] + chip_gap <= avail:
            shown_from -= 1
            used += widths[shown_from] + chip_gap
        if shown_from == len(contents) and contents:
            shown_from = len(contents) - 1   # ao menos a última, mesmo cortada
        shown = contents[shown_from:]
        hidden = shown_from

        # Pulso do contador quando entra um pedido novo (dura a animação de entrada)
        pulse = 0.0
        for label in shown:
            t = (now - born.get((nid, i, label), -9999)) / BUCKET_ENTER_TICKS
            if 0 <= t < 1:
                pulse = max(pulse, 1 - t)

        # Contador do BatchTimeout ao lado do badge: só na visão do LÍDER do grupo dono, enquanto
        # o CutBatch espera pedidos (some quando o batch é cortado).
        inst = self.sim.instances.get(owner)
        if (member and inst is not None and inst.leader == nid and inst.phase == Phase.BATCH_CUT
                and getattr(inst, "_waiting", False)):
            limit = getattr(inst, "_wait_limit", 0) or 1
            left = max(limit - getattr(inst, "_wait_elapsed", 0.0), 0) / TICKS_PER_SEC
            p.setPen(QColor(C["gold"]))
            p.setFont(QFont("Segoe UI", 6, QFont.Bold))
            p.drawText(QRectF(rect.x() + 22, rect.y() + 2, rect.width() - 22 - 22, 14),
                       Qt.AlignRight | Qt.AlignVCenter, f"⏱ {left:0.0f}s")

        # Quantos pedidos na fila DESTE nó
        if contents:
            r = 7 + 2.5 * pulse
            cx, cy = rect.right() - 10, rect.y() + 9
            p.setPen(Qt.NoPen)
            p.setBrush(QColor(gc))
            p.drawEllipse(QPointF(cx, cy), r, r)
            p.setPen(QColor(0, 0, 0))
            p.setFont(QFont("Segoe UI", 6, QFont.Bold))
            p.drawText(QRectF(cx - r, cy - r, r * 2, r * 2), Qt.AlignCenter, str(len(contents)))

            # Pedidos como pastilhas COM RÓTULO (cliente#sn), na ordem em que CHEGARAM A ESTE
            # NÓ, na cor do pedido; cross-ops em magenta com estrela. Ao entrar, a pastilha cai
            # de cima, cresce e ganha um anel.
            xk = x0
            for label in shown:
                meta = meta_of.get(label, {})
                is_cross = bool(meta.get("cross"))
                col = QColor(cross_col if is_cross else (meta.get("color") or gc))
                t = min(max((now - born.get((nid, i, label), -9999)) / BUCKET_ENTER_TICKS, 0.0), 1.0)
                e = 1 - (1 - t) ** 3                      # ease-out
                cw_ = fm.horizontalAdvance(chip_text(label)) + 6
                ccx = xk + cw_ / 2
                ccy = y0 + chip_h / 2 - (1 - e) * 16      # cai de cima até o lugar
                sc = 0.4 + 0.6 * e
                rc = QRectF(ccx - cw_ * sc / 2, ccy - chip_h * sc / 2, cw_ * sc, chip_h * sc)
                col.setAlpha(int(110 + 130 * e))
                p.setPen(QPen(QColor(255, 255, 255, 190), 1.2) if is_cross else Qt.NoPen)
                p.setBrush(col)
                p.drawRoundedRect(rc, 3, 3)
                if e > 0.6:
                    p.setFont(chip_font)
                    p.setPen(QColor(255, 255, 255) if is_cross else QColor(10, 14, 24))
                    p.drawText(rc, Qt.AlignCenter, chip_text(label))
                if t < 1:
                    ring = QColor(col)
                    ring.setAlpha(int(200 * (1 - t)))
                    p.setPen(QPen(ring, 1.2))
                    p.setBrush(Qt.NoBrush)
                    rr = 4 + 10 * t
                    p.drawEllipse(QPointF(ccx, y0 + chip_h / 2), rr, rr)
                xk += cw_ + chip_gap
            if hidden > 0:
                p.setPen(QColor(255, 255, 255, 190))
                p.setFont(QFont("Segoe UI", 6, QFont.Bold))
                p.drawText(QRectF(rect.x(), rect.y() + 2, rect.width() - 22, 11),
                           Qt.AlignRight | Qt.AlignVCenter, f"+{hidden}")
        max_chips = max(len(shown), 1)

        # Saída: pedidos que ESTE nó acabou de tirar do bucket (líder: no corte do batch;
        # seguidor: no commit) ficam dourados, sobem, crescem e somem.
        for ev in self.sim.bucket_leaving:
            if ev["bucket"] != i or ev.get("node") != nid:
                continue
            t = min((now - ev["born"]) / BUCKET_LEAVE_TICKS, 1.0)
            col = QColor(C["gold"] if t < 0.35 else (ev["color"] or gc))
            col.setAlpha(int(230 * (1 - t)))
            sc = 1 + 0.6 * t
            chip_w = 22
            ccx = x0 + min(ev["slot"], max_chips - 1) * (chip_w + chip_gap) + chip_w / 2
            ccy = y0 + chip_h / 2 - 26 * t
            p.setPen(Qt.NoPen)
            p.setBrush(col)
            p.drawRoundedRect(QRectF(ccx - chip_w * sc / 2, ccy - chip_h * sc / 2,
                                     chip_w * sc, chip_h * sc), 2, 2)

        # Chegada de cross-op NESTE nó: anéis magenta pulsantes ao redor da célula
        born_x = self.sim.cross_pulse.get((nid, i))
        if born_x is not None and 0 <= now - born_x < CROSS_PULSE_TICKS:
            t = (now - born_x) / CROSS_PULSE_TICKS
            for k in range(2):
                ph = (t * 3 + k * 0.5) % 1.0
                ring = QColor("#FF3DF2")
                ring.setAlpha(int(240 * (1 - t) * (1 - ph)))
                grow = 1 + 9 * ph
                rp = QPainterPath()
                rp.addRoundedRect(rect.adjusted(-grow, -grow, grow, grow), 8, 8)
                p.setPen(QPen(ring, 2.4))
                p.setBrush(Qt.NoBrush)
                p.drawPath(rp)
            tint = QColor("#FF3DF2")
            tint.setAlpha(int(70 * (1 - t) * (0.5 + 0.5 * math.sin(t * 6 * math.pi))))
            p.fillPath(path, tint)

        # Brilho da célula: dourado quando entra pedido NESTE nó, verde quando ele corta o batch
        for ev in self.sim.visual_events:
            if ev.get("bucket") != i or ev.get("node") != nid:
                continue
            if ev["type"] == "bucket_in":
                glow, grow = QColor(C["gold"]), 2
                glow.setAlpha(min(200, ev["ttl"] * 10))
            elif ev["type"] == "batch_cut":
                glow, grow = QColor(C["green"]), 3
                glow.setAlpha(min(230, ev["ttl"] * 12))
            else:
                continue
            gp = QPainterPath()
            gp.addRoundedRect(rect.adjusted(-grow, -grow, grow, grow), 7, 7)
            p.setPen(QPen(glow, 2))
            p.setBrush(Qt.NoBrush)
            p.drawPath(gp)
        p.restore()

    def _draw_bucket_bubble(self, p, bx, by, bucket_w, bucket_h, bubble):
        """Desenha thought bubble estilo balão de pensamento acima do bucket."""
        text = bubble["text"]
        color_key = bubble["color"]
        ttl = bubble["ttl"]
        alpha = min(240, ttl * 8)
        if alpha <= 0:
            return

        color = C.get(color_key, color_key)
        lines = text.split("\n")
        p.setFont(QFont("Consolas", 6))
        fm = p.fontMetrics()
        line_h = fm.height() + 1
        text_w = max(fm.horizontalAdvance(l) for l in lines) + 12
        bubble_w = max(70, min(140, text_w))
        bubble_h = line_h * len(lines) + 8

        # Posiciona acima do bucket
        bbx = bx + bucket_w / 2 - bubble_w / 2
        bby = max(by - bubble_h - 6, 2)

        # Clamp dentro do widget
        if bbx < 2:
            bbx = 2
        if bbx + bubble_w > self.width() - 2:
            bbx = self.width() - bubble_w - 2

        # Fundo do balão
        bg_path = QPainterPath()
        bg_path.addRoundedRect(QRectF(bbx, bby, bubble_w, bubble_h), 6, 6)
        bg_color = QColor(12, 20, 38, alpha)
        p.fillPath(bg_path, bg_color)

        # Borda
        border_c = QColor(color)
        border_c.setAlpha(alpha)
        p.setPen(QPen(border_c, 1.2))
        p.setBrush(Qt.NoBrush)
        p.drawPath(bg_path)

        # Bolinhas de pensamento (conectam balão ao bucket)
        p.setPen(Qt.NoPen)
        dot_c = QColor(color)
        dot_c.setAlpha(alpha)
        p.setBrush(dot_c)
        anchor_x = bx + bucket_w / 2
        anchor_y = by
        for frac, r in [(0.25, 2.0), (0.55, 3.0), (0.82, 2.0)]:
            cx = anchor_x + (bbx + bubble_w / 2 - anchor_x) * frac * 0.4
            cy = anchor_y + (bby + bubble_h - anchor_y) * frac
            p.drawEllipse(QPointF(cx, cy), r, r)

        # Texto
        ty = bby + 4
        for idx, line in enumerate(lines):
            if idx == 0:
                tc = QColor(color)
                tc.setAlpha(alpha)
                p.setPen(tc)
                p.setFont(QFont("Consolas", 6, QFont.Bold))
            else:
                tc = QColor(C["text"])
                tc.setAlpha(alpha)
                p.setPen(tc)
                p.setFont(QFont("Consolas", 6))
            p.drawText(QRectF(bbx + 5, ty, bubble_w - 10, line_h),
                       Qt.AlignLeft | Qt.AlignVCenter, line)
            ty += line_h

    def _detail_lines(self, bid: int) -> list[str]:
        sim = self.sim
        em = sim.epoch_mgr
        node = self._node()
        nid = node.id if node else self._node_id
        lists = sim.bucket_lists(nid)
        contents = lists[bid] if bid < len(lists) else []
        owner = em.bucket_owner(bid)
        ng = em.sn_stride
        mine = em.buckets_of_group(owner)
        member = owner == 0 or (owner < len(sim.groups) and nid in sim.groups[owner].members)

        lines = [f"Bucket {bid} no {node.name if node else nid} - dono: G{owner}"]
        if owner == 0:
            lines.append("G0 = Sequenciador: mensagens SYSTEM (GSN/META), nao pedidos de cliente.")
        elif not member:
            lines.append(f"Este no NAO pertence ao G{owner}: tem o bucket na memoria, mas nunca")
            lines.append("recebe pedidos dele (o proxy so envia aos membros do grupo).")
        lines += [
            f"Buckets do G{owner}: {mine}",
            f"Pedidos neste no: {len(contents)}",
            "",
            "Cada no tem os SEUS buckets. O LIDER tira o pedido do bucket ao CORTAR o",
            "batch (CutBatch); os seguidores so removem no COMMIT. Entre o corte e o",
            "commit o bucket do lider fica mais vazio que o dos seguidores. O que e",
            "igual em todos e o log decidido pelo Paxos.",
            "",
            "Formula real (GetBucketNr):",
            "  b = grupo + nGrupos * ((clienteId + clienteSn) mod nBucketsDoGrupo)",
            f"  b = {owner} + {ng} * ((clienteId + clienteSn) mod {len(mine)})",
            "",
            "Fila (na ordem de chegada a este no):",
        ]
        for item in contents[:12]:
            lines.append(f"  {item}")
        if len(contents) > 12:
            lines.append(f"  ... +{len(contents) - 12} mais")
        if not contents:
            lines.append("  (vazio - aguardando pedidos)")
        return lines

    def _refresh_detail(self):
        """Mostra/atualiza o detalhe do bucket selecionado (texto selecionável).
        O texto só é reescrito quando muda, para não perder a seleção do usuário."""
        bid = self._selected_bucket
        if not (0 <= bid < self.sim.num_buckets):
            self._detail.hide()
            return
        text = "\n".join(self._detail_lines(bid))
        if text != self._detail_text:
            self._detail_text = text
            self._detail.setPlainText(text)

        w, h = self.width(), self.height()
        doc = self._detail.document()
        doc.setTextWidth(-1)
        # No máximo ~60% da largura: deixa a grade visível ao lado (linhas longas quebram).
        popup_w = int(min(doc.idealWidth() + 28, max(w * 0.6, 220), w - 12))
        doc.setTextWidth(popup_w - 14)
        popup_h = int(min(doc.size().height() + 14, max(h - 30, 60)))
        self._detail.setVerticalScrollBarPolicy(
            Qt.ScrollBarAlwaysOff if popup_h >= doc.size().height() + 14 else Qt.ScrollBarAsNeeded)
        self._detail.setGeometry(w - popup_w - 6, 24, popup_w, popup_h)
        self._detail.show()
        self._detail.raise_()

