"""Painéis educacionais — explicam cada fase com detalhes."""

from PySide6.QtWidgets import (
    QWidget, QVBoxLayout, QLabel, QScrollArea, QSizePolicy, QPlainTextEdit, QPushButton
)
from PySide6.QtCore import Qt, QRectF, QPointF, QTimer
from PySide6.QtGui import QPainter, QPainterPath, QColor, QPen, QBrush, QFont, QLinearGradient, QRadialGradient

from mirbftview.qt.theme import C
from mirbftview.qt.simulation import Simulation, Phase, RequestInfo


def _title(text, color=None):
    lbl = QLabel(text)
    c = color or C["primary"]
    lbl.setStyleSheet(f"color: {c}; font-size: 11pt; font-weight: bold; border: none; background: transparent;")
    return lbl


def _body(text=""):
    lbl = QLabel(text)
    lbl.setStyleSheet(f"color: {C['text']}; font-size: 9pt; border: none; background: transparent;")
    lbl.setWordWrap(True)
    lbl.setTextFormat(Qt.PlainText)
    return lbl


class InfoPanel(QWidget):
    """Painel principal de informacoes — mostra requests ativas com dropdown."""

    def __init__(self, sim: Simulation, parent=None):
        super().__init__(parent)
        self.sim = sim
        self.setAttribute(Qt.WA_TranslucentBackground)
        self.setSizePolicy(QSizePolicy.Expanding, QSizePolicy.Expanding)
        layout = QVBoxLayout(self)
        layout.setContentsMargins(10, 10, 10, 10)
        layout.setSpacing(6)

        layout.addWidget(_title("O que esta acontecendo"))

        scroll = QScrollArea()
        scroll.setWidgetResizable(True)
        scroll.setHorizontalScrollBarPolicy(Qt.ScrollBarAlwaysOff)
        scroll.setStyleSheet("background: transparent; border: none;")

        self._container = QWidget()
        self._container.setAttribute(Qt.WA_TranslucentBackground)
        self._container_layout = QVBoxLayout(self._container)
        self._container_layout.setContentsMargins(0, 0, 0, 0)
        self._container_layout.setSpacing(2)
        self._container_layout.addStretch()
        scroll.setWidget(self._container)
        layout.addWidget(scroll, 1)

        self._expanded: set[int] = set()  # SNs expandidos
        self._items: list[QWidget] = []
        self._last_snapshot = ""
        self._timer = QTimer(self)
        self._timer.setInterval(150)
        self._timer.timeout.connect(self._refresh)
        self._timer.start()

    def _refresh(self):
        active = self.sim.active_requests
        if not active:
            snapshot = "idle"
        else:
            snapshot = "|".join(f"{r.sn}:{r.phase.name if r.phase else ''}" for r in active)
        if snapshot == self._last_snapshot:
            return
        self._last_snapshot = snapshot
        self._rebuild(active)

    def _rebuild(self, active):
        # Remove widgets antigos
        for item in self._items:
            item.setParent(None)
            item.deleteLater()
        self._items = []

        if not active:
            lbl = _body(self.sim.info_text)
            self._container_layout.insertWidget(0, lbl)
            self._items.append(lbl)
            return

        for req in active:
            item = self._make_item(req)
            self._container_layout.insertWidget(len(self._items), item)
            self._items.append(item)

    def _make_item(self, req) -> QWidget:
        from mirbftview.qt.simulation import Phase
        sn = req.sn
        phase_name = req.phase.name.replace('_', ' ').title() if req.phase else "Idle"
        cross = " [cross]" if req.is_cross_group else ""
        is_open = sn in self._expanded

        widget = QWidget()
        widget.setAttribute(Qt.WA_TranslucentBackground)
        vl = QVBoxLayout(widget)
        vl.setContentsMargins(0, 0, 0, 0)
        vl.setSpacing(0)

        # Header button
        arrow = "v" if is_open else ">"
        btn = QPushButton(f" {arrow}  SN{sn} | N{req.leader} | {phase_name}{cross}")
        btn.setStyleSheet(
            f"QPushButton {{ text-align: left; color: {C['text']}; font-size: 9pt; "
            f"font-family: Consolas; background: rgba(255,255,255,0.04); "
            f"border: none; border-radius: 4px; padding: 4px 6px; }}"
            f"QPushButton:hover {{ background: rgba(255,255,255,0.08); }}"
        )
        btn.setCursor(Qt.PointingHandCursor)
        btn.clicked.connect(lambda checked, s=sn: self._toggle(s))
        vl.addWidget(btn)

        # Detail (collapsible)
        if is_open:
            detail = _body(self._get_detail(req))
            detail.setStyleSheet(
                f"color: {C['text2']}; font-size: 8pt; border: none; "
                f"background: rgba(255,255,255,0.02); padding: 4px 12px; border-radius: 4px;"
            )
            vl.addWidget(detail)

        return widget

    def _toggle(self, sn: int):
        if sn in self._expanded:
            self._expanded.discard(sn)
        else:
            self._expanded.add(sn)
        self._last_snapshot = ""  # force rebuild
        self._refresh()

    @staticmethod
    @staticmethod
    def _get_detail(req) -> str:
        from mirbftview.qt.simulation import Phase
        p = req.phase
        if not p:
            return ""
        dispatch = {
            Phase.CLIENT_SEND: (
                f"Um cliente enviou um pedido!\n"
                f"\n"
                f"Imagine que Cliente {req.client_id} e um usuario enviando\n"
                f"uma transacao para o sistema.\n"
                f"\n"
                f"O pedido vai para o Node {req.proxy_node} (o 'carteiro'\n"
                f"que recebe e encaminha ao responsavel).\n"
                f"\n"
                f"Detalhes tecnicos:\n"
                f"Payload Hash: {req.payload_hash[:8]}\n"
                f"Bucket: {req.bucket_id} | Lider: Node {req.leader}"
            ),
            Phase.BUCKET_ASSIGN: (
                f"Classificando o pedido numa 'caixa de entrada'\n"
                f"\n"
                f"O sistema tem varias caixas (buckets).\n"
                f"Cada caixa tem um responsavel (lider).\n"
                f"\n"
                f"O pedido e enviado para TODOS os membros\n"
                f"do grupo G{req.group_id} (nao so o lider -\n"
                f"garante tolerancia a falhas).\n"
                f"\n"
                f"Como a fila e escolhida?\n"
                f"Formula: (cliente + no do pedido) mod N\n"
                f"= Bucket {req.bucket_id}\n"
                f"Lider do bucket: Node {req.leader}"
            ),
            Phase.BATCH_CUT: (
                f"Empacotando pedidos para votacao\n"
                f"\n"
                f"O lider (Node {req.leader}) junta pedidos num\n"
                f"'pacote' (batch) antes de pedir aprovacao.\n"
                f"\n"
                f"Agora o pacote recebe um numero de ordem\n"
                f"(SN={req.sn}) e vai para votacao no grupo.\n"
                f"\n"
                f"Detalhes tecnicos:\n"
                f"Batch: {req.batch_requests} reqs | Digest: {req.batch_digest[:10]}"
            ),
            Phase.GSN_ASSIGN: (
                f"Numeracao global para pedidos multi-grupo\n"
                f"\n"
                f"Este pedido afeta mais de um grupo!\n"
                f"Para evitar conflitos, um coordenador\n"
                f"atribui um numero de ordem GLOBAL: GSN={req.gsn}\n"
                f"\n"
                f"E como numerar senhas num hospital que tem\n"
                f"varios guiches - garante que todos atendam\n"
                f"na mesma ordem.\n"
                f"\n"
                f"Grupos envolvidos: {req.touched_groups}"
            ),
            Phase.PREPARE: (
                f"PREPARE - O lider pede permissao\n"
                f"\n"
                f"Node {req.leader} pergunta ao grupo:\n"
                f"'Posso coordenar a decisao no {req.sn}?'\n"
                f"\n"
                f"Precisa que a MAIORIA concorde ({req.quorum} de\n"
                f"membros) - isso e o 'quorum'.\n"
                f"\n"
                f"Se a maioria disser sim, ele tem autoridade\n"
                f"para propor um valor.\n"
                f"\n"
                f"Ballot={req.ballot} | Grupo G{req.group_id}"
            ),
            Phase.PROMISE: (
                f"PROMISE - O grupo concorda\n"
                f"\n"
                f"Os membros responderam: 'Sim, voce pode\n"
                f"coordenar! Prometemos nao aceitar outro\n"
                f"coordenador com prioridade menor.'\n"
                f"\n"
                f"Respostas: {req.promises_received} de {req.quorum} necessarias\n"
                f"\n"
                f"Analogia: e como uma eleicao - a maioria\n"
                f"votou neste lider, entao ele tem mandato."
            ),
            Phase.ACCEPT: (
                f"ACCEPT - O lider propoe o valor\n"
                f"\n"
                f"Agora que tem permissao, Node {req.leader} diz:\n"
                f"'Proponho que o pacote {req.batch_digest[:6]}...\n"
                f"seja registrado na posicao {req.sn}.'\n"
                f"\n"
                f"Todos os membros do grupo recebem a proposta\n"
                f"e vao decidir se aceitam ou nao.\n"
                f"\n"
                f"Ballot={req.ballot} | Digest={req.batch_digest[:10]}"
            ),
            Phase.ACCEPTED: (
                f"ACCEPTED - Todos concordam!\n"
                f"\n"
                f"A maioria aceitou a proposta:\n"
                f"{req.accepted_received} de {req.quorum} necessarios\n"
                f"\n"
                f"Cada membro gravou o pacote no seu registro.\n"
                f"Agora e IMPOSSIVEL que outro valor seja\n"
                f"registrado nesta posicao - consenso quase pronto!\n"
                f"\n"
                f"Proximo: o lider confirma para todos (COMMIT)."
            ),
            Phase.COMMIT: (
                f"COMMIT - Decisao final!\n"
                f"\n"
                f"O grupo DECIDIU: o pacote foi aceito\n"
                f"permanentemente na posicao {req.sn}.\n"
                f"\n"
                f"Nenhum participante pode voltar atras.\n"
                f"\n"
                f"Agora falta avisar o cliente que seu\n"
                f"pedido foi processado com sucesso.\n"
                f"\n"
                f"Digest: {req.batch_digest[:10]}"
            ),
            Phase.COMMIT_NOTIFY: (
                f"Avisando o cliente: 'Seu pedido foi aprovado!'\n"
                f"\n"
                f"O 'carteiro' (Node {req.proxy_node}) segurou a resposta\n"
                f"ate ter CERTEZA de que o grupo aprovou.\n"
                f"\n"
                f"Agora envia a confirmacao ao Cliente {req.client_id}.\n"
                f"\n"
                f"Fluxo completo:\n"
                f"Cliente -> Carteiro -> Lider -> Votacao ->\n"
                f"Aprovacao -> Carteiro -> Cliente"
            ),
            Phase.ADELIVER: (
                (f"Entrega confirmada!\n"
                f"\n"
                f"Pedido multi-grupo no {req.gsn} entregue\n"
                f"na ordem correta!\n"
                f"\n"
                f"O sistema garante que pedidos que afetam\n"
                f"multiplos grupos sejam entregues na mesma\n"
                f"ordem em todos os grupos.\n"
                f"\n"
                f"Grupos: {req.touched_groups}")
                if req.gsn > 0 else
                ("Entrega direta (grupo unico).\n"
                "\n"
                "Pedido simples -> entregue imediatamente.")
            ),
            Phase.CHECKPOINT: (
                f"CHECKPOINT - Salvando progresso\n"
                f"\n"
                f"O sistema 'salva o jogo' periodicamente.\n"
                f"Todos os nos confirmam entre si que estao\n"
                f"sincronizados ate a decisao no {req.sn}.\n"
                f"\n"
                f"Isso permite descartar dados antigos e\n"
                f"ajuda nos que ficaram para tras a se\n"
                f"atualizarem rapidamente."
            ),
            Phase.VIEW_CHANGE: (
                f"Lider caiu! Elegendo substituto...\n"
                f"\n"
                f"Node {req.leader} parou de responder.\n"
                f"Os outros perceberam (timeout) e estao\n"
                f"elegendo um novo coordenador.\n"
                f"\n"
                f"O sistema continua funcionando mesmo com\n"
                f"falhas - isso e tolerancia a faltas!"
            ),
            Phase.RETRANSMIT: (
                f"TIMEOUT! Retransmitindo...\n"
                f"\n"
                f"O lider (Node {req.leader}) esperou uma resposta\n"
                f"mas nem todos responderam a tempo.\n"
                f"\n"
                f"Isso pode acontecer por:\n"
                f"- Rede lenta entre os nos\n"
                f"- Um no sobrecarregado\n"
                f"- Mensagem perdida no caminho\n"
                f"\n"
                f"Solucao: o lider REENVIA a proposta.\n"
                f"O protocolo e seguro: reenviar nao causa\n"
                f"problemas (duplicadas sao ignoradas).\n"
                f"\n"
                f"SN={req.sn} | Ballot={req.ballot}"
            ),
        }
        return dispatch.get(p, "")


