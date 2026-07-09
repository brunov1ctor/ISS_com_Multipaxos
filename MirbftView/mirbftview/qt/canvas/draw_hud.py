"""HUD panels — GSN/META, ADeliver, progress bar."""

from PySide6.QtCore import Qt, QPointF, QRectF
from PySide6.QtGui import (
    QPainter, QPainterPath, QColor, QRadialGradient, QPen, QBrush, QFont
)
from mirbftview.qt.theme import C
from mirbftview.qt.simulation import Phase


_PROGRESS_STEPS_SETUP = [
    (Phase.PREPARE,       "Prepare",   C["phase_prepare"]),
    (Phase.PROMISE,       "Promise",   C["phase_promise"]),
    (Phase.CLIENT_SEND,   "Request",   C["orange"]),
    (Phase.BUCKET_ASSIGN, "Bucket",    C["gold"]),
    (Phase.ACCEPT,        "Accept",    C["phase_accept"]),
    (Phase.ACCEPTED,      "Accepted",  C["phase_accepted"]),
    (Phase.COMMIT,        "Commit",    C["phase_commit"]),
    (Phase.COMMIT_NOTIFY, "Notify",    C["green"]),
]

_PROGRESS_STEPS_STEADY = [
    (Phase.CLIENT_SEND,   "Request",   C["orange"]),
    (Phase.BUCKET_ASSIGN, "Bucket",    C["gold"]),
    (Phase.ACCEPT,        "Accept",    C["phase_accept"]),
    (Phase.ACCEPTED,      "Accepted",  C["phase_accepted"]),
    (Phase.COMMIT,        "Commit",    C["phase_commit"]),
    (Phase.COMMIT_NOTIFY, "Notify",    C["green"]),
]


