"""Queda definitiva de nó (crash-stop), usada pelos cenários de falha.

Um nó que cai para de responder até o Reset: não é mais líder (de grupo de dados nem do
sequenciador), não envia nem recebe mensagens, e seus grupos seguem com a maioria restante.
Na tela ele treme, desbota e some (ver qt/canvas/draw_nodes.py).
"""

from mirbftview.protocol.types import Phase

SHAKE_TICKS = 45      # tremendo
FADE_TICKS = 55       # desbotando, depois some


def is_down(st, nid) -> bool:
    return nid in getattr(st, "down_nodes", set())


def alive(st, ids):
    return [n for n in ids if not is_down(st, n)]


def kill_node(st, nid: int, reason: str):
    """Derruba o nó `nid` de vez. Instâncias que ele liderava fazem view change."""
    if is_down(st, nid):
        return
    st.down_nodes.add(nid)
    st.fail_anim[nid] = st.tick_count
    for n in st.nodes:
        if n.id == nid:
            n.is_alive = False
    if getattr(st, "seq_down", None) is not None:
        st.seq_down.add(nid)
    st.epoch_mgr.down = st.down_nodes
    try:
        st.view_change_mgr.kill_node(nid)
    except Exception:
        pass
    led = sorted(i.group_id for i in st.instances.values() if i.leader == nid)
    for inst in st.instances.values():
        if inst.leader == nid and inst.phase not in (Phase.DONE, Phase.VIEW_CHANGE):
            inst.phase = Phase.VIEW_CHANGE  # _handle_special devolve o batch e encerra a posição
    groups = sorted(g.id for g in st.groups if nid in g.members)
    st.log_event(Phase.VIEW_CHANGE, f"Node {nid} CAIU",
                 f"{reason}\n"
                 f"Node {nid} parou de responder (crash-stop): fica fora ate o Reset.\n"
                 f"Grupos afetados: {['G' + str(g) for g in groups]}"
                 + (f"\nEra lider de {['G' + str(g) for g in led]}: view change." if led else ""),
                 "red")


def has_quorum(st, group_id: int) -> bool:
    members = st.groups[group_id].members
    return len(alive(st, members)) >= len(members) // 2 + 1