class BucketsPanel(QWidget):
    """Visualização gráfica dos buckets — baldes com requests dentro. Clique para detalhes."""

    def __init__(self, sim: Simulation, parent=None):
        super().__init__(parent)
        self.sim = sim
        self.setAttribute(Qt.WA_TranslucentBackground)
        self.setMinimumHeight(140)
        self.setSizePolicy(QSizePolicy.Expanding, QSizePolicy.Expanding)
        self.setMouseTracking(True)
        self._bucket_rects: list[QRectF] = []  # hit areas
        self._selected_bucket: int = -1
        self._timer = QTimer(self)
        self._timer.setInterval(100)
        self._timer.timeout.connect(self.update)
        self._timer.start()

    def mousePressEvent(self, event):
        if event.button() == Qt.LeftButton:
            pos = event.position()
            clicked = -1
            for i, rect in enumerate(self._bucket_rects):
                if rect.contains(pos):
                    clicked = i
                    break
            if clicked == self._selected_bucket:
                self._selected_bucket = -1  # toggle off
            else:
                self._selected_bucket = clicked
            self.update()
        super().mousePressEvent(event)

    def mouseMoveEvent(self, event):
        pos = event.position()
        hovering = any(r.contains(pos) for r in self._bucket_rects)
        self.setCursor(Qt.PointingHandCursor if hovering else Qt.ArrowCursor)
        super().mouseMoveEvent(event)

    def paintEvent(self, event):
        p = QPainter(self)
        p.setRenderHint(QPainter.Antialiasing)
        w, h = self.width(), self.height()
        num = self.sim.num_buckets
        if num == 0:
            p.end()
            return

        # Title
        p.setPen(QColor(C["gold"]))
        p.setFont(QFont("Segoe UI", 10, QFont.Bold))
        p.drawText(QRectF(10, 4, w, 18), Qt.AlignLeft, "🪣 Buckets (clique para detalhes)")

        # Formula
        p.setPen(QColor(C["text3"]))
        p.setFont(QFont("Segoe UI", 7))
        p.drawText(QRectF(10, h - 16, w - 20, 14), Qt.AlignLeft,
                   f"Regra: bucket = (clientID + clientSN) mod {num}")

        # Layout buckets
        top_y = 26
        bot_y = h - 22
        available_h = bot_y - top_y
        margin_x = 14  # margem lateral para nao cortar primeiro/ultimo
        available_w = w - margin_x * 2
        bucket_w = max(28, min(70, available_w / num - 4))
        total_w = bucket_w * num
        spacing = (available_w - total_w) / max(num - 1, 1) if num > 1 else 0
        spacing = max(2, min(spacing, 12))
        total_w = bucket_w * num + spacing * (num - 1)
        start_x = margin_x + (available_w - total_w) / 2

        group_colors = ["#EF4444", "#6C63FF", "#34D399", "#F97316", "#A855F7", "#58D8FF", "#FACC15", "#8B7DFF"]

        self._bucket_rects = []

        for i in range(num):
            contents = self.sim.bucket_contents[i] if i < len(self.sim.bucket_contents) else []
            x = start_x + i * (bucket_w + spacing)
            group_id = 1 + (i % max(1, len(self.sim.groups) - 1))
            gc = QColor(group_colors[group_id % len(group_colors)])

            # Bucket shape (trapezoid/container)
            bucket_h = available_h - 20
            bx, by = x, top_y + 14
            taper = 4

            # Store rect for hit-test
            self._bucket_rects.append(QRectF(bx, by, bucket_w, bucket_h))

            is_selected = (i == self._selected_bucket)

            path = QPainterPath()
            path.moveTo(bx, by)
            path.lineTo(bx + bucket_w, by)
            path.lineTo(bx + bucket_w - taper, by + bucket_h)
            path.lineTo(bx + taper, by + bucket_h)
            path.closeSubpath()

            # Fill
            grad = QLinearGradient(bx, by, bx, by + bucket_h)
            base = QColor(gc)
            base.setAlpha(60 if is_selected else 30)
            grad.setColorAt(0.0, QColor(gc.red(), gc.green(), gc.blue(), 30 if is_selected else 15))
            grad.setColorAt(1.0, base)
            p.fillPath(path, QBrush(grad))

            # Border (highlight if selected)
            border = QColor(gc)
            border.setAlpha(220 if is_selected else 120)
            p.setPen(QPen(border, 2.5 if is_selected else 1.5))
            p.setBrush(Qt.NoBrush)
            p.drawPath(path)

            # Requests inside (blocks stacked from bottom)
            max_visible = 5
            block_h = max(10, min(18, (bucket_h - 8) / max(max_visible, 1)))
            visible = contents[-max_visible:]
            for j, req_label in enumerate(reversed(visible)):
                ry = by + bucket_h - 4 - (j + 1) * (block_h + 2)
                rx = bx + taper + 2
                rw = bucket_w - taper * 2 - 4
                if ry < by + 2:
                    break
                req_path = QPainterPath()
                req_path.addRoundedRect(QRectF(rx, ry, rw, block_h), 3, 3)
                req_color = QColor(gc)
                req_color.setAlpha(160)
                p.fillPath(req_path, req_color)
                p.setPen(QColor(0, 0, 0, 200))
                p.setFont(QFont("Consolas", 6))
                p.drawText(QRectF(rx, ry, rw, block_h), Qt.AlignCenter, req_label[:10])

            # Overflow indicator
            if len(contents) > max_visible:
                p.setPen(QColor(255, 255, 255, 140))
                p.setFont(QFont("Segoe UI", 6))
                p.drawText(QRectF(bx, by + 2, bucket_w, 10), Qt.AlignCenter, f"+{len(contents) - max_visible}")

            self._draw_bucket_labels(p, bx, by, bucket_w, bucket_h, top_y, i, group_id, gc, is_selected, contents)

            # Batch fill progress bar (lateral)
            fill_count = self.sim.batch_fill.get(i, 0) if hasattr(self.sim, 'batch_fill') else 0
            batch_size = self.sim.batch_visual_size if hasattr(self.sim, 'batch_visual_size') else 3
            if fill_count > 0:
                fill_pct = min(1.0, fill_count / max(batch_size, 1))
                bar_w = 3
                bar_x_pos = bx + bucket_w + 1
                bar_full_h = bucket_h
                bar_fill_h = bar_full_h * fill_pct
                # Background
                p.setPen(Qt.NoPen)
                p.setBrush(QColor(255, 255, 255, 15))
                p.drawRoundedRect(QRectF(bar_x_pos, by, bar_w, bar_full_h), 1, 1)
                # Fill (bottom to top)
                fill_color = QColor(C["green"]) if fill_pct >= 1.0 else QColor(gc)
                fill_color.setAlpha(200 if fill_pct >= 1.0 else 140)
                p.setBrush(fill_color)
                p.drawRoundedRect(QRectF(bar_x_pos, by + bar_full_h - bar_fill_h, bar_w, bar_fill_h), 1, 1)
                # "CORTE!" flash when full
                if fill_pct >= 1.0:
                    p.setPen(QColor(C["green"]))
                    p.setFont(QFont("Segoe UI", 5, QFont.Bold))
                    p.drawText(QRectF(bx, by - 10, bucket_w, 9), Qt.AlignCenter, "CORTE")

            # Flash effects from visual_events
            for ev in (self.sim.visual_events if hasattr(self.sim, 'visual_events') else []):
                if ev.get("bucket") != i:
                    continue
                alpha = min(200, ev["ttl"] * 7)
                if ev["type"] == "bucket_in":
                    # Glow amarelo no bucket quando request entra
                    glow_c = QColor(C["gold"])
                    glow_c.setAlpha(alpha)
                    glow_path = QPainterPath()
                    glow_path.addRoundedRect(QRectF(bx - 2, by - 2, bucket_w + 4, bucket_h + 4), 6, 6)
                    p.setPen(QPen(glow_c, 2))
                    p.setBrush(Qt.NoBrush)
                    p.drawPath(glow_path)
                elif ev["type"] == "batch_cut":
                    # Flash verde quando batch e cortado
                    flash_c = QColor(C["green"])
                    flash_c.setAlpha(alpha)
                    p.setPen(Qt.NoPen)
                    p.setBrush(flash_c)
                    p.drawRoundedRect(QRectF(bx, by, bucket_w, bucket_h), 4, 4)

        # Selected bucket detail popup
        if 0 <= self._selected_bucket < num:
            self._draw_bucket_detail(p, w, h)

        p.end()

    def _draw_bucket_labels(self, p: QPainter, bx, by, bucket_w, bucket_h, top_y, i, group_id, gc, is_selected, contents):
        """Desenha labels, indicador de grupo e badge de contagem de um bucket."""
        p.setPen(QColor(C["text"] if is_selected else C["text2"]))
        p.setFont(QFont("Segoe UI", 7, QFont.Bold))
        p.drawText(QRectF(bx, by + bucket_h + 2, bucket_w, 12), Qt.AlignCenter, f"B{i}")

        p.setPen(gc)
        p.setFont(QFont("Segoe UI", 6))
        p.drawText(QRectF(bx, top_y, bucket_w, 12), Qt.AlignCenter, f"\u2192G{group_id}")

        if contents:
            badge_r = 8
            badge_x = bx + bucket_w - badge_r
            badge_y = by - badge_r + 2
            p.setPen(Qt.NoPen)
            p.setBrush(QColor(gc))
            p.drawEllipse(QPointF(badge_x, badge_y), badge_r, badge_r)
            p.setPen(QColor(0, 0, 0))
            p.setFont(QFont("Segoe UI", 6, QFont.Bold))
            p.drawText(QRectF(badge_x - badge_r, badge_y - badge_r, badge_r * 2, badge_r * 2),
                       Qt.AlignCenter, str(len(contents)))

    def _draw_bucket_detail(self, p: QPainter, w: float, h: float):
        """Desenha popup de detalhes do bucket selecionado (overlay no topo)."""
        bid = self._selected_bucket
        contents = self.sim.bucket_contents[bid] if bid < len(self.sim.bucket_contents) else []

        # Quem e o lider deste bucket?
        leader = "?"
        if hasattr(self.sim, 'epoch_mgr') and self.sim.epoch_mgr:
            ba = self.sim.epoch_mgr.bucket_assignment
            for lid, bkts in ba.items():
                if bid in bkts:
                    leader = f"Node {lid}"
                    break

        group_id = 1 + (bid % max(1, len(self.sim.groups) - 1))
        fill_count = self.sim.batch_fill.get(bid, 0) if hasattr(self.sim, 'batch_fill') else 0
        batch_size = self.sim.batch_visual_size if hasattr(self.sim, 'batch_visual_size') else 3

        lines = [
            f"Bucket {bid}",
            f"Lider: {leader}",
            f"Grupo destino: G{group_id}",
            f"Pedidos na fila: {len(contents)}",
            f"Batch fill: {fill_count}/{batch_size}",
            f"Formula: (clientID + clientSN) mod {self.sim.num_buckets} = {bid}",
        ]
        for item in contents[:6]:
            lines.append(f"  {item}")
        if len(contents) > 6:
            lines.append(f"  ... +{len(contents) - 6} mais")
        if not contents:
            lines.append("  (vazio - aguardando requests)")

        # Draw popup overlay (top-right, always visible)
        p.setFont(QFont("Segoe UI", 8))
        fm = p.fontMetrics()
        line_h = fm.height() + 2
        popup_w = min(220, w - 10)
        popup_h = line_h * len(lines) + 14
        px = w - popup_w - 6
        py = 24

        rect = QRectF(px, py, popup_w, popup_h)
        bg = QPainterPath()
        bg.addRoundedRect(rect, 8, 8)
        p.fillPath(bg, QColor(10, 18, 32, 245))
        p.setPen(QPen(QColor(C["gold"]), 1.5))
        p.drawPath(bg)

        y = py + 8
        for i, line in enumerate(lines):
            if i == 0:
                p.setPen(QColor(C["gold"]))
                p.setFont(QFont("Segoe UI", 9, QFont.Bold))
            elif line.startswith("  "):
                p.setPen(QColor(C["accent"]))
                p.setFont(QFont("Consolas", 7))
            else:
                p.setPen(QColor(C["text"]))
                p.setFont(QFont("Segoe UI", 8))
            p.drawText(QRectF(px + 10, y, popup_w - 20, line_h), Qt.AlignLeft | Qt.AlignVCenter, line)
            y += line_h