def draw_gsn_meta_panel(p, sim):
    """Painel visual do Sequenciador Global.

    Mostra de forma intuitiva:
    - O 'cartorio' que numera pedidos multi-grupo
    - Senhas sendo distribuidas (GSN)
    - Quais grupos estao envolvidos (bolinhas coloridas)
    """
    meta = sim.meta_stream
    gsn_val = sim.gsn
    box_w, box_x, box_y, line_h = 220, 10, 10, 16
    visible_meta = meta[-5:] if meta else []
    # Altura: titulo + status + separador + senhas visuais
    n_entries = max(len(visible_meta), 1)
    box_h = line_h * 3 + n_entries * 22 + 16

    # Background
    bg = QPainterPath()
    bg.addRoundedRect(QRectF(box_x, box_y, box_w, box_h), 10, 10)
    p.fillPath(bg, QColor(10, 18, 32, 230))
    border_c = QColor("#A855F7")
    border_c.setAlpha(100)
    p.setPen(QPen(border_c, 1.2))
    p.setBrush(Qt.NoBrush)
    p.drawPath(bg)

    cy = box_y + 8

    # Titulo com icone
    p.setPen(QColor("#A855F7"))
    p.setFont(QFont("Segoe UI", 9, QFont.Bold))
    p.drawText(QRectF(box_x + 10, cy, box_w - 20, line_h),
               Qt.AlignLeft | Qt.AlignVCenter, "GSN Sequencer")
    cy += line_h

    # Status: quantas senhas distribuidas
    seq_id = min(sim.groups[0].members) if sim.groups and sim.groups[0].members else 0
    p.setPen(QColor(C["text2"]))
    p.setFont(QFont("Segoe UI", 7))
    if gsn_val == 0:
        status = "Nenhum pedido multi-grupo ainda"
    else:
        status = f"{gsn_val} senha(s) distribuida(s)"
    p.drawText(QRectF(box_x + 10, cy, box_w - 20, line_h),
               Qt.AlignLeft | Qt.AlignVCenter, status)
    cy += line_h + 4

    # Separador
    p.setPen(QPen(QColor(255, 255, 255, 30), 1))
    p.drawLine(QPointF(box_x + 12, cy), QPointF(box_x + box_w - 12, cy))
    cy += 6

    # Senhas visuais (como tickets)
    if visible_meta:
        group_colors = ["#4FC3F7", "#66BB6A", "#FFA726", "#AB47BC", "#EF5350"]
        for entry in reversed(visible_meta):
            is_latest = (entry == visible_meta[-1])
            ticket_h = 18
            ticket_rect = QRectF(box_x + 10, cy, box_w - 20, ticket_h)

            # Fundo do ticket
            ticket_bg = QPainterPath()
            ticket_bg.addRoundedRect(ticket_rect, 4, 4)
            bg_alpha = 40 if is_latest else 20
            p.fillPath(ticket_bg, QColor(168, 85, 247, bg_alpha))

            # Numero da senha (GSN)
            p.setPen(QColor("#E1BEE7") if is_latest else QColor(C["text3"]))
            p.setFont(QFont("Consolas", 8, QFont.Bold if is_latest else QFont.Normal))
            p.drawText(QRectF(box_x + 14, cy, 36, ticket_h),
                       Qt.AlignLeft | Qt.AlignVCenter, f"#{entry['gsn']}")

            # Bolinhas dos grupos envolvidos
            dot_x = box_x + 52
            for gid in entry["groups"]:
                gc = QColor(group_colors[gid % len(group_colors)])
                p.setPen(Qt.NoPen)
                p.setBrush(gc)
                p.drawEllipse(QPointF(dot_x, cy + ticket_h / 2), 5, 5)
                # Label do grupo dentro da bolinha
                p.setPen(QColor(0, 0, 0))
                p.setFont(QFont("Segoe UI", 5, QFont.Bold))
                p.drawText(QRectF(dot_x - 5, cy, 10, ticket_h),
                           Qt.AlignCenter, str(gid))
                dot_x += 14

            # Seta e descricao
            p.setPen(QColor(C["text2"]) if is_latest else QColor(C["text3"]))
            p.setFont(QFont("Segoe UI", 7))
            grp_names = " e ".join(f"G{g}" for g in entry["groups"])
            p.drawText(QRectF(dot_x + 4, cy, box_w - dot_x - 14, ticket_h),
                       Qt.AlignLeft | Qt.AlignVCenter, grp_names)

            cy += ticket_h + 4
    else:
        # Estado vazio — mensagem amigavel
        p.setPen(QColor(C["text3"]))
        p.setFont(QFont("Segoe UI", 7))
        p.drawText(QRectF(box_x + 10, cy, box_w - 20, 20),
                   Qt.AlignLeft | Qt.AlignVCenter,
                   "Aguardando pedidos multi-grupo...")
        cy += 20


