"""NetworkCanvas — classe principal do grafo da rede."""

import math
import re
from PySide6.QtWidgets import (
    QWidget, QTextEdit, QLineEdit, QLabel, QToolTip, QListWidget, QListWidgetItem,
)
from PySide6.QtCore import Qt, QPointF, QRectF, QTimer, QEvent
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


_PLACEHOLDER_RE = re.compile(r"<[^>]+>")

# (verbo, molde pra inserir, descrição curta) -- mesma sintaxe de apimode.go. put/get aparecem
# duas vezes (com e sem grupo), igual "overloads" de função no autocomplete de um editor: mesmo
# verbo, assinaturas diferentes. Filtrado por PREFIXO do primeiro token digitado (ver
# _update_cmd_suggestions) -- "ge" já estreita pra só as entradas de "get".
_CMD_SUGGESTIONS = [
    ("ls", "ls", "mostra a topologia -- sem consenso"),
    ("help", "help", "mostra a ajuda completa (igual o terminal)"),
    ("put", "put <chave> <valor>", "grava -- grupo por hash automático"),
    ("put", "put <grupo> <chave> <valor>", "grava -- grupo explícito"),
    ("get", "get <chave>", "lê -- grupo por hash automático"),
    ("get", "get <grupo> <chave>", "lê -- grupo explícito"),
    ("delete", "delete <chave>", "apaga -- grupo por hash automático"),
    ("delete", "delete <grupo> <chave>", "apaga -- grupo explícito"),
    ("transfer", "transfer <deGrupo> <deChave> <paraGrupo> <paraChave> <valor>",
     "move valor entre dois grupos"),
    ("overload", "overload <protocolo|grupo> <duração>",
     "liga o gerador de carga contínuo por um tempo"),
]


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
        self._cmd_prefix_label = self._make_cmd_prefix_label()
        self._cmd_input = self._make_cmd_input()
        self._cmd_suggest = self._make_cmd_suggest_list()
        self._cmd_target_client: int | None = None
        self._cmd_open_pos = QPointF(0, 0)
        self._cmd_is_template = False  # texto atual veio de uma sugestão aceita, ainda não editado
        self._cmd_list_geom = (0, 0, 0)  # (x, y_abaixo_do_input, largura) -- calculado na abertura
        self._thought_bubbles: dict[int, dict] = {}
        self._hidden_groups: set[int] = set()   # grupos com a região escondida (olhinho)
        self._eye_rects: dict[int, QRectF] = {}  # área de clique do olhinho de cada grupo (cena)
        self._eye_click = False
        self._color_pool_idx = 0
        self._known_msgs: set = set()
        self._timer = QTimer(self)
        self._timer.setInterval(16)
        self._timer.timeout.connect(self._on_tick)
        self._timer.start()

    # ─── Tick ─────────────────────────────────────────────────────────────

    def _on_tick(self):
        update_thought_bubbles(self.sim, self._thought_bubbles)
        self._refresh_live_popup()
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

    def _make_cmd_prefix_label(self):
        """"orderingclient " em cinza na frente do campo -- simula o nome do binário já digitado
        no prompt, igual um terminal de verdade ("Bruno@node-0:~$ orderingclient _"), pra deixar
        claro que o que você digita depois é literalmente o resto da linha de comando real."""
        lbl = QLabel("orderingclient", self)
        lbl.setFont(QFont("Consolas", 9))
        lbl.setStyleSheet(
            "QLabel { background: rgba(12,20,38,240); color: rgba(212,212,212,140); "
            "border: 1px solid rgba(88,216,255,0.6); border-right: none; "
            "border-top-left-radius: 4px; border-bottom-left-radius: 4px; padding: 4px 0px 4px 6px; }"
        )
        lbl.hide()
        return lbl

    def _make_cmd_input(self) -> QLineEdit:
        """Campo de texto para digitar um comando na bolinha de um cliente (clique único, ver
        _open_client_command_input) e simular a execução dele no sistema -- mesma sintaxe de 'orderingclient api ...
        put/get/transfer/ls/help' (ver apimode.go), traduzida pro payload real em
        engine/tick.py:inject_client_command (ou, pra ls/help, pra saída estática simulada em
        engine/tick.py:local_command_output -- ver _submit_client_command)."""
        w = QLineEdit(self)
        w.setFont(QFont("Consolas", 9))
        w.setStyleSheet(
            "QLineEdit { background: rgba(12,20,38,240); color: #D4D4D4; "
            "border: 1px solid rgba(88,216,255,0.6); border-top-right-radius: 4px; "
            "border-bottom-right-radius: 4px; border-top-left-radius: 0px; "
            "border-bottom-left-radius: 0px; padding: 4px; } "
            "QLineEdit[error=\"true\"] { border: 1px solid rgba(255,90,90,0.9); } "
            # Molde recém-inserido por um chip, ainda não editado: todo o texto fica opaco
            # (esmaecido) -- sinaliza "isto é só um molde, preencha os <placeholders>". Some
            # assim que a pessoa digita algo (ver _on_cmd_text_edited).
            "QLineEdit[template=\"true\"] { color: rgba(212,212,212,130); font-style: italic; }"
        )
        w.setPlaceholderText("digite um comando -- ex.: get chave, put grupo chave valor")
        w.returnPressed.connect(self._submit_client_command)
        w.textEdited.connect(self._on_cmd_text_edited)
        w.installEventFilter(self)
        w.hide()
        return w

    def _make_cmd_suggest_list(self) -> QListWidget:
        """Autocomplete estilo VSCode: enquanto o primeiro token do campo ainda está sendo
        digitado (sem espaço depois dele), mostra as entradas de _CMD_SUGGESTIONS cujo verbo
        começa com o que já foi digitado. Setinhas navegam, Tab/Enter aceita a destacada, clique
        também aceita. Aceitar preenche o campo com o molde e seleciona o primeiro
        <placeholder> -- Tab então passa a pular entre placeholders (ver eventFilter)."""
        w = QListWidget(self)
        w.setFont(QFont("Consolas", 9))
        w.setFocusPolicy(Qt.NoFocus)  # não tira o foco do campo (mesmo motivo dos chips antigos)
        w.setStyleSheet(
            "QListWidget { background: rgba(12,20,38,248); color: #D4D4D4; "
            "border: 1px solid rgba(88,216,255,0.6); border-radius: 4px; padding: 2px; "
            "outline: none; } "
            "QListWidget::item { padding: 3px 6px; border-radius: 3px; } "
            "QListWidget::item:selected { background: rgba(88,216,255,0.30); color: #FFFFFF; }"
        )
        w.setVerticalScrollBarPolicy(Qt.ScrollBarAsNeeded)
        w.setHorizontalScrollBarPolicy(Qt.ScrollBarAlwaysOff)
        w.itemClicked.connect(lambda item: self._accept_cmd_suggestion(item))
        w.hide()
        return w

    def _update_cmd_suggestions(self):
        """Recalcula a lista filtrada a partir do primeiro token do campo. Só mostra enquanto
        esse primeiro token ainda não terminou (sem espaço depois) -- uma vez que a pessoa já
        passou pro argumento seguinte, as sugestões de verbo não fazem mais sentido."""
        text = self._cmd_input.text()
        if " " in text:
            self._cmd_suggest.hide()
            return
        prefix = text.lower()
        matches = [(verb, template, desc) for verb, template, desc in _CMD_SUGGESTIONS
                   if verb.startswith(prefix)]
        self._cmd_suggest.clear()
        if not matches:
            self._cmd_suggest.hide()
            return
        for verb, template, desc in matches:
            item = QListWidgetItem(f"{verb:<9}{template}")
            item.setData(Qt.UserRole, template)
            item.setToolTip(desc)
            self._cmd_suggest.addItem(item)
        self._cmd_suggest.setCurrentRow(0)

        x, y, w = self._cmd_list_geom
        row_h = self._cmd_suggest.sizeHintForRow(0) + 2
        h = min(row_h * len(matches) + 6, 160)
        self._cmd_suggest.setGeometry(x, y, w, h)
        self._cmd_suggest.show()
        self._cmd_suggest.raise_()

    def _accept_cmd_suggestion(self, item: QListWidgetItem):
        """Aceita a sugestão (clique ou Tab/Enter com ela destacada): preenche o campo com o
        molde e seleciona o primeiro <placeholder>, em modo "opaco" até a primeira edição."""
        template = item.data(Qt.UserRole)
        self._cmd_suggest.hide()
        self._cmd_input.setText(template)
        self._cmd_input.setProperty("template", "true")
        self._cmd_input.style().unpolish(self._cmd_input)
        self._cmd_input.style().polish(self._cmd_input)
        self._cmd_is_template = True
        self._cmd_input.setFocus()
        self._select_placeholder(_PLACEHOLDER_RE.search(template))

    def _select_placeholder(self, match: "re.Match | None"):
        if match is None:
            self._cmd_input.setCursorPosition(len(self._cmd_input.text()))
            return
        self._cmd_input.setSelection(match.start(), match.end() - match.start())

    def _select_next_placeholder(self):
        """Tab: pula pro próximo <placeholder> depois da seleção/cursor atual -- dá a volta pro
        primeiro se não achar nenhum depois (snippet-like, sem precisar de rich text)."""
        text = self._cmd_input.text()
        after = self._cmd_input.selectionStart() + len(self._cmd_input.selectedText()) \
            if self._cmd_input.hasSelectedText() else self._cmd_input.cursorPosition()
        m = _PLACEHOLDER_RE.search(text, after)
        if m is None:
            m = _PLACEHOLDER_RE.search(text)  # volta pro primeiro
        self._select_placeholder(m)

    def _on_cmd_text_edited(self, _text: str):
        # Primeira edição em cima de um molde aceito: tira o modo "opaco" -- a partir daqui é
        # texto de verdade, não mais um molde não preenchido.
        if self._cmd_is_template:
            self._cmd_is_template = False
            self._cmd_input.setProperty("template", "false")
            self._cmd_input.style().unpolish(self._cmd_input)
            self._cmd_input.style().polish(self._cmd_input)
        self._update_cmd_suggestions()

    def eventFilter(self, obj, event):
        # getattr, não self._cmd_input direto: w.hide() dentro de _make_cmd_input já dispara
        # um QHideEvent passando por este filtro (installEventFilter roda antes do hide()),
        # num momento em que self._cmd_input ainda não foi atribuído (a atribuição só
        # acontece quando _make_cmd_input() retorna) -- AttributeError sem o getattr.
        if obj is getattr(self, "_cmd_input", None):
            suggest_open = self._cmd_suggest.isVisible() and self._cmd_suggest.count() > 0
            if event.type() == QEvent.KeyPress:
                key = event.key()
                if key == Qt.Key_Escape:
                    if suggest_open:
                        self._cmd_suggest.hide()
                    else:
                        self._cancel_client_command()
                    return True
                if suggest_open and key in (Qt.Key_Down, Qt.Key_Up):
                    row = self._cmd_suggest.currentRow()
                    n = self._cmd_suggest.count()
                    row = (row + (1 if key == Qt.Key_Down else -1)) % n
                    self._cmd_suggest.setCurrentRow(row)
                    return True
                if suggest_open and key in (Qt.Key_Tab, Qt.Key_Return, Qt.Key_Enter):
                    item = self._cmd_suggest.currentItem() or self._cmd_suggest.item(0)
                    self._accept_cmd_suggestion(item)
                    return True
                if key == Qt.Key_Tab:
                    # Lista fechada: Tab já não sugere verbo, pula pro próximo <placeholder>.
                    self._select_next_placeholder()
                    return True
            if event.type() == QEvent.FocusOut:
                # Clicar fora cancela (sem submeter) -- mesmo padrão de um editor transiente.
                # Clicar na lista de sugestões NÃO conta como "fora" (ela é NoFocus, ver
                # _make_cmd_suggest_list), então isso só dispara de verdade quando o foco vai
                # pra algo que não é nosso.
                self._cancel_client_command()
        return super().eventFilter(obj, event)

    def _open_client_command_input(self, client_id: int, pos: QPointF):
        """Balão único do cliente: clique simples já traz a info (nome, status -- as mesmas
        linhas de _inspect_client_lines, ao vivo) grudada em cima do campo de comando. Antes
        eram duas coisas separadas (clique simples = só inspeção, duplo clique = só campo);
        agora é tudo junto, aberto com um clique só."""
        self._cmd_target_client = client_id
        self._cmd_open_pos = pos
        self._cmd_is_template = False
        self._cmd_input.setProperty("error", "false")
        self._cmd_input.setProperty("template", "false")
        self._cmd_input.style().unpolish(self._cmd_input)
        self._cmd_input.style().polish(self._cmd_input)
        self._cmd_input.clear()
        w_canvas = self.width()
        prefix_w = self._cmd_prefix_label.fontMetrics().horizontalAdvance("orderingclient") + 10
        box_w = min(440, w_canvas - 20 - prefix_w)
        px = min(max(pos.x() + 15, 10), w_canvas - box_w - prefix_w - 10)
        input_y = int(pos.y() - 14)

        self._cmd_prefix_label.setGeometry(int(px), input_y, prefix_w, 28)
        self._cmd_input.setGeometry(int(px) + prefix_w, input_y, box_w, 28)
        self._cmd_prefix_label.show()
        self._cmd_input.show()
        self._cmd_prefix_label.raise_()
        self._cmd_input.raise_()
        self._cmd_input.setFocus()

        # Lista de sugestões logo ABAIXO do campo, mesma largura (prefixo + campo).
        self._cmd_list_geom = (int(px), input_y + 30, prefix_w + box_w)
        self._update_cmd_suggestions()  # mostra tudo de cara (campo vazio = prefixo "" casa com tudo)

        # Info do cliente (nome, status) grudada em cima da linha prefixo+campo -- ao vivo,
        # igual o popup de inspeção de nó já fazia, só que agora faz parte do mesmo balão.
        lines, _color = self._inspect_client_lines(client_id)
        if lines:
            anchor = QPointF(px, input_y)
            self._show_debug_popup(
                lines, pos, live=lambda: self._inspect_client_lines(client_id)[0],
                anchor_above=anchor, fixed_width=prefix_w + box_w,
            )

    def _cancel_client_command(self):
        self._hide_debug_popup()
        self._cmd_input.hide()
        self._cmd_prefix_label.hide()
        self._cmd_suggest.hide()
        self._cmd_target_client = None

    def _submit_client_command(self):
        if self._cmd_target_client is None:
            return
        text = self._cmd_input.text().strip()
        if not text:
            self._cancel_client_command()
            return

        # "ls"/"help" não existem como pedido de cliente no sistema real (help nem conecta; ls
        # não passa por consenso) -- não entram no pipeline de protocolo simulado, só mostram a
        # saída que o orderingclient de verdade imprimiria, no mesmo popup usado pra inspeção.
        local_lines = self.sim.local_command_output(text)
        if local_lines is not None:
            pos = self._cmd_open_pos
            self._cancel_client_command()
            self._show_debug_popup(local_lines, pos)
            return

        # "overload" também não é um pedido de cliente (não existe RequestInfo nem resposta) --
        # é controle de simulação: liga a geração automática de GET/TX por um tempo (fiel ao
        # sistema real, onde nada sai sozinho até "overload" ligar isso -- ver tick.py).
        overload_lines, overload_err = self.sim.handle_overload_command(text)
        if overload_err is not None:
            self._show_cmd_error(overload_err)
            return
        if overload_lines is not None:
            pos = self._cmd_open_pos
            self._cancel_client_command()
            self._show_debug_popup(overload_lines, pos)
            return

        err = self.sim.inject_client_command(self._cmd_target_client, text)
        if err:
            self._show_cmd_error(err)
            return
        self._cancel_client_command()

    def _show_cmd_error(self, err: str):
        self._cmd_input.setProperty("error", "true")
        self._cmd_input.style().unpolish(self._cmd_input)
        self._cmd_input.style().polish(self._cmd_input)
        self._cmd_input.selectAll()
        self.sim.info_text = f"Comando inválido: {err}"
        # Tooltip direto no campo -- a borda vermelha sozinha passa batido fácil, e
        # sim.info_text é um painel lateral que a pessoa pode nem estar olhando.
        QToolTip.showText(
            self._cmd_input.mapToGlobal(self._cmd_input.rect().bottomLeft()),
            err, self._cmd_input,
        )

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

    def _show_debug_popup(self, lines: list[str], pos: QPointF, live=None,
                           anchor_above: QPointF | None = None, fixed_width: int | None = None):
        # `live`: função sem argumentos que devolve as linhas atuais; enquanto o popup estiver
        # aberto, ele é reconstruído a cada mudança da simulação (em vez de ficar um retrato).
        # `anchor_above`/`fixed_width`: usado pelo balão unificado do cliente (ver
        # _open_client_command_input) -- em vez de centralizar em volta de `pos`, gruda o popup
        # bem em cima do campo de comando, com a MESMA largura dele (visualmente um balão só).
        self._live_fn = live
        self._live_pos = pos
        self._live_anchor_above = anchor_above
        self._live_fixed_width = fixed_width
        self._live_last = list(lines)
        self._live_tick = self.sim.tick_count
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
        popup_w = fixed_width if fixed_width is not None else min(ideal_w + 24, w_canvas - 20)
        doc.setTextWidth(popup_w - 12)
        popup_h = min(doc.size().height() + 16, h_canvas - 20)
        if anchor_above is not None:
            # Gruda o popup bem em cima do ponto dado (o campo de comando fica logo abaixo) --
            # mesma largura fixa, visualmente uma peça só.
            px = min(max(anchor_above.x(), 10), w_canvas - popup_w - 10)
            py = max(anchor_above.y() - popup_h - 4, 10)
        else:
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
        self._live_fn = None
        self._popup_widget.hide()

    def _refresh_live_popup(self):
        fn = getattr(self, '_live_fn', None)
        if fn is None or not self._popup_widget.isVisible():
            return
        if self.sim.tick_count == self._live_tick:
            return
        self._live_tick = self.sim.tick_count
        if self._popup_widget.textCursor().hasSelection():
            return  # não apaga o texto que o usuário está selecionando para copiar
        lines = fn()
        if lines and lines != self._live_last:
            self._show_debug_popup(lines, self._live_pos, live=fn,
                                    anchor_above=getattr(self, '_live_anchor_above', None),
                                    fixed_width=getattr(self, '_live_fixed_width', None))

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

    def _eye_at(self, pos: QPointF):
        """Id do grupo cujo olhinho está sob `pos` (coordenadas da tela), ou None."""
        scene = self._to_scene(pos)
        for gid, rect in self._eye_rects.items():
            if rect.contains(scene):
                return gid
        return None

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
        return QRectF()  # painel removido da tela: sem área de clique
        if self.sim.gsn <= 0 and not self.sim.meta_stream:
            return QRectF()  # painel oculto (sem pedidos cross-group)
        meta = self.sim.meta_stream
        visible_meta = meta[-5:] if meta else []
        n_entries = max(len(visible_meta), 1)
        box_w, box_x, box_y, line_h = 220, 10, 10, 16
        box_h = line_h * 3 + n_entries * 22 + 16
        return QRectF(box_x, box_y, box_w, box_h)

    def _adeliver_panel_rect(self) -> QRectF:
        """Retângulo do painel 'ADeliver (por grupo)' (mesma fórmula de
        draw_hud.py draw_adeliver_panel), usado para hit-test do clique."""
        return QRectF()  # painel removido da tela
        dlv = self.sim.delivery
        if (not dlv or not dlv._last_delivered_gsn
                or (self.sim.gsn <= 0 and not self.sim.meta_stream)):
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
            gid = self._eye_at(event.position())
            if gid is not None:
                # Olhinho ao lado do nome do grupo: mostra/esconde a região dele.
                self._hidden_groups.symmetric_difference_update({gid})
                self._eye_click = True
                self.update()
                event.accept()
                return
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
            self._cancel_client_command()  # arrastar fecha o balão (ele ficaria para trás)
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
            eye = self._eye_at(event.position()) is not None
            self.setCursor(Qt.PointingHandCursor if (hit or msg or panel_hit or eye) else Qt.ArrowCursor)
        super().mouseMoveEvent(event)

    def mouseReleaseEvent(self, event):
        if event.button() == Qt.LeftButton and self._eye_click:
            self._eye_click = False
            event.accept()
            return
        if event.button() == Qt.LeftButton:
            if self._dragging and not self._drag_moved:
                kind, id_str = self._dragging.split(":")
                if kind == "client":
                    # Balão único: info + campo de comando juntos, num clique só (ver
                    # _open_client_command_input) -- nó continua só com o popup de inspeção.
                    self._open_client_command_input(int(id_str), event.position())
                else:
                    self._open_inspect(self._dragging, event.position())
            elif not self._dragging and not self._panning:
                msg = self._hit_test_message(event.position())
                if msg:
                    self._open_msg_inspect(msg, event.position())
                elif self._gsn_panel_rect().contains(event.position()):
                    self._open_gsn_inspect(event.position())
                elif self._adeliver_panel_rect().contains(event.position()):
                    self._open_adeliver_inspect(event.position())
                elif self._hit_test(event.position()):
                    pass  # 2º release de um duplo-clique no mesmo nó/cliente -- já tratado acima
                else:
                    self._cancel_client_command()
            elif self._panning and not self._drag_moved:
                self._cancel_client_command()
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
        self._cancel_client_command()
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
        self._cancel_client_command()
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
        self._cancel_client_command()  # abrir outro popup fecha o balão do cliente, se tinha um
        kind, id_str = hit.split(":")
        id_ = int(id_str)
        if kind == "node":
            lines, color = self._inspect_node_lines(id_)
            live = lambda: self._inspect_node_lines(id_)[0]
        else:
            lines, color = self._inspect_client_lines(id_)
            live = lambda: self._inspect_client_lines(id_)[0]
        if lines:
            self._show_debug_popup(lines, pos, live=live)

    def _inspect_node_lines(self, node_id):
        node = self.sim.nodes[node_id] if node_id < len(self.sim.nodes) else None
        if not node:
            return [], C["accent"]
        groups = [f"G{g}" for g in node.groups]
        # Buckets pertencem ao GRUPO (todos os membros veem os mesmos buckets),
        # não a um nó específico — o líder de cada SN roda por rodízio dentro
        # do grupo, então não há "buckets deste nó" fixos para mostrar aqui.
        roles = []
        for inst in self.sim.instances.values():
            if inst.leader == node_id:
                n = len(inst.members)
                roles.append(f"\u2605 L\u00cdDER G{inst.group_id} \u00b7 SN {inst.sn}"
                             + (f" ({n} ped.)" if n else " (esperando pedidos)"))
        for (cid, csn), info in self.sim.pending_info.items():
            if info["proxy"] == node_id:
                roles.append(f"\U0001f4ec PROXY de {info['name']} #{csn}")
        member_of = [g.id for g in self.sim.groups if g.id != 0 and node_id in g.members]
        if member_of and not roles:
            roles.append("\U0001f465 PARTICIPANTE dos grupos " + ", ".join(f"G{g}" for g in member_of))
        role = "; ".join(roles) if roles else "Ocioso"
        lines = [
            f"\u2500\u2500 {node.name} \u2500\u2500",
            f"Estado: {'\U0001f7e2 Ativo' if node.is_alive else '\U0001f534 Falhou'}",
            f"Papel atual: {role}",
            f"Grupos: {', '.join(groups)}",
        ]
        lines += self._inspect_sequencer_state(node_id, member_of)
        return lines, C["accent"]

    def _inspect_sequencer_state(self, node_id, member_of):
        """Estado do Sequencer que CADA nó guarda em memória (sequencer.go): metadata,
        groupGSNQueue, lastDeliveredGSN e pendingCommits. O META é transmitido a todos os nós,
        então o simulador mantém uma só cópia, que vale para todos; a fila, a última entrega e os
        commits guardados aparecem só para os grupos DESTE nó."""
        sim = self.sim
        metas = list(sim.meta_stream)
        dlv = sim.delivery
        if not metas and not (dlv and dlv._last_delivered_gsn):
            return []
        out = ["", "── Sequencer (neste nó) ──",
               "metadata[gsn] = grupos que o GSN toca"]
        if metas:
            for m in metas[-5:]:
                out.append(f"  {m['gsn']} → " + ", ".join(f"G{g}" for g in m["groups"]))
            if len(metas) > 5:
                out.append(f"  (+{len(metas) - 5} anteriores)")
        else:
            out.append("  (vazio)")
        if dlv:
            for g in member_of:
                pend = dlv._pending.get(g, [])
                queue = ", ".join(f"{e.gsn}{'*' if e.committed else ''}" for e in pend) or "—"
                last = dlv._last_delivered_gsn.get(g, 0)
                blocked = dlv.get_blocked_entries(g)
                out.append(f"G{g}: groupGSNQueue = [{queue}]  (* = commit já decidido)")
                out.append(f"    lastDeliveredGSN = {last}")
                out.append("    pendingCommits = "
                           + (", ".join(f"gsn {e.gsn} (SN {e.sn})" for e in blocked) if blocked else "—"))
        return out

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
        self._cancel_client_command()
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

    def _msg_detail_lines(self, msg):
        digest = msg.batch_digest[:12] if msg.batch_digest else '?'
        if msg.msg_type == MsgType.COMMIT_NOTIFY:
            lines = ["--- Resposta ---", f"SN: {msg.sn}", f"Digest: {digest}"]
            # Se o payload é um comando da API (put/get/transfer), mostra o que
            # "orderingclient" REALMENTE imprimiria (found=.../OK...) em vez de só
            # "COMMITTED" -- ver engine/tick.py:simulate_api_response. api_snapshot (não
            # req_snapshot) porque é o payload do PEDIDO ESPECÍFICO que esta mensagem
            # responde, não da instância/batch inteiro (ver Message.api_snapshot).
            snap = msg.api_snapshot
            api_resp = None
            if snap:
                api_resp = self.sim.simulate_api_response(
                    snap["group_id"], snap["payload"], snap.get("result"))
            lines.extend(api_resp if api_resp is not None else ["COMMITTED"])
            return lines
        dispatch = {
            MsgType.PREPARE: ["--- MPxPrepare ---", f"SN: {msg.sn}", f"Ballot: {msg.ballot}", f"Leader: Node {msg.from_id}"],
            MsgType.PROMISE: ["--- MPxPromise ---", f"SN: {msg.sn}", f"Ballot: {msg.ballot}", "Ok: true"],
            MsgType.ACCEPT: ["--- MPxAccept ---", f"SN: {msg.sn}", f"Ballot: {msg.ballot}", f"Digest: {digest}"],
            MsgType.ACCEPTED: ["--- MPxAccepted ---", f"SN: {msg.sn}", f"Ballot: {msg.ballot}", "Ok: true"],
            MsgType.COMMIT: ["--- MPxCommit ---", f"SN: {msg.sn}", f"Digest: {digest}"],
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
        self._eye_rects = {}
        draw_groups(p, self.sim, self._node_pos, self._hidden_groups, self._eye_rects)
        draw_connections(p, self.sim, self._node_pos)
        draw_meta_broadcast_lines(p, self.sim, self._node_pos)
        draw_messages(p, self.sim, self._node_pos, self._client_pos)
        msg_color = get_active_msg_color(self.sim)
        draw_nodes(p, self.sim, self._node_pos, msg_color)
        draw_sequencer_glow(p, self.sim, self._node_pos)
        draw_thought_bubbles(p, self._thought_bubbles, self._node_pos, self._client_pos)
        draw_clients(p, self.sim, self._client_pos)
        p.restore()

        # Painéis 'GSN Sequencer' e 'ADeliver (por grupo)' removidos da tela (a pedido).
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