class ExecutionPanel(QWidget):
    """Painel visual de execução — pipeline gráfico + métricas + progresso."""

    _PHASE_COLORS = {
        Phase.CLIENT_SEND: "#A9B4C8",
        Phase.BUCKET_ASSIGN: "#A9B4C8",
        Phase.BATCH_CUT: "#F97316",
        Phase.GSN_ASSIGN: "#F97316",
        Phase.PREPARE: "#FACC15",
        Phase.PROMISE: "#58D8FF",
        Phase.ACCEPT: "#6C63FF",
        Phase.ACCEPTED: "#8B7DFF",
        Phase.COMMIT: "#34D399",
        Phase.COMMIT_NOTIFY: "#34D399",
        Phase.ADELIVER: "#34D399",
        Phase.CHECKPOINT: "#EF4444",
        Phase.EPOCH_TRANSITION: "#EF4444",
        Phase.VIEW_CHANGE: "#EF4444",
        Phase.RETRANSMIT: "#F97316",
    }

    # Fases ordenadas para a barra de pipeline
    _PHASE_ORDER = [
        Phase.CLIENT_SEND, Phase.BUCKET_ASSIGN, Phase.BATCH_CUT, Phase.GSN_ASSIGN,
        Phase.PREPARE, Phase.PROMISE, Phase.ACCEPT, Phase.ACCEPTED,
        Phase.COMMIT, Phase.COMMIT_NOTIFY, Phase.ADELIVER,
    ]

    def __init__(self, sim: Simulation, parent=None):
        super().__init__(parent)
        self.sim = sim
        self.setAttribute(Qt.WA_TranslucentBackground)
        self.setSizePolicy(QSizePolicy.Expanding, QSizePolicy.Expanding)
        self.setMinimumHeight(130)
        self._timer = QTimer(self)
        self._timer.setInterval(100)
        self._timer.timeout.connect(self.update)
        self._timer.start()

    def paintEvent(self, event):
        p = QPainter(self)
        p.setRenderHint(QPainter.Antialiasing)
        w, h = self.width(), self.height()
        s = self.sim

        # Title
        p.setPen(QColor(C["green"]))
        p.setFont(QFont("Segoe UI", 10, QFont.Bold))
        p.drawText(QRectF(10, 4, w, 18), Qt.AlignLeft, "Execucao")

        # ── Metrics badges (top row) ──
        badge_y = 24
        badge_h = 32
        n_leaders = len(s.epoch_mgr.current_leaders) if s.epoch_mgr else 0
        remaining = s._checkpoint_interval - (s._committed % s._checkpoint_interval) if s._committed > 0 else s._checkpoint_interval
        metrics = [
            ("Epoch", str(s._epoch), C["accent"]),
            ("Commits", str(s._committed), C["green"]),
            ("Lideres", str(n_leaders), C["gold"]),
            ("Prox.Ckpt", str(remaining), C["orange"]),
        ]
        badge_w = (w - 20 - 6 * (len(metrics) - 1)) / len(metrics)
        for i, (label, val, color) in enumerate(metrics):
            bx = 10 + i * (badge_w + 6)
            self._draw_metric_badge(p, bx, badge_y, badge_w, badge_h, label, val, color)

        # ── Pipeline visual (center) ──
        pipe_y = badge_y + badge_h + 10
        pipe_h = max(16, min(22, (h - pipe_y - 36) / max(len(s.active_requests), 1) - 3))
        active = s.active_requests

        if active:
            p.setPen(QColor(C["text3"]))
            p.setFont(QFont("Segoe UI", 7))
            p.drawText(QRectF(10, pipe_y - 12, w - 20, 12), Qt.AlignLeft, "Pipeline de Requests")

            for idx, req in enumerate(active[:6]):
                ry = pipe_y + idx * (pipe_h + 3)
                if ry + pipe_h > h - 28:
                    break
                self._draw_pipeline_bar(p, 10, ry, w - 20, pipe_h, req)
        else:
            p.setPen(QColor(C["text3"]))
            p.setFont(QFont("Segoe UI", 8))
            p.drawText(QRectF(10, pipe_y, w - 20, 30), Qt.AlignCenter, "Aguardando requests...")

        # ── GSN / META / ADeliver section ──
        section_y = pipe_y + (min(len(active), 6) * (pipe_h + 3) if active else 30) + 4
        section_y = self._draw_gsn_meta_section(p, 10, section_y, w - 20, s)
        section_y = self._draw_adeliver_section(p, 10, section_y + 4, w - 20, s)

        # Epoch progress bar (bottom)
        bar_h = 6
        bar_y = h - bar_h - 8
        bar_x = 10
        bar_w = w - 20
        progress = (s._committed % s._checkpoint_interval) / max(s._checkpoint_interval, 1) if s._committed > 0 else 0.0

        bg_path = QPainterPath()
        bg_path.addRoundedRect(QRectF(bar_x, bar_y, bar_w, bar_h), 3, 3)
        p.fillPath(bg_path, QColor(255, 255, 255, 20))

        if progress > 0:
            fill_path = QPainterPath()
            fill_path.addRoundedRect(QRectF(bar_x, bar_y, bar_w * progress, bar_h), 3, 3)
            grad = QLinearGradient(bar_x, bar_y, bar_x + bar_w * progress, bar_y)
            grad.setColorAt(0.0, QColor(C["accent"]))
            grad.setColorAt(1.0, QColor(C["green"]))
            p.fillPath(fill_path, QBrush(grad))

        p.setPen(QColor(C["text3"]))
        p.setFont(QFont("Segoe UI", 6))
        p.drawText(QRectF(bar_x, bar_y - 10, bar_w, 10), Qt.AlignRight,
                   f"Epoch {s._epoch} \u2014 {int(progress * 100)}%")

        p.end()

    def _draw_gsn_meta_section(self, p: QPainter, x, y, w, s) -> float:
        """Desenha secao GSN/META dentro do ExecutionPanel. Retorna y final."""
        line_h = 13
        cy = y
        meta = s.meta_stream
        gsn_val = s.gsn

        # Header
        p.setPen(QColor("#A855F7"))
        p.setFont(QFont("Segoe UI", 8, QFont.Bold))
        p.drawText(QRectF(x, cy, w, line_h), Qt.AlignLeft | Qt.AlignVCenter, "GSN Sequencer")
        cy += line_h

        # GSN counter
        seq_id = 0
        if s.groups and s.groups[0].members:
            seq_id = min(s.groups[0].members)
        p.setPen(QColor(C["text"]))
        p.setFont(QFont("Consolas", 7))
        p.drawText(QRectF(x, cy, w, line_h), Qt.AlignLeft | Qt.AlignVCenter,
                   f"GSN={gsn_val}  Node {seq_id}")
        cy += line_h

        # META entries (last 3)
        visible_meta = meta[-3:] if meta else []
        if visible_meta:
            p.setPen(QColor("#D500F9"))
            p.setFont(QFont("Segoe UI", 7, QFont.Bold))
            p.drawText(QRectF(x, cy, w, line_h), Qt.AlignLeft | Qt.AlignVCenter, "META Stream:")
            cy += line_h
            for entry in reversed(visible_meta):
                grps = ",".join(str(g) for g in entry["groups"])
                is_latest = (entry == visible_meta[-1])
                p.setPen(QColor("#D500F9") if is_latest else QColor(C["text2"]))
                p.setFont(QFont("Consolas", 7, QFont.Bold if is_latest else QFont.Normal))
                p.drawText(QRectF(x + 4, cy, w - 4, line_h), Qt.AlignLeft | Qt.AlignVCenter,
                           f"GSN={entry['gsn']} -> G[{grps}] N{entry['published_by']}")
                cy += line_h
        return cy

    def _draw_adeliver_section(self, p: QPainter, x, y, w, s) -> float:
        """Desenha secao ADeliver dentro do ExecutionPanel. Retorna y final."""
        line_h = 13
        cy = y
        dlv = s.delivery
        if not dlv:
            return cy
        groups_with_data = sorted(dlv._last_delivered_gsn.keys())
        if not groups_with_data:
            return cy

        p.setPen(QColor(C["green"]))
        p.setFont(QFont("Segoe UI", 8, QFont.Bold))
        p.drawText(QRectF(x, cy, w, line_h), Qt.AlignLeft | Qt.AlignVCenter, "ADeliver")
        cy += line_h

        for gid in groups_with_data:
            next_gsn = dlv.get_next_expected_gsn(gid)
            blocked = dlv.get_blocked_entries(gid)
            last = dlv._last_delivered_gsn.get(gid, 0)
            txt = f"G{gid}: last={last} next={next_gsn}"
            if blocked:
                txt += f" BLOQ:{len(blocked)}"
                p.setPen(QColor(C["red"]))
            else:
                p.setPen(QColor(C["text"]))
            p.setFont(QFont("Consolas", 7))
            p.drawText(QRectF(x + 4, cy, w - 4, line_h), Qt.AlignLeft | Qt.AlignVCenter, txt)
            cy += line_h
        return cy



    def _draw_metric_badge(self, p: QPainter, x, y, w, h, label, value, color):
        """Desenha um badge de métrica com fundo glass."""
        path = QPainterPath()
        path.addRoundedRect(QRectF(x, y, w, h), 6, 6)
        p.fillPath(path, QColor(28, 46, 74, 100))
        p.setPen(QPen(QColor(color).darker(140), 1))
        p.setBrush(Qt.NoBrush)
        p.drawPath(path)

        # Value
        p.setPen(QColor(color))
        p.setFont(QFont("Segoe UI", 11, QFont.Bold))
        p.drawText(QRectF(x, y + 2, w, h * 0.6), Qt.AlignCenter, value)

        # Label
        p.setPen(QColor(C["text3"]))
        p.setFont(QFont("Segoe UI", 6))
        p.drawText(QRectF(x, y + h * 0.55, w, h * 0.4), Qt.AlignCenter, label)

    def _draw_pipeline_bar(self, p: QPainter, x, y, w, h, req: RequestInfo):
        """Desenha uma barra de pipeline mostrando a fase atual da request."""
        num_phases = len(self._PHASE_ORDER)
        # Determine current phase index
        phase = req.phase or Phase.CLIENT_SEND
        cur_idx = 0
        for i, ph in enumerate(self._PHASE_ORDER):
            if ph == phase:
                cur_idx = i
                break

        # Background bar
        bg = QPainterPath()
        bg.addRoundedRect(QRectF(x, y, w, h), 4, 4)
        p.fillPath(bg, QColor(255, 255, 255, 12))

        # Filled portion (progress)
        fill_w = w * (cur_idx + 1) / num_phases
        fill = QPainterPath()
        fill.addRoundedRect(QRectF(x, y, fill_w, h), 4, 4)
        color = QColor(self._PHASE_COLORS.get(phase, C["text3"]))
        grad = QLinearGradient(x, y, x + fill_w, y)
        c1 = QColor(color)
        c1.setAlpha(140)
        c2 = QColor(color)
        c2.setAlpha(80)
        grad.setColorAt(0.0, c1)
        grad.setColorAt(1.0, c2)
        p.fillPath(fill, QBrush(grad))

        # Border
        p.setPen(QPen(QColor(color.red(), color.green(), color.blue(), 100), 1))
        p.setBrush(Qt.NoBrush)
        p.drawPath(bg)

        # Request label (left)
        p.setPen(QColor(C["text"]))
        p.setFont(QFont("Consolas", 7, QFont.Bold))
        lbl = f"SN{req.sn}"
        p.drawText(QRectF(x + 4, y, 40, h), Qt.AlignVCenter | Qt.AlignLeft, lbl)

        # Phase name (center)
        p.setPen(QColor(255, 255, 255, 200))
        p.setFont(QFont("Segoe UI", 7))
        phase_name = phase.name.replace("_", " ").title()
        p.drawText(QRectF(x + 44, y, w - 88, h), Qt.AlignVCenter | Qt.AlignCenter, phase_name)

        # Leader (right)
        p.setPen(QColor(C["text2"]))
        p.setFont(QFont("Segoe UI", 6))
        p.drawText(QRectF(x + w - 44, y, 40, h), Qt.AlignVCenter | Qt.AlignRight, f"N{req.leader}")


