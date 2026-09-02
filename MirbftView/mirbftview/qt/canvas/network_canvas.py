"""NetworkCanvas — classe principal do grafo da rede."""

import math
from PySide6.QtWidgets import QWidget, QTextEdit
from PySide6.QtCore import Qt, QPointF, QRectF, QTimer
from PySide6.QtGui import QPainter, QPainterPath, QColor, QPen, QFont

from mirbftview.qt.theme import C
from mirbftview.qt.simulation import Simulation, Phase, MsgType
from mirbftview.qt.canvas._constants import MSG_COLORS
from mirbftview.qt.canvas.draw_nodes import (
    draw_groups, draw_connections, draw_nodes, draw_clients,
    draw_sequencer_glow, draw_meta_broadcast_lines,
)
from mirbftview.qt.canvas.draw_messages import (
    assign_msg_colors, get_active_msg_color, draw_messages,
)
from mirbftview.qt.canvas.draw_hud import (
    draw_gsn_meta_panel, draw_adeliver_panel,
)
from mirbftview.qt.canvas.thought_bubbles import (
    update_thought_bubbles, draw_thought_bubbles,
)


class NetworkCanvas(QWidget):
    """Grafo da rede — nós, mensagens, interação. Arraste os nós!"""

    def __init__(self, sim: Simulation, parent=None):
        super().__init__(parent)
        self.sim = sim
        self.setAttribute(Qt.WA_TranslucentBackground)
        self.setMouseTracking(True)
        self._node_pos: dict[int, QPointF] = {}
        self._client_pos: dict[int, QPointF] = {}
        self._dragging: str | None = None
        self._drag_offset = QPointF(0, 0)
        self._drag_moved = False
        self._zoom = 1.0
        self._pan_offset = QPointF(0, 0)
        self._panning = False
        self._pan_start = QPointF(0, 0)
        self._popup_widget = self._make_popup_widget()
        self._thought_bubbles: dict[int, dict] = {}
        self._color_pool_idx = 0
        self._known_msgs: set = set()
        self._timer = QTimer(self)
        self._timer.setInterval(16)
        self._timer.timeout.connect(self._on_tick)
        self._timer.start()

    # ─── Tick ─────────────────────────────────────────────────────────────

    def _on_tick(self):
        update_thought_bubbles(self.sim, self._thought_bubbles)
        self._color_pool_idx = assign_msg_colors(
            self.sim, self._color_pool_idx, self._known_msgs
        )
        self.sim.tick()
        self.update()

    # ─── Popup de inspeção (widget real, texto selecionável) ────────────────

    # Cores no estilo do painel "Variables" do debugger do VSCode.
    _DBG_KEY_COLOR = "#9CDCFE"
    _DBG_VAL_COLOR = "#D4D4D4"
    _DBG_HEADER_COLOR = "#6A9955"

    def _make_popup_widget(self) -> QTextEdit:
        """Popup no estilo do painel 'Variables' do debugger do VSCode: nome
        da variável e valor em cores diferentes (discretas), uma única fonte
        monoespaçada e tamanho para tudo, sem negrito nem caixas internas.
        Texto selecionável e copiável com Ctrl+C — ao contrário do antigo
        popup pintado com QPainter, que não permitia selecionar nem copiar."""
        w = QTextEdit(self)
        w.setReadOnly(True)
        w.setTextInteractionFlags(Qt.TextSelectableByMouse | Qt.TextSelectableByKeyboard)
        w.setFont(QFont("Consolas", 9))
        w.setStyleSheet(
            "QTextEdit { background: rgba(12,20,38,240); color: #D4D4D4; "
            "border: 1px solid rgba(255,255,255,0.15); border-radius: 4px; "
            "padding: 4px; selection-background-color: rgba(88,216,255,0.35); }"
        )
        w.setLineWrapMode(QTextEdit.NoWrap)
        w.setVerticalScrollBarPolicy(Qt.ScrollBarAlwaysOff)
        w.setHorizontalScrollBarPolicy(Qt.ScrollBarAlwaysOff)
        w.hide()
        return w

    def _lines_to_debug_html(self, lines: list[str]) -> str:
        """Renderiza cada linha como 'nome: valor', coloridas como no painel
        Variables do VSCode (nome em azul claro, valor em cinza claro),
        preservando indentação. Linhas de título (--- Algo ---) ficam num
        verde discreto, sem negrito, mesmo tamanho de fonte que o resto."""
        import html as html_mod
        rows = []
        for line in lines:
            stripped = line.strip()
            indent = len(line) - len(line.lstrip(" "))
            pad = "&nbsp;" * indent
            is_header = stripped.startswith("--") or stripped.startswith("─")
            if is_header or ":" not in stripped:
                color = self._DBG_HEADER_COLOR if is_header else self._DBG_VAL_COLOR
                rows.append(f'<div style="color:{color};">{pad}{html_mod.escape(stripped)}</div>')
                continue
            key, _, val = stripped.partition(":")
            rows.append(
                f'<div>{pad}<span style="color:{self._DBG_KEY_COLOR};">{html_mod.escape(key.rstrip())}</span>'
                f'<span style="color:{self._DBG_VAL_COLOR};">:{html_mod.escape(val)}</span></div>'
            )
        return "".join(rows)

    def _show_debug_popup(self, lines: list[str], pos: QPointF):
        body = self._lines_to_debug_html(lines)
        self._popup_widget.setHtml(f'<div style="font-family:Consolas; font-size:9pt; white-space:pre;">{body}</div>')

        w_canvas, h_canvas = self.width(), self.height()

        # Mede o tamanho REAL do conteúdo renderizado (HTML, não texto puro
        # via fontMetrics — a marcação/indentação em &nbsp; renderiza um
        # pouco diferente e o cálculo por fontMetrics ficava subestimado,
        # fazendo a barra de rolagem aparecer mesmo cabendo tudo).
        doc = self._popup_widget.document()
        doc.setTextWidth(-1)
        ideal_w = doc.idealWidth()
        popup_w = min(ideal_w + 24, w_canvas - 20)
        doc.setTextWidth(popup_w - 12)
        popup_h = min(doc.size().height() + 16, h_canvas - 20)
        px = pos.x() + 15
        py = pos.y() - popup_h / 2
        if px + popup_w > w_canvas - 10:
            px = pos.x() - popup_w - 15
        if py < 10:
            py = 10
        if py + popup_h > h_canvas - 10:
            py = h_canvas - popup_h - 10

        self._popup_widget.setGeometry(int(px), int(py), int(popup_w), int(popup_h))
        self._popup_widget.show()
        self._popup_widget.raise_()

    def _hide_debug_popup(self):
        self._popup_widget.hide()

    # ─── Geometry ─────────────────────────────────────────────────────────

    def _compute_positions(self):
        w, h = self.width(), self.height()
        cx, cy = w * 0.55, h * 0.5
        n = len(self.sim.nodes)
        radius = min(w, h) * 0.28
        for i, node in enumerate(self.sim.nodes):
            angle = (2 * math.pi * i / n) - math.pi / 2
            self._node_pos[node.id] = QPointF(cx + radius * math.cos(angle), cy + radius * math.sin(angle))
        for i, client in enumerate(self.sim.clients):
            self._client_pos[client.id] = QPointF(60, cy - 50 + i * 100)

    def resizeEvent(self, event):
        super().resizeEvent(event)
        if not self._node_pos:
            self._compute_positions()

    def _to_scene(self, screen_pos: QPointF) -> QPointF:
        return (screen_pos - self._pan_offset) / self._zoom

    # ─── Hit testing ──────────────────────────────────────────────────────

    def _hit_test(self, pos: QPointF) -> str | None:
        scene_pos = self._to_scene(pos)
        for nid, npos in self._node_pos.items():
            if (scene_pos - npos).manhattanLength() < 38:
                return f"node:{nid}"
        for cid, cpos in self._client_pos.items():
            if (scene_pos - cpos).manhattanLength() < 26:
                return f"client:{cid}"
        return None

    def _hit_test_message(self, pos: QPointF):
        scene_pos = self._to_scene(pos)
        best_msg, best_dist = None, 20 / self._zoom
        for msg in self.sim.messages:
            src = self._client_pos.get(msg.from_id) if msg.from_is_client else self._node_pos.get(msg.from_id)
            dst = self._client_pos.get(msg.to_id) if msg.to_is_client else self._node_pos.get(msg.to_id)
            if src is None or dst is None:
                continue
            t = msg.progress
            mx = src.x() + (dst.x() - src.x()) * t
            my = src.y() + (dst.y() - src.y()) * t
            d = math.hypot(scene_pos.x() - mx, scene_pos.y() - my)
            if d < best_dist:
                best_dist = d
                best_msg = msg
        return best_msg

    def _gsn_panel_rect(self) -> QRectF:
        """Retângulo do painel 'GSN Sequencer' (mesma fórmula de draw_hud.py
        draw_gsn_meta_panel), usado para hit-test do clique."""
        meta = self.sim.meta_stream
        visible_meta = meta[-5:] if meta else []
        n_entries = max(len(visible_meta), 1)
        box_w, box_x, box_y, line_h = 220, 10, 10, 16
        box_h = line_h * 3 + n_entries * 22 + 16
        return QRectF(box_x, box_y, box_w, box_h)

    def _adeliver_panel_rect(self) -> QRectF:
        """Retângulo do painel 'ADeliver (por grupo)' (mesma fórmula de
        draw_hud.py draw_adeliver_panel), usado para hit-test do clique."""
        dlv = self.sim.delivery
        if not dlv or not dlv._last_delivered_gsn:
            return QRectF()
        groups_with_data = sorted(dlv._last_delivered_gsn.keys())

        meta = self.sim.meta_stream
        visible_meta = meta[-5:] if meta else []
        n_entries = max(len(visible_meta), 1)
        gsn_box_h = 16 * 3 + n_entries * 22 + 16

        box_w, box_x, line_h = 220, 10, 18
        box_y = 10 + gsn_box_h + 8
        box_h = line_h + len(groups_with_data) * 24 + 12
        return QRectF(box_x, box_y, box_w, box_h)

    def _is_over_config(self, pos: QPointF) -> bool:
        for child in self.children():
            if hasattr(child, 'isVisible') and child.isVisible() and hasattr(child, 'geometry'):
                if child.geometry().contains(pos.toPoint()):
                    return True
        return False

    # ─── Events ───────────────────────────────────────────────────────────

    def wheelEvent(self, event):
        if self._is_over_config(event.position()):
            event.ignore()
            return
        old_zoom = self._zoom
        delta = event.angleDelta().y()
        factor = 1.15 if delta > 0 else 1 / 1.15
        self._zoom = max(0.3, min(5.0, self._zoom * factor))
        cursor_pos = event.position()
        self._pan_offset = cursor_pos - (self._zoom / old_zoom) * (cursor_pos - self._pan_offset)
        self._hide_debug_popup()
        self.update()
        event.accept()

    def mousePressEvent(self, event):
        if self._is_over_config(event.position()):
            event.ignore()
            return
        if event.button() == Qt.LeftButton:
            self._drag_moved = False
            hit = self._hit_test(event.position())
            if hit:
                self._dragging = hit
                kind, id_str = hit.split(":")
                id_ = int(id_str)
                scene_pos = self._to_scene(event.position())
                if kind == "node":
                    self._drag_offset = self._node_pos[id_] - scene_pos
                else:
                    self._drag_offset = self._client_pos[id_] - scene_pos
                self.setCursor(Qt.ClosedHandCursor)
            elif self._gsn_panel_rect().contains(event.position()) or self._adeliver_panel_rect().contains(event.position()):
                self._dragging = None
            else:
                msg = self._hit_test_message(event.position())
                if not msg:
                    self._panning = True
                    self._pan_start = event.position()
                    self.setCursor(Qt.ClosedHandCursor)
                self._dragging = None
        super().mousePressEvent(event)

    def mouseMoveEvent(self, event):
        if not self._dragging and not self._panning and self._is_over_config(event.position()):
            self.setCursor(Qt.ArrowCursor)
            event.ignore()
            return
        if self._dragging:
            self._drag_moved = True
            kind, id_str = self._dragging.split(":")
            id_ = int(id_str)
            scene_pos = self._to_scene(event.position())
            new_pos = scene_pos + self._drag_offset
            if kind == "node":
                self._node_pos[id_] = new_pos
            else:
                self._client_pos[id_] = new_pos
            self._hide_debug_popup()
            self.update()
        elif self._panning:
            self._drag_moved = True
            delta = event.position() - self._pan_start
            self._pan_offset += delta
            self._pan_start = event.position()
            self._hide_debug_popup()
            self.update()
        else:
            hit = self._hit_test(event.position())
            msg = self._hit_test_message(event.position()) if not hit else None
            panel_hit = False
            if not (hit or msg):
                panel_hit = (self._gsn_panel_rect().contains(event.position())
                             or self._adeliver_panel_rect().contains(event.position()))
            self.setCursor(Qt.PointingHandCursor if (hit or msg or panel_hit) else Qt.ArrowCursor)
        super().mouseMoveEvent(event)

    def mouseReleaseEvent(self, event):
        if event.button() == Qt.LeftButton:
            if self._dragging and not self._drag_moved:
                self._open_inspect(self._dragging, event.position())
            elif not self._dragging and not self._panning:
                msg = self._hit_test_message(event.position())
                if msg:
                    self._open_msg_inspect(msg, event.position())
                elif self._gsn_panel_rect().contains(event.position()):
                    self._open_gsn_inspect(event.position())
                elif self._adeliver_panel_rect().contains(event.position()):
                    self._open_adeliver_inspect(event.position())
                else:
                    self._hide_debug_popup()
            elif self._panning and not self._drag_moved:
                self._hide_debug_popup()
            self._dragging = None
            self._panning = False
            self.setCursor(Qt.ArrowCursor)
        super().mouseReleaseEvent(event)

    # ─── Inspect popups ───────────────────────────────────────────────────

    def _open_gsn_inspect(self, pos):
        """Struct real do Sequenciador (orderer/sequencer.go), populada com
        os valores atuais da simulacao — mesmo espirito do popup de mensagem:
        a caixa 'GSN Sequencer' e uma metafora visual (fichas/senhas), o
        popup mostra os campos como eles existem de fato no codigo Go."""
        sim = self.sim
        members = sim.groups[0].members if sim.groups else []
        leader = min(members) if members else 0

        recent_meta = sim.meta_stream[-8:]
        metadata = "{" + ", ".join(f"{e['gsn']}: {e['groups']}" for e in recent_meta) + "}" if recent_meta else "{}"

        last_delivered = {}
        pending_queue = {}
        if sim.delivery:
            last_delivered = dict(sim.delivery._last_delivered_gsn)
            for gid, entries in sim.delivery._pending.items():
                pend = sorted(e.gsn for e in entries if not e.delivered)
                if pend:
                    pending_queue[gid] = pend
        lastdeliv_str = "{" + ", ".join(f"{g}: {v}" for g, v in sorted(last_delivered.items())) + "}" if last_delivered else "{}"
        queue_str = "{" + ", ".join(f"{g}: {v}" for g, v in sorted(pending_queue.items())) + "}" if pending_queue else "{}"

        lines = [
            "--- Sequencer (orderer/sequencer.go) ---",
            f"nextGSN: {sim.gsn + 1}",
            f"metadata: {metadata}",
            f"groupGSNQueue: {queue_str}",
            f"lastDeliveredGSN: {lastdeliv_str}",
            f"members: {members}",
            f"leader: {leader}",
            "term: 0 (eleicao nao simulada aqui)",
            "status: leader (idem)",
        ]
        self._show_debug_popup(lines, pos)

    def _open_adeliver_inspect(self, pos):
        """Struct real da entrega atômica por grupo (aDeliverInternal em
        orderer/sequencer.go: lastDeliveredGSN[groupID] e groupGSNQueue
        [groupID]), populada com os valores atuais desta simulação — a
        caixa 'ADeliver (por grupo)' é a metáfora visual, o popup mostra
        os campos como eles existem de fato no código Go."""
        sim = self.sim
        dlv = sim.delivery
        if not dlv:
            return
        groups_with_data = sorted(dlv._last_delivered_gsn.keys())

        lines = ["--- ADeliver / aDeliverInternal (orderer/sequencer.go) ---"]
        for gid in groups_with_data:
            last = dlv._last_delivered_gsn.get(gid, 0)
            pending = dlv._pending.get(gid, [])
            queue_repr = [f"{e.gsn}{'*' if e.committed else ''}" for e in pending]
            n_blocked = len(sim.blocked_requests.get(gid, []))
            lines.append(f"G{gid}.lastDeliveredGSN: {last}")
            lines.append(f"G{gid}.groupGSNQueue: {queue_repr}")
            lines.append(f"G{gid}.requestsBloqueadas: {n_blocked}")
        lines.append("(* = ja commitado, ainda atras de um GSN anterior)")
        self._show_debug_popup(lines, pos)

    def _open_inspect(self, hit, pos):
        kind, id_str = hit.split(":")
        id_ = int(id_str)
        if kind == "node":
            lines, color = self._inspect_node_lines(id_)
        else:
            lines, color = self._inspect_client_lines(id_)
        if lines:
            self._show_debug_popup(lines, pos)

    def _inspect_node_lines(self, node_id):
        node = self.sim.nodes[node_id] if node_id < len(self.sim.nodes) else None
        if not node:
            return [], C["accent"]
        groups = [f"G{g}" for g in node.groups]
        # Buckets pertencem ao GRUPO (todos os membros veem os mesmos buckets),
        # não a um nó específico — o líder de cada SN roda por rodízio dentro
        # do grupo, então não há "buckets deste nó" fixos para mostrar aqui.
        req = self.sim.current_request
        role = "Ocioso"
        if req:
            if req.leader == node_id:
                role = "\u2605 L\u00cdDER (coordenando vota\u00e7\u00e3o)"
            elif req.proxy_node == node_id:
                role = "\U0001f4ec PROXY (carteiro do cliente)"
            elif req.group_id in [g.id for g in self.sim.groups if node_id in g.members]:
                role = "\U0001f465 PARTICIPANTE (votando)"
        lines = [
            f"\u2500\u2500 {node.name} \u2500\u2500",
            f"Estado: {'\U0001f7e2 Ativo' if node.is_alive else '\U0001f534 Falhou'}",
            f"Papel atual: {role}",
            f"Grupos: {', '.join(groups)}",
        ]
        return lines, C["accent"]

    def _inspect_client_lines(self, client_id):
        client = self.sim.clients[client_id] if client_id < len(self.sim.clients) else None
        if not client:
            return [], C["orange"]
        req = self.sim.current_request
        is_active = req and req.client_id == client_id
        lines = [
            f"\u2500\u2500 {client.name} \u2500\u2500",
            f"{'\U0001f7e1 Enviando pedido agora' if is_active else '\u26aa Aguardando'}",
        ]
        if is_active:
            lines.append(f"Pedido n\u00ba: {req.client_sn}")
            lines.append(f"Destino: Node {req.proxy_node} (carteiro)")
            lines.append(f"Fila: Bucket {req.bucket_id}")
        return lines, C["orange"]

    _MSG_TYPE_NAMES = {
        MsgType.CLIENT_REQUEST: "Pedido do cliente",
        MsgType.GSN_REQUEST: "Pedido de numera\u00e7\u00e3o global",
        MsgType.GSN_RESPONSE: "Resposta de numera\u00e7\u00e3o",
        MsgType.META_STREAM: "Ordem global (META)",
        MsgType.PREPARE: "PREPARE (Fase 1a)",
        MsgType.PROMISE: "PROMISE (Fase 1b)",
        MsgType.ACCEPT: "ACCEPT (Fase 2a)",
        MsgType.ACCEPTED: "ACCEPTED (Fase 2b)",
        MsgType.COMMIT: "COMMIT (Fase 3)",
        MsgType.COMMIT_NOTIFY: "Resposta ao cliente",
        MsgType.CHECKPOINT: "Checkpoint",
        MsgType.VIEW_CHANGE: "Troca de l\u00edder",
        MsgType.NEW_VIEW: "Novo l\u00edder confirmado",
    }

    def _open_msg_inspect(self, msg, pos):
        type_name = self._MSG_TYPE_NAMES.get(msg.msg_type, str(msg.msg_type))
        src_name = f"Cliente {msg.from_id}" if msg.from_is_client else f"Node {msg.from_id}"
        dst_name = f"Cliente {msg.to_id}" if msg.to_is_client else f"Node {msg.to_id}"
        phase_name = self.sim.phase.name if self.sim.phase else "IDLE"
        lines = [
            f"-- {type_name} --",
            f"De: {src_name} -> Para: {dst_name}",
            f"Progresso: {int(msg.progress * 100)}%",
            f"Fase atual: {phase_name}",
        ]
        lines.extend(self._struct_lines(msg))
        lines.extend(self._msg_detail_lines(msg))
        self._show_debug_popup(lines, pos)

    @staticmethod
    def _struct_lines(msg):
        """Struct real de protobufs.ClientRequest (request.pb.go), com os
        valores desta request no instante desta mensagem \u2014 um teste de mesa
        visual din\u00e2mico, n\u00e3o est\u00e1tico."""
        snap = msg.req_snapshot
        if not snap:
            return []
        touched = snap["touched_groups"]
        gsn = snap["gsn"]
        return [
            "--- ClientRequest (protobuf) ---",
            f"RequestId: {{ClientId: {snap['client_id']}, ClientSn: {snap['client_sn']}}}",
            f"Payload:   \"{snap['payload']}\"" if snap['payload'] else "Payload:   (vazio)",
            f"Pubkey:    0x{snap['pubkey']} (simulado)",
            f"Signature: 0x{snap['signature']} (simulado)",
            f"GroupId:   {snap['group_id']}",
            f"TouchedGroups: {touched if touched else '[]'}",
            f"GSN:       {gsn if gsn > 0 else '0 (nao alocado \u2014 single-group)'}",
        ]

    @staticmethod
    def _msg_detail_lines(msg):
        digest = msg.batch_digest[:12] if msg.batch_digest else '?'
        dispatch = {
            MsgType.PREPARE: ["--- MPxPrepare ---", f"SN: {msg.sn}", f"Ballot: {msg.ballot}", f"Leader: Node {msg.from_id}"],
            MsgType.PROMISE: ["--- MPxPromise ---", f"SN: {msg.sn}", f"Ballot: {msg.ballot}", "Ok: true"],
            MsgType.ACCEPT: ["--- MPxAccept ---", f"SN: {msg.sn}", f"Ballot: {msg.ballot}", f"Digest: {digest}"],
            MsgType.ACCEPTED: ["--- MPxAccepted ---", f"SN: {msg.sn}", f"Ballot: {msg.ballot}", "Ok: true"],
            MsgType.COMMIT: ["--- MPxCommit ---", f"SN: {msg.sn}", f"Digest: {digest}"],
            MsgType.COMMIT_NOTIFY: ["--- Resposta ---", f"SN: {msg.sn}", f"Digest: {digest}", "COMMITTED"],
        }
        if msg.msg_type in dispatch:
            return dispatch[msg.msg_type]
        lines = []
        # Payload j\u00e1 aparece no bloco da struct (_struct_lines) \u2014 evita
        # duplicar o mesmo campo com formata\u00e7\u00e3o diferente.
        if msg.detail and not msg.detail.startswith("Payload="):
            lines.append(msg.detail)
        elif msg.label and not msg.label.startswith("REQ "):
            lines.append(f"R\u00f3tulo: {msg.label}")
        return lines

    # ─── Paint ────────────────────────────────────────────────────────────

    def paintEvent(self, event):
        if not self._node_pos:
            self._compute_positions()
        p = QPainter(self)
        p.setRenderHint(QPainter.Antialiasing)

        p.save()
        p.translate(self._pan_offset)
        p.scale(self._zoom, self._zoom)
        draw_groups(p, self.sim, self._node_pos)
        draw_connections(p, self.sim, self._node_pos)
        draw_meta_broadcast_lines(p, self.sim, self._node_pos)
        draw_messages(p, self.sim, self._node_pos, self._client_pos)
        msg_color = get_active_msg_color(self.sim)
        draw_nodes(p, self.sim, self._node_pos, msg_color)
        draw_sequencer_glow(p, self.sim, self._node_pos)
        draw_thought_bubbles(p, self._thought_bubbles, self._node_pos)
        draw_clients(p, self.sim, self._client_pos)
        p.restore()

        draw_gsn_meta_panel(p, self.sim)
        draw_adeliver_panel(p, self.sim)
        self._draw_phase_banner(p)
        p.end()

    _PHASE_BANNER_TEXT = {
        Phase.PREPARE: ("\u270b Setup: Prepare", "Lideres pedem permissao (so na 1a vez)"),
        Phase.PROMISE: ("\u2705 Setup: Promise", "Membros concordam — prepared=true"),
        Phase.CLIENT_SEND: ("\U0001f4e8 Pedido", "Clientes enviam requests ao sistema"),
        Phase.BUCKET_ASSIGN: ("\U0001faa3 Fila", "Pedidos entram no bucket"),
        Phase.ACCEPT: ("\U0001f5f3\ufe0f Steady State: Accept", "Lider propoe batch (ProposeIfDue)"),
        Phase.ACCEPTED: ("\U0001f44d Steady State: Accepted", "Membros aprovam — quorum"),
        Phase.COMMIT: ("\U0001f389 Commit", "Consenso atingido! deliverCommit"),
        Phase.COMMIT_NOTIFY: ("\U0001f4ec Notify", "Proxy responde ao cliente"),
        Phase.ADELIVER: ("\U0001f513 ADeliver", "Entrega atomica confirmada"),
        Phase.GSN_ASSIGN: ("\U0001f522 GSN", "Sequenciador atribui ordem global"),
    }

    def _draw_phase_banner(self, p):
        """Banner no topo central do canvas com a fase atual em linguagem simples."""
        phase = self.sim.phase
        if not phase or phase in (Phase.IDLE, Phase.DONE):
            return

        info = self._PHASE_BANNER_TEXT.get(phase)
        if not info:
            return

        title, subtitle = info
        w = self.width()
        banner_w = min(360, w - 260)
        banner_h = 36
        bx = (w - banner_w) / 2
        by = 8

        # Background
        bg = QPainterPath()
        bg.addRoundedRect(QRectF(bx, by, banner_w, banner_h), 10, 10)
        p.fillPath(bg, QColor(10, 18, 32, 220))
        border_c = QColor(C["primary"])
        border_c.setAlpha(80)
        p.setPen(QPen(border_c, 1))
        p.setBrush(Qt.NoBrush)
        p.drawPath(bg)

        # Titulo
        p.setPen(QColor(C["text"]))
        p.setFont(QFont("Segoe UI", 10, QFont.Bold))
        p.drawText(QRectF(bx, by, banner_w, banner_h * 0.55),
                   Qt.AlignCenter | Qt.AlignBottom, title)

        # Subtitulo
        p.setPen(QColor(C["text2"]))
        p.setFont(QFont("Segoe UI", 7))
        p.drawText(QRectF(bx, by + banner_h * 0.5, banner_w, banner_h * 0.45),
                   Qt.AlignCenter | Qt.AlignTop, subtitle)