def draw_adeliver_panel(p, sim):
    """Painel visual de entrega atômica.

    Mostra de forma intuitiva:
    - Cada grupo como uma 'fila de entrega'
    - Status: entregando normalmente ou bloqueado
    - Barra visual de progresso por grupo
    """
    dlv = sim.delivery
    if not dlv:
        return
    groups_with_data = sorted(dlv._last_delivered_gsn.keys())
    if not groups_with_data:
        return

    # Calcula posicao abaixo do painel GSN
    meta = sim.meta_stream
    visible_meta = meta[-5:] if meta else []
    n_entries = max(len(visible_meta), 1)
    gsn_box_h = 16 * 3 + n_entries * 22 + 16

    box_w, box_x, line_h = 220, 10, 18
    box_y = 10 + gsn_box_h + 8
    box_h = line_h + len(groups_with_data) * 24 + 12

    # Background
    bg = QPainterPath()
    bg.addRoundedRect(QRectF(box_x, box_y, box_w, box_h), 10, 10)
    p.fillPath(bg, QColor(10, 18, 32, 230))
    border_c = QColor(C["green"])
    border_c.setAlpha(80)
    p.setPen(QPen(border_c, 1.2))
    p.setBrush(Qt.NoBrush)
    p.drawPath(bg)

    cy = box_y + 8

    # Titulo
    p.setPen(QColor(C["green"]))
    p.setFont(QFont("Segoe UI", 9, QFont.Bold))
    p.drawText(QRectF(box_x + 10, cy, box_w - 20, line_h),
               Qt.AlignLeft | Qt.AlignVCenter, "ADeliver (por grupo)")
    cy += line_h + 4

    group_colors = ["#4FC3F7", "#66BB6A", "#FFA726", "#AB47BC", "#EF5350"]

    for gid in groups_with_data:
        next_gsn = dlv.get_next_expected_gsn(gid)
        blocked = dlv.get_blocked_entries(gid)
        last = dlv._last_delivered_gsn.get(gid, 0)
        is_blocked = len(blocked) > 0

        row_h = 20
        row_rect = QRectF(box_x + 10, cy, box_w - 20, row_h)

        # Fundo da linha
        row_bg = QPainterPath()
        row_bg.addRoundedRect(row_rect, 4, 4)
        if is_blocked:
            p.fillPath(row_bg, QColor(239, 83, 80, 25))
        else:
            p.fillPath(row_bg, QColor(102, 187, 106, 15))

        # Bolinha do grupo
        gc = QColor(group_colors[gid % len(group_colors)])
        p.setPen(Qt.NoPen)
        p.setBrush(gc)
        p.drawEllipse(QPointF(box_x + 22, cy + row_h / 2), 6, 6)
        p.setPen(QColor(0, 0, 0))
        p.setFont(QFont("Segoe UI", 5, QFont.Bold))
        p.drawText(QRectF(box_x + 16, cy, 12, row_h), Qt.AlignCenter, str(gid))

        # Status icon + texto
        if is_blocked:
            icon = "\U0001f6d1"  # stop sign
            status_text = f"Esperando #{next_gsn}"
            text_color = QColor(C["red"])
        else:
            icon = "\u2705"
            status_text = f"Entregue ate #{last}" if last > 0 else "Pronta"
            text_color = QColor(C["text"])

        p.setPen(text_color)
        p.setFont(QFont("Segoe UI", 7))
        p.drawText(QRectF(box_x + 34, cy, box_w - 50, row_h),
                   Qt.AlignLeft | Qt.AlignVCenter, f"G{gid}: {status_text}")

        # Indicador de bloqueio (quantos esperando)
        if is_blocked:
            p.setPen(QColor(C["red"]))
            p.setFont(QFont("Segoe UI", 6, QFont.Bold))
            p.drawText(QRectF(box_x + box_w - 50, cy, 36, row_h),
                       Qt.AlignRight | Qt.AlignVCenter, f"{len(blocked)} na fila")

        cy += row_h + 4