class CommitChainPanel(QWidget):
    """Panorama visual dos commits — cadeia de blocos mostrando o encadeamento."""

    def __init__(self, sim: Simulation, parent=None):
        super().__init__(parent)
        self.sim = sim
        self.setAttribute(Qt.WA_TranslucentBackground)
        self.setSizePolicy(QSizePolicy.Expanding, QSizePolicy.Expanding)
        self.setMinimumHeight(90)
        self.setMouseTracking(True)
        self._scroll_offset = 0.0  # horizontal scroll in pixels
        self._dragging = False
        self._drag_start_x = 0.0
        self._drag_start_offset = 0.0
        self._timer = QTimer(self)
        self._timer.setInterval(120)
        self._timer.timeout.connect(self.update)
        self._timer.start()

    def wheelEvent(self, event):
        """Scroll horizontal com roda do mouse."""
        delta = event.angleDelta().y() or event.angleDelta().x()
        self._scroll_offset += delta * 0.5
        self._clamp_scroll()
        self.update()
        event.accept()

    def mousePressEvent(self, event):
        if event.button() == Qt.LeftButton:
            self._dragging = True
            self._drag_start_x = event.position().x()
            self._drag_start_offset = self._scroll_offset
            self.setCursor(Qt.ClosedHandCursor)
        super().mousePressEvent(event)

    def mouseMoveEvent(self, event):
        if self._dragging:
            dx = event.position().x() - self._drag_start_x
            self._scroll_offset = self._drag_start_offset + dx
            self._clamp_scroll()
            self.update()
        super().mouseMoveEvent(event)

    def mouseReleaseEvent(self, event):
        if event.button() == Qt.LeftButton:
            self._dragging = False
            self.setCursor(Qt.ArrowCursor)
        super().mouseReleaseEvent(event)

    def _clamp_scroll(self):
        """Limita scroll ao conteúdo."""
        history = self.sim.commit_history
        if not history:
            self._scroll_offset = 0
            return
        block_w = 52
        gap = 6
        arrow_w = 12
        cell_w = block_w + gap + arrow_w
        total_content_w = len(history) * cell_w + 10
        visible_w = self.width()
        max_scroll = 0
        min_scroll = min(0, visible_w - total_content_w)
        self._scroll_offset = max(min_scroll, min(max_scroll, self._scroll_offset))

    def paintEvent(self, event):
        p = QPainter(self)
        p.setRenderHint(QPainter.Antialiasing)
        w, h = self.width(), self.height()

        # Background
        bg = QPainterPath()
        bg.addRoundedRect(QRectF(0, 0, w, h), 10, 10)
        p.fillPath(bg, QColor(28, 46, 74, 100))
        p.setPen(QPen(QColor(255, 255, 255, 25), 1))
        p.drawPath(bg)

        # Title
        p.setPen(QColor(C["green"]))
        p.setFont(QFont("Segoe UI", 9, QFont.Bold))
        p.drawText(QRectF(10, 4, 300, 16), Qt.AlignLeft, "⛓ Cadeia de Commits (Log Replicado)")

        history = self.sim.commit_history
        if not history:
            p.setPen(QColor(C["text3"]))
            p.setFont(QFont("Segoe UI", 8))
            p.drawText(QRectF(10, 24, w - 20, h - 28), Qt.AlignCenter, "Nenhum commit ainda...")
            p.end()
            return

        # Layout: blocks in a chain
        top_y = 24
        block_h = h - top_y - 10
        block_w = 52
        gap = 6
        arrow_w = 12
        cell_w = block_w + gap + arrow_w

        # Draw all blocks with scroll offset
        start_x = 10 + self._scroll_offset

        leader_colors = ["#F97316", "#6C63FF", "#34D399", "#EF4444", "#A855F7",
                         "#58D8FF", "#FACC15", "#8B7DFF"]

        for i, entry in enumerate(history):
            x = start_x + i * cell_w
            if x + block_w < 0 or x > w:
                continue
            lc = QColor(leader_colors[entry["leader"] % len(leader_colors)])
            self._draw_commit_block(p, x, top_y, block_w, block_h, entry, lc)
            if i < len(history) - 1:
                self._draw_chain_arrow(p, x + block_w + 2, top_y + block_h / 2, arrow_w, w)

        # Summary on the right edge
        p.setPen(QColor(C["text2"]))
        p.setFont(QFont("Segoe UI", 7))
        p.drawText(QRectF(w - 80, 4, 70, 16), Qt.AlignRight,
                   f"Total: {len(history)} | E{history[-1]['epoch']}")

        # Flash effect on latest block when commit event is active
        for ev in (self.sim.visual_events if hasattr(self.sim, 'visual_events') else []):
            if ev["type"] == "commit":
                alpha = min(180, ev["ttl"] * 6)
                # Glow on the right edge (latest commit area)
                glow_x = start_x + (len(history) - 1) * cell_w
                if 0 < glow_x < w:
                    glow_c = QColor(C["green"])
                    glow_c.setAlpha(alpha)
                    glow_rect = QRectF(glow_x - 4, top_y - 4, block_w + 8, block_h + 8)
                    glow_path = QPainterPath()
                    glow_path.addRoundedRect(glow_rect, 8, 8)
                    p.setPen(QPen(glow_c, 2.5))
                    p.setBrush(Qt.NoBrush)
                    p.drawPath(glow_path)
                break  # only one flash at a time

        p.end()

    def _draw_commit_block(self, p: QPainter, x, y, block_w, block_h, entry, lc: QColor):
        """Desenha um bloco individual da cadeia de commits."""
        block_path = QPainterPath()
        block_path.addRoundedRect(QRectF(x, y, block_w, block_h), 6, 6)

        grad = QLinearGradient(x, y, x, y + block_h)
        c1 = QColor(lc)
        c1.setAlpha(50)
        c2 = QColor(lc)
        c2.setAlpha(20)
        grad.setColorAt(0.0, c1)
        grad.setColorAt(1.0, c2)
        p.fillPath(block_path, QBrush(grad))

        border_c = QColor(lc)
        border_c.setAlpha(140)
        p.setPen(QPen(border_c, 1.2))
        p.setBrush(Qt.NoBrush)
        p.drawPath(block_path)

        if entry["is_cross"]:
            p.setPen(Qt.NoPen)
            p.setBrush(QColor("#D500F9"))
            p.drawEllipse(QPointF(x + block_w - 6, y + 6), 3, 3)

        p.setPen(QColor(C["text"]))
        p.setFont(QFont("Consolas", 9, QFont.Bold))
        p.drawText(QRectF(x, y + 2, block_w, 16), Qt.AlignCenter, f"SN{entry['sn']}")

        p.setPen(lc)
        p.setFont(QFont("Segoe UI", 6))
        p.drawText(QRectF(x, y + 17, block_w, 10), Qt.AlignCenter, f"N{entry['leader']}")

        p.setPen(QColor(C["text3"]))
        p.setFont(QFont("Consolas", 6))
        p.drawText(QRectF(x, y + 28, block_w, 10), Qt.AlignCenter, entry["hash"])

        if block_h > 48:
            p.setPen(QColor(C["text3"]))
            p.setFont(QFont("Segoe UI", 6))
            p.drawText(QRectF(x, y + block_h - 12, block_w, 10), Qt.AlignCenter, f"E{entry['epoch']}")

    def _draw_chain_arrow(self, p: QPainter, ax, ay, arrow_w, max_w):
        """Desenha seta de conexão entre blocos."""
        if ax >= max_w:
            return
        p.setPen(QPen(QColor(C["text3"]), 1.5))
        p.drawLine(QPointF(ax, ay), QPointF(ax + arrow_w - 4, ay))
        p.setBrush(QColor(C["text3"]))
        p.setPen(Qt.NoPen)
        arrow_path = QPainterPath()
        arrow_path.moveTo(ax + arrow_w - 4, ay - 3)
        arrow_path.lineTo(ax + arrow_w, ay)
        arrow_path.lineTo(ax + arrow_w - 4, ay + 3)
        arrow_path.closeSubpath()
        p.fillPath(arrow_path, QColor(C["text3"]))


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


