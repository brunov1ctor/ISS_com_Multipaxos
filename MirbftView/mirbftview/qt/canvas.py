"""Canvas — Grafo da rede com nós grandes, clientes, grupos e pacotes lentos."""

import math
from PySide6.QtWidgets import QWidget
from PySide6.QtCore import Qt, QPointF, QRectF, QTimer
from PySide6.QtGui import (
    QPainter, QPainterPath, QColor, QLinearGradient, QRadialGradient,
    QPen, QBrush, QFont
)
from mirbftview.qt.theme import C
from mirbftview.qt.simulation import Simulation, Phase, MsgType

MSG_COLORS = {
    MsgType.CLIENT_REQUEST: "#F97316",
    MsgType.GSN_REQUEST:    "#FACC15",
    MsgType.GSN_RESPONSE:   "#FACC15",
    MsgType.META_STREAM:    "#A855F7",
    MsgType.PREPARE:        C["phase_prepare"],
    MsgType.PROMISE:        C["phase_promise"],
    MsgType.ACCEPT:         C["phase_accept"],
    MsgType.ACCEPTED:       C["phase_accepted"],
    MsgType.COMMIT:         C["phase_commit"],
    MsgType.COMMIT_NOTIFY:  "#34D399",
    MsgType.CHECKPOINT:     "#A855F7",
    MsgType.VIEW_CHANGE:    "#EF4444",
    MsgType.NEW_VIEW:       "#F97316",
}

GROUP_COLORS = ["#00E5FF", "#FF6D00", "#76FF03", "#D500F9", "#FFEA00", "#F50057", "#00E676", "#651FFF"]