def draw_progress_bar(p, sim, canvas_w, canvas_h):
    """Desenha uma barra de progresso por request ativa.

    Cada barra tem a cor da request e mostra em qual fase ela esta.
    Usa steps de setup (com PREPARE/PROMISE) ou steady-state (sem).
    """
    active = sim.active_requests
    if not active:
        return

    # Escolhe steps baseado no estado prepared
    is_steady = getattr(sim, 'prepared', False)
    steps = _PROGRESS_STEPS_STEADY if is_steady else _PROGRESS_STEPS_SETUP
    n = len(steps)
    margin_x = 20
    available_w = canvas_w - margin_x * 2
    step_w = available_w / n

    bar_h_each = 18
    spacing = 4
    total_bars = len(active)
    total_h = total_bars * bar_h_each + (total_bars - 1) * spacing + 16
    bar_y_start = canvas_h - total_h - 8

    # Background
    bg_rect = QRectF(margin_x - 6, bar_y_start - 4, available_w + 12, total_h + 8)
    bg_path = QPainterPath()
    bg_path.addRoundedRect(bg_rect, 12, 12)
    p.fillPath(bg_path, QColor(10, 18, 32, 210))
    p.setPen(QPen(QColor(255, 255, 255, 30), 1))
    p.setBrush(Qt.NoBrush)
    p.drawPath(bg_path)

    # Desenha cada barra
    for bar_idx, req in enumerate(active):
        bar_y = bar_y_start + bar_idx * (bar_h_each + spacing)
        req_color = req.color if hasattr(req, 'color') and req.color else _get_req_color(bar_idx)

        # Determina indice da fase atual desta request
        current_idx = -1
        for i, (step_phase, _, _) in enumerate(steps):
            if step_phase == req.phase:
                current_idx = i
                break
        if current_idx == -1 and req.phase == Phase.DONE:
            current_idx = n

        # Linha de fundo (track)
        track_y = bar_y + bar_h_each / 2
        p.setPen(QPen(QColor(255, 255, 255, 25), 2))
        p.drawLine(QPointF(margin_x, track_y), QPointF(margin_x + available_w, track_y))

        # Linha de progresso preenchida
        if current_idx > 0:
            fill_w = step_w * current_idx
            p.setPen(QPen(QColor(req_color), 3))
            p.drawLine(QPointF(margin_x, track_y), QPointF(margin_x + fill_w, track_y))

        # Bolinhas das fases
        for i, (step_phase, label, _) in enumerate(steps):
            cx = margin_x + step_w * i + step_w / 2
            cy = track_y
            is_done = i < current_idx
            is_current = i == current_idx

            radius = 5 if is_current else 3
            if is_current:
                # Glow
                glow = QRadialGradient(QPointF(cx, cy), 10)
                gc = QColor(req_color)
                gc.setAlpha(100)
                glow.setColorAt(0.0, gc)
                glow.setColorAt(1.0, QColor(0, 0, 0, 0))
                p.setPen(Qt.NoPen)
                p.setBrush(QBrush(glow))
                p.drawEllipse(QPointF(cx, cy), 10, 10)

            p.setPen(Qt.NoPen)
            if is_done or is_current:
                p.setBrush(QColor(req_color))
            else:
                p.setBrush(QColor(C["text3"]))
            p.drawEllipse(QPointF(cx, cy), radius, radius)

        # Label da fase atual (a direita da barra)
        if 0 <= current_idx < n:
            phase_label = steps[current_idx][1]
        elif current_idx >= n:
            phase_label = "Pronto"
        else:
            phase_label = "?"
        p.setPen(QColor(req_color))
        p.setFont(QFont("Segoe UI", 7, QFont.Bold))
        p.drawText(
            QRectF(margin_x + available_w + 4, bar_y, 60, bar_h_each),
            Qt.AlignLeft | Qt.AlignVCenter, phase_label
        )

        # Indicador de grupo (a esquerda)
        p.setPen(QColor(req_color))
        p.setFont(QFont("Segoe UI", 7))
        p.drawText(
            QRectF(margin_x - 40, bar_y, 34, bar_h_each),
            Qt.AlignRight | Qt.AlignVCenter, f"G{req.group_id}"
        )

    # Labels das fases (so uma vez, embaixo)
    label_y = bar_y_start + total_bars * (bar_h_each + spacing)
    p.setPen(QColor(C["text3"]))
    p.setFont(QFont("Segoe UI", 6))
    for i, (_, label, _) in enumerate(steps):
        cx = margin_x + step_w * i + step_w / 2
        p.drawText(QRectF(cx - step_w / 2, label_y, step_w, 12), Qt.AlignCenter, label)


def _get_req_color(idx: int) -> str:
    """Cor fallback para request sem cor atribuida."""
    from mirbftview.qt.canvas._constants import MSG_COLOR_POOL
    return MSG_COLOR_POOL[idx % len(MSG_COLOR_POOL)]