class GlobalOrderPanel(QWidget):
    """Visualizacao da Ordem Global Atomica — timeline com setas e efeitos visuais."""

    _GROUP_COLORS = ["#EF4444", "#6C63FF", "#34D399", "#F97316", "#A855F7", "#58D8FF"]

    def __init__(self, sim: Simulation, parent=None):
        super().__init__(parent)
        self.sim = sim
        self.setAttribute(Qt.WA_TranslucentBackground)
        self.setSizePolicy(QSizePolicy.Expanding, QSizePolicy.Expanding)
        self.setMinimumHeight(100)
        self.setMouseTracking(True)
        self._event_rects: list[tuple[QRectF, dict]] = []  # hit areas
        self._popup: dict | None = None  # {"entry": ..., "pos": QPointF}
        self._timer = QTimer(self)
        self._timer.setInterval(120)
        self._timer.timeout.connect(self.update)
        self._timer.start()

    def mousePressEvent(self, event):
        if event.button() == Qt.LeftButton:
            pos = event.position()
            clicked = None
            for rect, entry in self._event_rects:
                if rect.contains(pos):
                    clicked = entry
                    break
            if clicked:
                self._popup = {"entry": clicked, "pos": pos}
            else:
                self._popup = None
            self.update()
        super().mousePressEvent(event)

    def mouseMoveEvent(self, event):
        pos = event.position()
        hovering = any(r.contains(pos) for r, _ in self._event_rects)
        self.setCursor(Qt.PointingHandCursor if hovering else Qt.ArrowCursor)
        super().mouseMoveEvent(event)

    def paintEvent(self, event):
        p = QPainter(self)
        p.setRenderHint(QPainter.Antialiasing)
        w, h = self.width(), self.height()
        s = self.sim

        # Background
        bg = QPainterPath()
        bg.addRoundedRect(QRectF(0, 0, w, h), 10, 10)
        p.fillPath(bg, QColor(28, 46, 74, 100))
        p.setPen(QPen(QColor(255, 255, 255, 25), 1))
        p.drawPath(bg)

        # Title
        p.setPen(QColor("#A855F7"))
        p.setFont(QFont("Segoe UI", 9, QFont.Bold))
        p.drawText(QRectF(10, 4, 300, 16), Qt.AlignLeft, "Ordem Global Atomica (ADeliver)")

        history = s.delivery.delivery_history if s.delivery else []
        if not history:
            p.setPen(QColor(C["text3"]))
            p.setFont(QFont("Segoe UI", 8))
            p.drawText(QRectF(10, 24, w - 20, h - 28), Qt.AlignCenter,
                       "Aguardando entregas...")
            p.end()
            return

        # Draw group lanes with gradient
        # G0 (sequenciador) aparece como lane de publicacao META
        # G1, G2... aparecem como lanes de entrega
        top_y = 24
        groups = s.groups
        lane_h = max(16, min(26, (h - top_y - 24) / max(len(groups), 1)))
        lanes_bottom = top_y + len(groups) * lane_h

        for i, grp in enumerate(groups):
            ly = top_y + i * lane_h
            gc = QColor(self._GROUP_COLORS[i % len(self._GROUP_COLORS)])
            is_seq = (grp.id == 0)

            # Lane gradient background
            lane_rect = QRectF(54, ly + 1, w - 64, lane_h - 2)
            lane_grad = QLinearGradient(54, ly, w - 10, ly)
            c1 = QColor(gc)
            c1.setAlpha(12 if is_seq else 20)
            c2 = QColor(gc)
            c2.setAlpha(5 if is_seq else 8)
            lane_grad.setColorAt(0.0, c1)
            lane_grad.setColorAt(1.0, c2)
            lane_path = QPainterPath()
            lane_path.addRoundedRect(lane_rect, 3, 3)
            p.fillPath(lane_path, QBrush(lane_grad))

            # Lane border (subtle, dashed for sequencer)
            bc = QColor(gc)
            bc.setAlpha(40)
            pen_style = Qt.DashLine if is_seq else Qt.SolidLine
            p.setPen(QPen(bc, 0.5, pen_style))
            p.setBrush(Qt.NoBrush)
            p.drawPath(lane_path)

            # Lane label
            p.setPen(gc)
            p.setFont(QFont("Segoe UI", 7, QFont.Bold))
            label = "SEQ" if is_seq else f"G{grp.id}"
            p.drawText(QRectF(4, ly, 48, lane_h), Qt.AlignVCenter | Qt.AlignRight, label)

        # Timeline arrow at bottom (horizontal)
        arrow_y = lanes_bottom + 4
        p.setPen(QPen(QColor(C["text3"]), 1.5))
        p.drawLine(QPointF(54, arrow_y), QPointF(w - 16, arrow_y))
        # Arrowhead
        p.setPen(Qt.NoPen)
        p.setBrush(QColor(C["text3"]))
        arr = QPainterPath()
        arr.moveTo(w - 16, arrow_y)
        arr.lineTo(w - 22, arrow_y - 3)
        arr.lineTo(w - 22, arrow_y + 3)
        arr.closeSubpath()
        p.fillPath(arr, QColor(C["text3"]))
        # "tempo" label
        p.setPen(QColor(C["text3"]))
        p.setFont(QFont("Segoe UI", 6))
        p.drawText(QRectF(w - 50, arrow_y + 2, 40, 10), Qt.AlignRight, "tempo")

        # Draw delivery events
        visible = history[-16:]
        event_w = max(22, min(40, (w - 74) / max(len(visible), 1)))
        start_x = 60
        self._event_rects = []

        for idx, entry in enumerate(visible):
            ex = start_x + idx * event_w
            if ex > w - 20:
                break
            cx = ex + event_w / 2

            # Store hit rect for click detection
            hit_rect = QRectF(ex, top_y - 14, event_w, lanes_bottom - top_y + 20)
            self._event_rects.append((hit_rect, entry))

            gsn = entry["gsn"]
            touched_groups = entry["groups"]
            is_cross = entry["type"] == "cross"
            is_last = (idx == len(visible) - 1)
            color = QColor("#D500F9") if is_cross else QColor(C["green"])

            # Find lanes touched (all groups including G0 for META publications)
            lane_indices = []
            for gi, grp in enumerate(groups):
                if grp.id in touched_groups:
                    lane_indices.append(gi)

            # For cross-group ops, G0 (sequencer) is always involved as publisher
            if is_cross and 0 not in lane_indices:
                lane_indices.insert(0, 0)  # SEQ lane

            if not lane_indices:
                continue

            min_li = min(lane_indices)
            max_li = max(lane_indices)
            y1 = top_y + min_li * lane_h + lane_h / 2
            y2 = top_y + max_li * lane_h + lane_h / 2

            # Glow effect on latest event
            if is_last:
                glow_y = (y1 + y2) / 2
                glow = QRadialGradient(QPointF(cx, glow_y), 18)
                gc = QColor(color)
                gc.setAlpha(60)
                glow.setColorAt(0.0, gc)
                glow.setColorAt(1.0, QColor(0, 0, 0, 0))
                p.setPen(Qt.NoPen)
                p.setBrush(QBrush(glow))
                p.drawEllipse(QPointF(cx, glow_y), 18, 18)

            # Vertical connector with arrow for cross-group
            if is_cross and min_li != max_li:
                # Dashed glow line
                pen = QPen(color, 2.5)
                p.setPen(pen)
                p.drawLine(QPointF(cx, y1), QPointF(cx, y2))

                # Arrowheads at each end (bidirectional)
                p.setPen(Qt.NoPen)
                p.setBrush(color)
                # Down arrow
                da = QPainterPath()
                da.moveTo(cx, y2 + 4)
                da.lineTo(cx - 3, y2 - 1)
                da.lineTo(cx + 3, y2 - 1)
                da.closeSubpath()
                p.fillPath(da, color)
                # Up arrow
                ua = QPainterPath()
                ua.moveTo(cx, y1 - 4)
                ua.lineTo(cx - 3, y1 + 1)
                ua.lineTo(cx + 3, y1 + 1)
                ua.closeSubpath()
                p.fillPath(ua, color)

            # Dots on each touched lane
            for li in lane_indices:
                dy = top_y + li * lane_h + lane_h / 2
                is_seq_lane = (li == 0)  # G0 = sequencer
                if is_seq_lane:
                    # Diamante para SEQ (publicou GSN, nao entregou)
                    diamond = QPainterPath()
                    diamond.moveTo(cx, dy - 6)
                    diamond.lineTo(cx + 5, dy)
                    diamond.lineTo(cx, dy + 6)
                    diamond.lineTo(cx - 5, dy)
                    diamond.closeSubpath()
                    p.setPen(QPen(color, 1.5))
                    p.setBrush(Qt.NoBrush)
                    p.drawPath(diamond)
                    # Small filled center
                    p.setPen(Qt.NoPen)
                    p.setBrush(color)
                    p.drawEllipse(QPointF(cx, dy), 2, 2)
                else:
                    # Circulo para grupos de dados (entrega ADeliver)
                    ring_c = QColor(color)
                    ring_c.setAlpha(80)
                    p.setPen(QPen(ring_c, 1.5))
                    p.setBrush(Qt.NoBrush)
                    p.drawEllipse(QPointF(cx, dy), 7, 7)
                    # Inner filled dot
                    p.setPen(Qt.NoPen)
                    p.setBrush(color)
                    p.drawEllipse(QPointF(cx, dy), 4, 4)

            # Horizontal order arrow between events
            if idx < len(visible) - 1:
                next_cx = start_x + (idx + 1) * event_w + event_w / 2
                mid_y = arrow_y
                arr_color = QColor(C["text3"])
                arr_color.setAlpha(100)
                p.setPen(QPen(arr_color, 1))
                p.drawLine(QPointF(cx + 8, mid_y), QPointF(next_cx - 8, mid_y))
                # Small arrowhead
                p.setPen(Qt.NoPen)
                p.setBrush(arr_color)
                sa = QPainterPath()
                sa.moveTo(next_cx - 8, mid_y)
                sa.lineTo(next_cx - 12, mid_y - 2)
                sa.lineTo(next_cx - 12, mid_y + 2)
                sa.closeSubpath()
                p.fillPath(sa, arr_color)

            # GSN label above lanes
            p.setPen(color)
            p.setFont(QFont("Consolas", 7, QFont.Bold))
            lbl = f"{gsn}" if is_cross else "s"
            p.drawText(QRectF(ex, top_y - 14, event_w, 12), Qt.AlignCenter, lbl)

            # SN label below timeline
            p.setPen(QColor(C["text3"]))
            p.setFont(QFont("Consolas", 6))
            p.drawText(QRectF(ex, arrow_y + 2, event_w, 10), Qt.AlignCenter,
                       f"sn{entry['sn']}")

        # Legend
        legend_y = h - 14
        p.setFont(QFont("Segoe UI", 6))
        # Cross indicator
        p.setPen(QColor("#D500F9"))
        p.drawText(QRectF(10, legend_y, 80, 12), Qt.AlignLeft, "# = cross-op")
        # Single indicator
        p.setPen(QColor(C["green"]))
        p.drawText(QRectF(80, legend_y, 80, 12), Qt.AlignLeft, "s = single-op")
        # Diamond = SEQ published
        p.setPen(QColor(C["text3"]))
        p.drawText(QRectF(150, legend_y, 100, 12), Qt.AlignLeft, "<> = SEQ publica GSN")
        # Arrow meaning
        p.setPen(QColor(C["text3"]))
        p.drawText(QRectF(260, legend_y, 80, 12), Qt.AlignLeft, "-> = ordem")

        # Blocked count
        blocked_count = 0
        if s.delivery:
            for gid in s.delivery._last_delivered_gsn:
                blocked_count += len(s.delivery.get_blocked_entries(gid))
        if blocked_count > 0:
            p.setPen(QColor(C["red"]))
            p.setFont(QFont("Segoe UI", 7, QFont.Bold))
            p.drawText(QRectF(w - 110, 4, 100, 16), Qt.AlignRight,
                       f"BLOQUEADOS: {blocked_count}")

        # Popup de detalhes ao clicar num evento
        if self._popup:
            self._draw_event_popup(p, w, h)

        p.end()

    def _draw_event_popup(self, p: QPainter, w: float, h: float):
        """Desenha popup com detalhes do evento clicado."""
        entry = self._popup["entry"]
        pos = self._popup["pos"]

        is_cross = entry["type"] == "cross"
        gsn = entry["gsn"]
        sn = entry["sn"]
        groups = entry["groups"]
        color = QColor("#D500F9") if is_cross else QColor(C["green"])

        lines = [
            f"{'Cross-Group Op' if is_cross else 'Single-Group Op'}",
            f"SN: {sn}",
            f"GSN: {gsn}" if gsn > 0 else "GSN: (nenhum)",
            f"Tipo: {entry['type']}",
            f"Grupos: {groups}",
        ]
        if is_cross:
            lines.append(f"Coordenacao: SEQ publica GSN={gsn}")
            lines.append(f"Todos os grupos entregam na ordem GSN")
        else:
            lines.append(f"Entrega direta no grupo G{groups[0] if groups else '?'}")

        # Popup dimensions
        p.setFont(QFont("Segoe UI", 8))
        fm = p.fontMetrics()
        line_h = fm.height() + 2
        popup_w = 200
        popup_h = line_h * len(lines) + 14

        px = min(pos.x() + 10, w - popup_w - 6)
        py = max(6, pos.y() - popup_h - 10)
        if py < 6:
            py = pos.y() + 16

        rect = QRectF(px, py, popup_w, popup_h)
        bg = QPainterPath()
        bg.addRoundedRect(rect, 8, 8)
        p.fillPath(bg, QColor(10, 18, 32, 240))
        p.setPen(QPen(color, 1.5))
        p.setBrush(Qt.NoBrush)
        p.drawPath(bg)

        y = py + 8
        for i, line in enumerate(lines):
            if i == 0:
                p.setPen(color)
                p.setFont(QFont("Segoe UI", 9, QFont.Bold))
            else:
                p.setPen(QColor(C["text"]))
                p.setFont(QFont("Consolas", 7))
            p.drawText(QRectF(px + 10, y, popup_w - 20, line_h),
                       Qt.AlignLeft | Qt.AlignVCenter, line)
            y += line_h