class NetworkCanvas(QWidget):
    """Grafo da rede — nós grandes, mensagens lentas com labels claros. Arraste os nós!"""

    def __init__(self, sim: Simulation, parent=None):
        super().__init__(parent)
        self.sim = sim
        self.setAttribute(Qt.WA_TranslucentBackground)
        self.setMouseTracking(True)
        self._node_pos: dict[int, QPointF] = {}
        self._client_pos: dict[int, QPointF] = {}
        self._dragging: str | None = None  # "node:0" or "client:1"
        self._drag_offset = QPointF(0, 0)
        self._drag_moved = False
        # Zoom & pan
        self._zoom = 1.0
        self._pan_offset = QPointF(0, 0)
        self._panning = False
        self._pan_start = QPointF(0, 0)
        # Inspection popup state
        self._inspect_popup: dict | None = None  # {"type": "node"|"msg", "data": ..., "pos": QPointF}
        # Thought bubbles: node_id -> {"text": str, "color": str, "ttl": int}
        self._thought_bubbles: dict[int, dict] = {}
        self._prev_messages: list = []  # mensagens do tick anterior
        self._timer = QTimer(self)
        self._timer.setInterval(16)
        self._timer.timeout.connect(self._on_tick)
        self._timer.start()

    def _on_tick(self):
        # Detecta mensagens que acabaram de chegar (progress era <1, agora =1)
        self._update_thought_bubbles()
        self.sim.tick()
        self.update()

    def _update_thought_bubbles(self):
        """Gera thought bubbles quando mensagens chegam aos nos."""
        # Decrementa TTL dos bubbles existentes
        expired = [nid for nid, b in self._thought_bubbles.items() if b["ttl"] <= 0]
        for nid in expired:
            del self._thought_bubbles[nid]
        for nid in self._thought_bubbles:
            self._thought_bubbles[nid]["ttl"] -= 1

        # Detecta mensagens que chegaram neste frame
        for msg in self.sim.messages:
            if msg.progress < 1.0:
                continue
            # Mensagem chegou ao destino
            dest_id = msg.to_id
            if msg.to_is_client:
                continue  # clientes nao tem thought bubble
            # Gera bubble baseado no tipo de mensagem
            text, color, is_error = self._bubble_for_msg(msg)
            if text:
                self._thought_bubbles[dest_id] = {
                    "text": text, "color": color, "is_error": is_error, "ttl": 45
                }

    def _bubble_for_msg(self, msg) -> tuple[str, str, bool]:
        """Retorna (texto, cor, is_error) para o thought bubble.
        Cor baseada no tipo de MENSAGEM. Fase indicada como info textual."""
        is_error = False

        # Verifica se o no destino esta morto (falha)
        dest_node = None
        for n in self.sim.nodes:
            if n.id == msg.to_id:
                dest_node = n
                break
        if dest_node and not dest_node.is_alive:
            return "FALHA! No offline", C["red"], True

        # Cor vem do tipo de mensagem (MSG_COLORS)
        color = MSG_COLORS.get(msg.msg_type, C["text2"])
        phase_name = self.sim.phase.name if self.sim.phase else ""

        dispatch = {
            MsgType.CLIENT_REQUEST: f"handleClientRequest()\n[{phase_name}] bucket enqueue",
            MsgType.PREPARE: f"handlePrepare()\n[{phase_name}] ballot={msg.ballot} sn={msg.sn}",
            MsgType.PROMISE: f"handlePromise()\n[{phase_name}] ballot={msg.ballot} ok",
            MsgType.ACCEPT: f"handleAccept()\n[{phase_name}] sn={msg.sn} d={msg.batch_digest[:6]}",
            MsgType.ACCEPTED: f"handleAccepted()\n[{phase_name}] sn={msg.sn} quorum++",
            MsgType.COMMIT: f"handleCommit()\n[{phase_name}] sn={msg.sn} -> log",
            MsgType.COMMIT_NOTIFY: f"outputProcessing()\n[{phase_name}] COMMIT_NOTIFY",
            MsgType.META_STREAM: f"handleMETA()\n[{phase_name}] GSN publicado",
            MsgType.CHECKPOINT: f"handleCheckpoint()\n[{phase_name}] watermarks",
            MsgType.VIEW_CHANGE: f"handleViewChange()\n[{phase_name}] ballot={msg.ballot}",
            MsgType.NEW_VIEW: f"handleNewView()\n[{phase_name}] lider confirmado",
        }

        text = dispatch.get(msg.msg_type, "")
        if text:
            # Retransmissao?
            if msg.msg_type == MsgType.ACCEPT and msg.label and "RE-" in msg.label:
                text = f"handleAccept()\n[RETRANSMIT] sn={msg.sn}"
                is_error = True
                color = C["red"]
            return text, color, is_error

        return "", C["text"], False

    def _compute_positions(self):
        w, h = self.width(), self.height()
        cx, cy = w * 0.55, h * 0.5
        n = len(self.sim.nodes)
        radius = min(w, h) * 0.28
        for i, node in enumerate(self.sim.nodes):
            angle = (2 * math.pi * i / n) - math.pi / 2
            self._node_pos[node.id] = QPointF(cx + radius * math.cos(angle), cy + radius * math.sin(angle))
        # Clients on the far left
        for i, client in enumerate(self.sim.clients):
            self._client_pos[client.id] = QPointF(60, cy - 50 + i * 100)

    def resizeEvent(self, event):
        super().resizeEvent(event)
        if not self._node_pos:
            self._compute_positions()

    def _to_scene(self, screen_pos: QPointF) -> QPointF:
        """Converte coordenada de tela para coordenada de cena (considerando zoom/pan)."""
        return (screen_pos - self._pan_offset) / self._zoom

    def _hit_test(self, pos: QPointF) -> str | None:
        """Retorna 'node:id' ou 'client:id' se clicou em cima."""
        scene_pos = self._to_scene(pos)
        for nid, npos in self._node_pos.items():
            if (scene_pos - npos).manhattanLength() < 38:
                return f"node:{nid}"
        for cid, cpos in self._client_pos.items():
            if (scene_pos - cpos).manhattanLength() < 26:
                return f"client:{cid}"
        return None

    def _hit_test_message(self, pos: QPointF):
        """Retorna a Message mais próxima do clique (se dist < 20px)."""
        scene_pos = self._to_scene(pos)
        best_msg = None
        best_dist = 20 / self._zoom  # adjust threshold for zoom
        for msg in self.sim.messages:
            if msg.from_is_client:
                src = self._client_pos.get(msg.from_id)
            else:
                src = self._node_pos.get(msg.from_id)
            if msg.to_is_client:
                dst = self._client_pos.get(msg.to_id)
            else:
                dst = self._node_pos.get(msg.to_id)
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

    def _is_over_config(self, pos: QPointF) -> bool:
        """Retorna True se a posição cai sobre o config panel overlay."""
        for child in self.children():
            if hasattr(child, 'isVisible') and child.isVisible() and hasattr(child, 'geometry'):
                if child.geometry().contains(pos.toPoint()):
                    return True
        return False

    def wheelEvent(self, event):
        """Zoom com scroll do mouse, centrado na posição do cursor."""
        if self._is_over_config(event.position()):
            event.ignore()
            return
        old_zoom = self._zoom
        delta = event.angleDelta().y()
        factor = 1.15 if delta > 0 else 1 / 1.15
        self._zoom = max(0.3, min(5.0, self._zoom * factor))
        # Ajustar pan para zoom centrado no cursor
        cursor_pos = event.position()
        self._pan_offset = cursor_pos - (self._zoom / old_zoom) * (cursor_pos - self._pan_offset)
        self._inspect_popup = None
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
            else:
                # Check if clicked on a message, otherwise start panning
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
            self._inspect_popup = None
            self.update()
        elif self._panning:
            self._drag_moved = True
            delta = event.position() - self._pan_start
            self._pan_offset += delta
            self._pan_start = event.position()
            self._inspect_popup = None
            self.update()
        else:
            hit = self._hit_test(event.position())
            msg = self._hit_test_message(event.position()) if not hit else None
            if hit or msg:
                self.setCursor(Qt.PointingHandCursor)
            else:
                self.setCursor(Qt.ArrowCursor)
        super().mouseMoveEvent(event)

    def mouseReleaseEvent(self, event):
        if event.button() == Qt.LeftButton:
            if self._dragging and not self._drag_moved:
                # Click without drag → inspect
                self._open_inspect(self._dragging, event.position())
            elif not self._dragging and not self._panning:
                # Clicked on message
                msg = self._hit_test_message(event.position())
                if msg:
                    self._open_msg_inspect(msg, event.position())
                else:
                    self._inspect_popup = None
            elif self._panning and not self._drag_moved:
                # Click on empty without moving → close popup
                self._inspect_popup = None
            self._dragging = None
            self._panning = False
            self.setCursor(Qt.ArrowCursor)
        super().mouseReleaseEvent(event)

    def _open_inspect(self, hit: str, pos: QPointF):
        """Abre popup de inspeção de nó ou cliente."""
        kind, id_str = hit.split(":")
        id_ = int(id_str)
        if kind == "node":
            lines, color = self._inspect_node_lines(id_)
        else:
            lines, color = self._inspect_client_lines(id_)
        if lines:
            self._inspect_popup = {"type": kind, "lines": lines, "pos": pos, "color": color}
            self.update()

    def _inspect_node_lines(self, node_id: int) -> tuple[list[str], str]:
        """Gera linhas de inspeção para um nó."""
        node = self.sim.nodes[node_id] if node_id < len(self.sim.nodes) else None
        if not node:
            return [], C["accent"]
        groups = [f"G{g}" for g in node.groups]
        assigned_buckets = []
        if hasattr(self.sim, 'epoch_mgr') and self.sim.epoch_mgr:
            assigned_buckets = self.sim.epoch_mgr.bucket_assignment.get(node_id, [])
        buffer_items = []
        for b in assigned_buckets:
            if b < len(self.sim.bucket_contents):
                buffer_items.extend(self.sim.bucket_contents[b])
        req = self.sim.current_request
        role = "Ocioso"
        if req:
            if req.leader == node_id:
                role = "★ LÍDER (coordenando votação)"
            elif req.proxy_node == node_id:
                role = "📬 PROXY (carteiro do cliente)"
            elif req.group_id in [g.id for g in self.sim.groups if node_id in g.members]:
                role = "👥 PARTICIPANTE (votando)"
        lines = [
            f"── {node.name} ──",
            f"Estado: {'🟢 Ativo' if node.is_alive else '🔴 Falhou'}",
            f"Papel atual: {role}",
            f"Grupos: {', '.join(groups)}",
            f"Filas (buckets): {assigned_buckets[:8]}{'...' if len(assigned_buckets) > 8 else ''}",
            f"Buffer ({len(buffer_items)} pedidos):",
        ]
        for item in buffer_items[:5]:
            lines.append(f"  • {item}")
        if len(buffer_items) > 5:
            lines.append(f"  ... +{len(buffer_items) - 5} mais")
        if not buffer_items:
            lines.append("  (vazio)")
        return lines, C["accent"]

    def _inspect_client_lines(self, client_id: int) -> tuple[list[str], str]:
        """Gera linhas de inspeção para um cliente."""
        client = self.sim.clients[client_id] if client_id < len(self.sim.clients) else None
        if not client:
            return [], C["orange"]
        req = self.sim.current_request
        is_active = req and req.client_id == client_id
        lines = [
            f"── {client.name} ──",
            f"{'🟡 Enviando pedido agora' if is_active else '⚪ Aguardando'}",
        ]
        if is_active:
            lines.append(f"Pedido nº: {req.client_sn}")
            lines.append(f"Destino: Node {req.proxy_node} (carteiro)")
            lines.append(f"Fila: Bucket {req.bucket_id}")
        return lines, C["orange"]

    _MSG_TYPE_NAMES = {
        MsgType.CLIENT_REQUEST: "Pedido do cliente",
        MsgType.GSN_REQUEST: "Pedido de numeração global",
        MsgType.GSN_RESPONSE: "Resposta de numeração",
        MsgType.META_STREAM: "Ordem global (META)",
        MsgType.PREPARE: "PREPARE (Fase 1a)",
        MsgType.PROMISE: "PROMISE (Fase 1b)",
        MsgType.ACCEPT: "ACCEPT (Fase 2a)",
        MsgType.ACCEPTED: "ACCEPTED (Fase 2b)",
        MsgType.COMMIT: "COMMIT (Fase 3)",
        MsgType.COMMIT_NOTIFY: "Resposta ao cliente",
        MsgType.CHECKPOINT: "Checkpoint",
        MsgType.VIEW_CHANGE: "Troca de líder",
        MsgType.NEW_VIEW: "Novo líder confirmado",
    }

    def _open_msg_inspect(self, msg, pos: QPointF):
        """Abre popup de inspecao de mensagem. Inclui fase atual como info."""
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
        lines.extend(self._msg_detail_lines(msg))

        color = MSG_COLORS.get(msg.msg_type, C["text2"])
        self._inspect_popup = {"type": "msg", "lines": lines, "pos": pos, "color": color}
        self.update()

    @staticmethod
    def _msg_detail_lines(msg) -> list[str]:
        """Retorna linhas de detalhe específicas por tipo de mensagem."""
        digest = msg.batch_digest[:12] if msg.batch_digest else '?'
        dispatch = {
            MsgType.PREPARE: [
                "─── Conteúdo MPxPrepare ───",
                f"SN: {msg.sn}", f"Ballot: {msg.ballot}",
                f"Leader: Node {msg.from_id}",
                f"Pergunta: \"Posso coordenar SN {msg.sn}?\"",
            ],
            MsgType.PROMISE: [
                "─── Conteúdo MPxPromise ───",
                f"SN: {msg.sn}", f"Ballot prometido: {msg.ballot}",
                "Ok: true",
                f"Resposta: \"Prometo não aceitar ballot < {msg.ballot}\"",
            ],
            MsgType.ACCEPT: [
                "─── Conteúdo MPxAccept ───",
                f"SN: {msg.sn}", f"Ballot: {msg.ballot}",
                f"Batch digest: {digest}",
                f"Proposta: \"Registrem este pacote no SN {msg.sn}\"",
            ],
            MsgType.ACCEPTED: [
                "─── Conteúdo MPxAccepted ───",
                f"SN: {msg.sn}", f"Ballot: {msg.ballot}",
                "Ok: true", "Confirmação: \"Aceitei e gravei no log\"",
            ],
            MsgType.COMMIT: [
                "─── Conteúdo MPxCommit ───",
                f"SN: {msg.sn}", f"Digest: {digest}",
                f"Decisão: \"Consenso atingido para SN {msg.sn}\"",
            ],
            MsgType.COMMIT_NOTIFY: [
                "─── Resposta ao cliente ───",
                f"SN confirmado: {msg.sn}", f"Digest: {digest}",
                "Status: COMMITTED com sucesso",
            ],
        }
        if msg.msg_type in dispatch:
            return dispatch[msg.msg_type]
        # Tipos com detail textual
        lines = []
        if msg.msg_type == MsgType.CLIENT_REQUEST:
            lines.append("─── Request do cliente ───")
        elif msg.msg_type == MsgType.META_STREAM:
            lines.extend(["─── META_STREAM ───", "Ordem global para cross-group ops"])
        elif msg.msg_type == MsgType.VIEW_CHANGE:
            lines.extend(["─── ViewChange ───", f"Ballot proposto: {msg.ballot}"])
        if msg.detail:
            lines.append(msg.detail)
        elif msg.label:
            lines.append(f"Rótulo: {msg.label}")
        return lines

    def paintEvent(self, event):
        if not self._node_pos:
            self._compute_positions()
        p = QPainter(self)
        p.setRenderHint(QPainter.Antialiasing)
        # Apply zoom & pan transform for scene elements
        p.save()
        p.translate(self._pan_offset)
        p.scale(self._zoom, self._zoom)
        self._draw_groups(p)
        self._draw_connections(p)
        self._draw_meta_broadcast_lines(p)
        self._draw_messages(p)
        self._draw_nodes(p)
        self._draw_sequencer_glow(p)
        self._draw_thought_bubbles(p)
        self._draw_clients(p)
        p.restore()
        # HUD elements drawn in screen space (no zoom/pan)
        self._draw_gsn_meta_panel(p)
        self._draw_adeliver_panel(p)
        self._draw_progress_bar(p)
        self._draw_inspect_popup(p)
        p.end()

    def _draw_groups(self, p: QPainter):
        for group in self.sim.groups:
            if not group.members:
                continue
            points = [self._node_pos[nid] for nid in group.members if nid in self._node_pos]
            if len(points) < 2:
                continue
            cx = sum(pt.x() for pt in points) / len(points)
            cy = sum(pt.y() for pt in points) / len(points)
            max_dist = max(math.hypot(pt.x() - cx, pt.y() - cy) for pt in points)
            r = max_dist + 60
            color = QColor(GROUP_COLORS[group.id % len(GROUP_COLORS)])
            color.setAlpha(15)
            p.setPen(Qt.NoPen)
            p.setBrush(color)
            p.drawEllipse(QPointF(cx, cy), r, r * 0.7)
            # Border
            border_c = QColor(GROUP_COLORS[group.id % len(GROUP_COLORS)])
            border_c.setAlpha(45)
            p.setPen(QPen(border_c, 1.2, Qt.DashLine))
            p.setBrush(Qt.NoBrush)
            p.drawEllipse(QPointF(cx, cy), r, r * 0.7)
            # Label
            p.setPen(QColor(GROUP_COLORS[group.id % len(GROUP_COLORS)]))
            p.setFont(QFont("Segoe UI", 8, QFont.Bold))
            p.drawText(QRectF(cx - 60, cy - r * 0.7 - 18, 120, 16), Qt.AlignCenter, group.name)

    def _draw_connections(self, p: QPainter):
        """Linhas de conexão entre nós do mesmo grupo."""
        for group in self.sim.groups:
            if group.id == 0:
                continue  # Skip sequencer group (too many lines)
            color = QColor(GROUP_COLORS[group.id % len(GROUP_COLORS)])
            color.setAlpha(20)
            p.setPen(QPen(color, 0.8))
            members = [self._node_pos[nid] for nid in group.members if nid in self._node_pos]
            for i in range(len(members)):
                for j in range(i + 1, len(members)):
                    p.drawLine(members[i], members[j])

    def _draw_messages(self, p: QPainter):
        for msg in self.sim.messages:
            if msg.from_is_client:
                src = self._client_pos.get(msg.from_id)
            else:
                src = self._node_pos.get(msg.from_id)
            if msg.to_is_client:
                dst = self._client_pos.get(msg.to_id)
            else:
                dst = self._node_pos.get(msg.to_id)
            if src is None or dst is None:
                continue

            t = msg.progress
            x = src.x() + (dst.x() - src.x()) * t
            y = src.y() + (dst.y() - src.y()) * t
            color = QColor(MSG_COLORS.get(msg.msg_type, C["text2"]))

            # Trail line
            trail_c = QColor(color)
            trail_c.setAlpha(50)
            p.setPen(QPen(trail_c, 2.0))
            p.drawLine(src, QPointF(x, y))

            # Particle glow
            glow = QRadialGradient(QPointF(x, y), 16)
            gc = QColor(color)
            gc.setAlpha(100)
            glow.setColorAt(0.0, gc)
            glow.setColorAt(1.0, QColor(0, 0, 0, 0))
            p.setPen(Qt.NoPen)
            p.setBrush(QBrush(glow))
            p.drawEllipse(QPointF(x, y), 16, 16)

            # Core particle
            p.setBrush(color)
            p.drawEllipse(QPointF(x, y), 5, 5)

            # Label above particle
            if msg.label:
                p.setPen(QColor(255, 255, 255, 220))
                p.setFont(QFont("Segoe UI", 7, QFont.Bold))
                p.drawText(QRectF(x - 60, y - 20, 120, 14), Qt.AlignCenter, msg.label)

    def _get_active_msg_color(self) -> str:
        """Retorna a cor da mensagem atualmente em transito (dominante visual)."""
        for msg in self.sim.messages:
            if msg.progress < 1.0:
                return MSG_COLORS.get(msg.msg_type, C["text2"])
        # Sem mensagem em transito, usa a ultima mensagem
        if self.sim.messages:
            return MSG_COLORS.get(self.sim.messages[-1].msg_type, C["text2"])
        return C["node_idle"]

    def _draw_nodes(self, p: QPainter):
        req = self.sim.current_request
        msg_color = self._get_active_msg_color()
        for node in self.sim.nodes:
            pos = self._node_pos.get(node.id)
            if pos is None:
                continue
            is_leader = req and req.leader == node.id
            is_in_group = req and req.group_id in [g.id for g in self.sim.groups if node.id in g.members]
            self._draw_single_node(p, node, pos, is_leader, is_in_group, msg_color)

    def _draw_single_node(self, p: QPainter, node, pos: QPointF, is_leader: bool, is_in_group: bool, msg_color: str = ""):
        """Desenha um no individual com glow, glass, border e labels.
        Cor do border baseada na mensagem ativa, nao na fase."""
        r = 34
        if is_leader:
            border_color = QColor(msg_color) if msg_color else QColor(C["accent"])
        elif is_in_group and self.sim.phase not in (Phase.IDLE, Phase.DONE):
            bc = QColor(msg_color) if msg_color else QColor(C["accent"])
            bc.setAlpha(160)
            border_color = bc
        else:
            border_color = QColor(C["node_idle"])




        # Glow for leader
        if is_leader and self.sim.phase not in (Phase.IDLE, Phase.DONE):
            glow = QRadialGradient(pos, r + 16)
            gc = QColor(border_color)
            gc.setAlpha(50)
            glow.setColorAt(0.0, gc)
            glow.setColorAt(1.0, QColor(0, 0, 0, 0))
            p.setPen(Qt.NoPen)
            p.setBrush(QBrush(glow))
            p.drawEllipse(pos, r + 16, r + 16)

        # Glass circle
        path = QPainterPath()
        path.addEllipse(pos, r, r)
        grad = QLinearGradient(pos.x(), pos.y() - r, pos.x(), pos.y() + r)
        grad.setColorAt(0.0, QColor(40, 60, 90, 210))
        grad.setColorAt(1.0, QColor(20, 36, 60, 230))
        p.fillPath(path, QBrush(grad))

        # Specular
        spec = QPainterPath()
        spec.addEllipse(QPointF(pos.x(), pos.y() - r * 0.3), r * 0.65, r * 0.35)
        sg = QLinearGradient(pos.x(), pos.y() - r, pos.x(), pos.y())
        sg.setColorAt(0.0, QColor(255, 255, 255, 40))
        sg.setColorAt(1.0, QColor(255, 255, 255, 0))
        p.setPen(Qt.NoPen)
        p.fillPath(spec, QBrush(sg))

        # Border
        bc = QColor(border_color)
        bc.setAlpha(200 if is_leader else 100)
        p.setPen(QPen(bc, 2.5 if is_leader else 1.5))
        p.setBrush(Qt.NoBrush)
        p.drawEllipse(pos, r, r)

        # Name
        p.setPen(QColor(C["text"]))
        p.setFont(QFont("Segoe UI", 10, QFont.Bold))
        p.drawText(QRectF(pos.x() - r, pos.y() - 8, r * 2, 16), Qt.AlignCenter, node.name)

        # Groups below
        groups_str = ",".join(f"G{g}" for g in node.groups)
        p.setPen(QColor(C["text3"]))
        p.setFont(QFont("Segoe UI", 7))
        p.drawText(QRectF(pos.x() - r, pos.y() + 10, r * 2, 12), Qt.AlignCenter, groups_str)

        # Leader badge
        if is_leader and self.sim.phase not in (Phase.IDLE, Phase.DONE):
            p.setPen(QColor(C["phase_prepare"]))
            p.setFont(QFont("Segoe UI", 7, QFont.Bold))
            p.drawText(QRectF(pos.x() - r, pos.y() - r - 16, r * 2, 12), Qt.AlignCenter, "★ LÍDER")

    def _draw_clients(self, p: QPainter):
        for client in self.sim.clients:
            pos = self._client_pos.get(client.id)
            if pos is None:
                continue
            r = 22
            # Rounded rect
            rect = QRectF(pos.x() - r, pos.y() - r, r * 2, r * 2)
            path = QPainterPath()
            path.addRoundedRect(rect, 10, 10)
            p.fillPath(path, QColor(60, 40, 100, 190))
            p.setPen(QPen(QColor(C["orange"]), 1.5))
            p.drawPath(path)
            # Label
            p.setPen(QColor(C["text"]))
            p.setFont(QFont("Segoe UI", 8, QFont.Bold))
            p.drawText(rect, Qt.AlignCenter, client.name)

    def _draw_thought_bubbles(self, p: QPainter):
        """Desenha baloes de pensamento acima dos nos quando mensagens chegam."""
        for nid, bubble in self._thought_bubbles.items():
            pos = self._node_pos.get(nid)
            if pos is None:
                continue
            text = bubble["text"]
            color = bubble["color"]
            is_error = bubble.get("is_error", False)
            ttl = bubble["ttl"]

            # Fade out
            alpha = min(255, ttl * 6)
            if alpha <= 0:
                continue

            lines = text.split("\n")
            p.setFont(QFont("Consolas", 7))
            fm = p.fontMetrics()
            line_h = fm.height() + 1
            text_w = max(fm.horizontalAdvance(l) for l in lines) + 16
            bubble_w = max(80, min(160, text_w))
            bubble_h = line_h * len(lines) + 10

            # Position: above-right of node
            bx = pos.x() + 20
            by = pos.y() - 50 - bubble_h

            # Thought bubble shape
            bg_path = QPainterPath()
            bg_path.addRoundedRect(QRectF(bx, by, bubble_w, bubble_h), 8, 8)

            # Background
            bg_color = QColor(40, 10, 10, alpha) if is_error else QColor(12, 20, 38, alpha)
            p.fillPath(bg_path, bg_color)

            # Border
            border_c = QColor(color)
            border_c.setAlpha(alpha)
            p.setPen(QPen(border_c, 1.5 if is_error else 1.0))
            p.setBrush(Qt.NoBrush)
            p.drawPath(bg_path)

            # Connector dots (thought bubble style)
            dot_alpha = alpha
            p.setPen(Qt.NoPen)
            dot_c = QColor(color)
            dot_c.setAlpha(dot_alpha)
            p.setBrush(dot_c)
            # 3 dots from node to bubble
            dx = bx - pos.x()
            dy = by + bubble_h - pos.y()
            for i, (frac, r) in enumerate([(0.3, 2.5), (0.55, 3.5), (0.8, 2.0)]):
                cx = pos.x() + 24 + dx * frac * 0.3
                cy = pos.y() - 14 + dy * frac
                p.drawEllipse(QPointF(cx, cy), r, r)

            # Text
            ty = by + 5
            for i, line in enumerate(lines):
                if i == 0:
                    p.setPen(QColor(color))
                    p.setFont(QFont("Consolas", 7, QFont.Bold))
                else:
                    tc = QColor(C["text"] if not is_error else C["red"])
                    tc.setAlpha(alpha)
                    p.setPen(tc)
                    p.setFont(QFont("Consolas", 7))
                p.drawText(QRectF(bx + 6, ty, bubble_w - 12, line_h),
                           Qt.AlignLeft | Qt.AlignVCenter, line)
                ty += line_h

    # Steps for the progress bar (order matters)
    _PROGRESS_STEPS = [
        (Phase.CLIENT_SEND,      "Envio",       C["orange"]),
        (Phase.BUCKET_ASSIGN,    "Fila",        C["gold"]),
        (Phase.BATCH_CUT,        "Pacote",      C["gold"]),
        (Phase.PREPARE,          "Permissao",   C["phase_prepare"]),
        (Phase.PROMISE,          "Promessa",    C["phase_promise"]),
        (Phase.ACCEPT,           "Proposta",    C["phase_accept"]),
        (Phase.ACCEPTED,         "Aceite",      C["phase_accepted"]),
        (Phase.COMMIT,           "Decisao",     C["phase_commit"]),
        (Phase.COMMIT_NOTIFY,    "Resposta",    C["green"]),
    ]

    def _get_sequencer_node_id(self) -> int:
        """Retorna o ID do no sequenciador (menor ID do G0)."""
        if self.sim.groups and self.sim.groups[0].members:
            return min(self.sim.groups[0].members)
        return 0

    def _draw_sequencer_glow(self, p: QPainter):
        """Glow especial no no sequenciador quando GSN esta ativo."""
        meta = self.sim.meta_stream
        if not meta:
            return
        seq_id = self._get_sequencer_node_id()
        pos = self._node_pos.get(seq_id)
        if pos is None:
            return
        gsn_val = self.sim.gsn
        if gsn_val <= 0:
            return
        # Outer ring glow (roxo, nao vermelho)
        glow = QRadialGradient(pos, 55)
        gc = QColor("#A855F7")
        gc.setAlpha(35)
        glow.setColorAt(0.0, gc)
        gc2 = QColor("#D500F9")
        gc2.setAlpha(15)
        glow.setColorAt(0.6, gc2)
        glow.setColorAt(1.0, QColor(0, 0, 0, 0))
        p.setPen(Qt.NoPen)
        p.setBrush(QBrush(glow))
        p.drawEllipse(pos, 55, 55)
        # "SEQ" badge below node
        p.setPen(QColor("#A855F7"))
        p.setFont(QFont("Segoe UI", 7, QFont.Bold))
        p.drawText(QRectF(pos.x() - 30, pos.y() + 38, 60, 12),
                   Qt.AlignCenter, f"SEQ GSN={gsn_val}")

    def _draw_meta_broadcast_lines(self, p: QPainter):
        """Linhas de broadcast META do sequenciador para os outros nos (quando ativo)."""
        if self.sim.phase != Phase.GSN_ASSIGN:
            return
        seq_id = self._get_sequencer_node_id()
        seq_pos = self._node_pos.get(seq_id)
        if seq_pos is None:
            return
        # Linhas tracejadas roxas do sequenciador para todos os nos do G0
        pen = QPen(QColor("#A855F7"), 1.5, Qt.DashLine)
        p.setPen(pen)
        for nid in self.sim.groups[0].members:
            if nid == seq_id:
                continue
            npos = self._node_pos.get(nid)
            if npos:
                p.drawLine(seq_pos, npos)



    def _draw_gsn_meta_panel(self, p: QPainter):
        """Painel HUD do GSN/META — canto superior esquerdo."""
        meta = self.sim.meta_stream
        gsn_val = self.sim.gsn

        box_w = 200
        box_x = 10
        box_y = 10
        line_h = 14
        visible_meta = meta[-4:] if meta else []
        box_h = line_h * (len(visible_meta) + 3) + 10

        bg = QPainterPath()
        bg.addRoundedRect(QRectF(box_x, box_y, box_w, box_h), 8, 8)
        p.fillPath(bg, QColor(10, 18, 32, 220))
        border_c = QColor("#A855F7")
        border_c.setAlpha(120)
        p.setPen(QPen(border_c, 1.2))
        p.setBrush(Qt.NoBrush)
        p.drawPath(bg)

        cy = box_y + 6

        p.setPen(QColor("#A855F7"))
        p.setFont(QFont("Segoe UI", 8, QFont.Bold))
        p.drawText(QRectF(box_x + 8, cy, box_w - 16, line_h),
                   Qt.AlignLeft | Qt.AlignVCenter, "GSN Sequencer")
        cy += line_h

        seq_id = self._get_sequencer_node_id()
        p.setPen(QColor(C["text"]))
        p.setFont(QFont("Consolas", 8))
        p.drawText(QRectF(box_x + 8, cy, box_w - 16, line_h),
                   Qt.AlignLeft | Qt.AlignVCenter,
                   f"GSN atual: {gsn_val}  (Node {seq_id})")
        cy += line_h

        p.setPen(QColor("#D500F9"))
        p.setFont(QFont("Segoe UI", 7, QFont.Bold))
        p.drawText(QRectF(box_x + 8, cy, box_w - 16, line_h),
                   Qt.AlignLeft | Qt.AlignVCenter, "META Stream:")
        cy += line_h

        if visible_meta:
            for entry in reversed(visible_meta):
                grps = ",".join(str(g) for g in entry["groups"])
                is_latest = (entry == visible_meta[-1])
                p.setPen(QColor(C["text"]) if not is_latest else QColor("#D500F9"))
                p.setFont(QFont("Consolas", 7, QFont.Bold if is_latest else QFont.Normal))
                txt = f"GSN={entry['gsn']} -> G[{grps}] N{entry['published_by']}"
                p.drawText(QRectF(box_x + 12, cy, box_w - 20, line_h),
                           Qt.AlignLeft | Qt.AlignVCenter, txt)
                cy += line_h
        else:
            p.setPen(QColor(C["text3"]))
            p.setFont(QFont("Consolas", 7))
            p.drawText(QRectF(box_x + 12, cy, box_w - 20, line_h),
                       Qt.AlignLeft | Qt.AlignVCenter, "(nenhuma publicacao)")

    def _draw_adeliver_panel(self, p: QPainter):
        """Painel HUD do ADeliver — abaixo do GSN panel, canto superior esquerdo."""
        dlv = self.sim.delivery
        if not dlv:
            return
        groups_with_data = sorted(dlv._last_delivered_gsn.keys())
        if not groups_with_data:
            return

        meta = self.sim.meta_stream
        visible_meta = meta[-4:] if meta else []
        gsn_box_h = 14 * (len(visible_meta) + 3) + 10
        box_w = 200
        box_x = 10
        box_y = 10 + gsn_box_h + 6
        line_h = 14
        box_h = line_h * (len(groups_with_data) + 1) + 10

        bg = QPainterPath()
        bg.addRoundedRect(QRectF(box_x, box_y, box_w, box_h), 8, 8)
        p.fillPath(bg, QColor(10, 18, 32, 220))
        border_c = QColor(C["green"])
        border_c.setAlpha(120)
        p.setPen(QPen(border_c, 1.2))
        p.setBrush(Qt.NoBrush)
        p.drawPath(bg)

        cy = box_y + 6

        p.setPen(QColor(C["green"]))
        p.setFont(QFont("Segoe UI", 8, QFont.Bold))
        p.drawText(QRectF(box_x + 8, cy, box_w - 16, line_h),
                   Qt.AlignLeft | Qt.AlignVCenter, "ADeliver (por grupo)")
        cy += line_h

        for gid in groups_with_data:
            next_gsn = dlv.get_next_expected_gsn(gid)
            blocked = dlv.get_blocked_entries(gid)
            last = dlv._last_delivered_gsn.get(gid, 0)

            dot_x = box_x + 14
            dot_y = cy + line_h / 2
            dot_color = QColor(C["red"]) if blocked else QColor(C["green"])
            p.setPen(Qt.NoPen)
            p.setBrush(dot_color)
            p.drawEllipse(QPointF(dot_x, dot_y), 4, 4)

            p.setPen(QColor(C["text"]))
            p.setFont(QFont("Consolas", 7))
            txt = f"G{gid}: last={last} next={next_gsn}"
            if blocked:
                txt += f" BLOQ:{len(blocked)}"
                p.setPen(QColor(C["red"]))
            p.drawText(QRectF(box_x + 24, cy, box_w - 32, line_h),
                       Qt.AlignLeft | Qt.AlignVCenter, txt)
            cy += line_h

    def _draw_progress_bar(self, p: QPainter):
        """Barra de progresso na parte inferior — mostra etapas do fluxo."""
        phase = self.sim.phase
        if phase == Phase.IDLE:
            return

        w, h = self.width(), self.height()
        steps = self._PROGRESS_STEPS
        n = len(steps)

        # Dimensions
        bar_h = 36
        margin_x = 20
        bar_y = h - bar_h - 8
        available_w = w - margin_x * 2
        step_w = available_w / n

        # Background
        bg_rect = QRectF(margin_x - 6, bar_y - 4, available_w + 12, bar_h + 8)
        bg_path = QPainterPath()
        bg_path.addRoundedRect(bg_rect, 12, 12)
        p.fillPath(bg_path, QColor(10, 18, 32, 210))
        p.setPen(QPen(QColor(255, 255, 255, 30), 1))
        p.setBrush(Qt.NoBrush)
        p.drawPath(bg_path)

        # Find current step index
        current_idx = -1
        for i, (step_phase, _, _) in enumerate(steps):
            if step_phase == phase:
                current_idx = i
                break
        if current_idx == -1 and phase == Phase.DONE:
            current_idx = n

        for i, (step_phase, label, color) in enumerate(steps):
            cx = margin_x + step_w * i + step_w / 2
            cy = bar_y + bar_h / 2

            is_done = i < current_idx
            is_current = i == current_idx

            # Connector line to next step
            if i < n - 1:
                next_cx = margin_x + step_w * (i + 1) + step_w / 2
                line_color = QColor(color) if is_done else QColor(C["text3"])
                line_color.setAlpha(120 if is_done else 40)
                p.setPen(QPen(line_color, 2))
                p.drawLine(QPointF(cx + 8, cy), QPointF(next_cx - 8, cy))

            # Circle
            radius = 7 if is_current else 5
            if is_current:
                glow = QRadialGradient(QPointF(cx, cy), 14)
                gc = QColor(color)
                gc.setAlpha(80)
                glow.setColorAt(0.0, gc)
                glow.setColorAt(1.0, QColor(0, 0, 0, 0))
                p.setPen(Qt.NoPen)
                p.setBrush(QBrush(glow))
                p.drawEllipse(QPointF(cx, cy), 14, 14)

            p.setPen(Qt.NoPen)
            p.setBrush(QColor(color) if (is_done or is_current) else QColor(C["text3"]))
            p.drawEllipse(QPointF(cx, cy), radius, radius)

            # Label below
            lbl_color = QColor(color) if (is_done or is_current) else QColor(C["text3"])
            p.setPen(lbl_color)
            p.setFont(QFont("Segoe UI", 6, QFont.Bold if is_current else QFont.Normal))
            p.drawText(QRectF(cx - step_w / 2, cy + 10, step_w, 12), Qt.AlignCenter, label)



    def _draw_inspect_popup(self, p: QPainter):
        """Desenha popup de inspeção (nó ou mensagem)."""
        popup = self._inspect_popup
        if not popup:
            return

        lines = popup["lines"]
        pos = popup["pos"]
        color = popup["color"]
        w_canvas = self.width()
        h_canvas = self.height()

        # Calculate popup size
        p.setFont(QFont("Segoe UI", 8))
        fm = p.fontMetrics()
        line_h = fm.height() + 2
        max_text_w = max(fm.horizontalAdvance(l) for l in lines) + 24
        popup_w = min(max_text_w, 300)
        popup_h = line_h * len(lines) + 16

        # Position: try right of click, flip if off-screen
        px = pos.x() + 15
        py = pos.y() - popup_h / 2
        if px + popup_w > w_canvas - 10:
            px = pos.x() - popup_w - 15
        if py < 10:
            py = 10
        if py + popup_h > h_canvas - 10:
            py = h_canvas - popup_h - 10

        # Background
        rect = QRectF(px, py, popup_w, popup_h)
        bg = QPainterPath()
        bg.addRoundedRect(rect, 10, 10)
        p.fillPath(bg, QColor(12, 20, 38, 235))
        border_c = QColor(color)
        border_c.setAlpha(150)
        p.setPen(QPen(border_c, 1.5))
        p.drawPath(bg)

        # Lines
        y = py + 10
        for i, line in enumerate(lines):
            if i == 0:
                p.setPen(QColor(color))
                p.setFont(QFont("Segoe UI", 9, QFont.Bold))
            else:
                p.setPen(QColor(C["text"]))
                p.setFont(QFont("Segoe UI", 8))
            p.drawText(QRectF(px + 10, y, popup_w - 20, line_h), Qt.AlignLeft | Qt.AlignVCenter, line)
            y += line_h
