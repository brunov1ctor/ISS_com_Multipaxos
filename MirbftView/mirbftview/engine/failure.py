"""Queda definitiva de nó (crash-stop), usada pelos cenários de falha.

Um nó que cai para de responder até o Reset: não envia nem recebe mensagens. Como no Go:
- seguidor caído: o grupo segue com a maioria restante;
- líder de grupo de dados caído: o grupo para (não há troca de líder nos grupos de dados);
- líder do sequenciador (G0) caído: o G0 elege outro (engine/sequencer.py).
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
    """Derruba o nó `nid` de vez. Instâncias que ele liderava param (ver tick._advance)."""
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
    groups = sorted(g.id for g in st.groups if nid in g.members)
    st.log_event(Phase.VIEW_CHANGE, f"Node {nid} CAIU",
                 f"{reason}\n"
                 f"Node {nid} parou de responder (crash-stop): fica fora ate o Reset.\n"
                 f"Grupos afetados: {['G' + str(g) for g in groups]}"
                 + (f"\nEra lider de {['G' + str(g) for g in led]}: o grupo para "
                    f"(o Go nao troca de lider nos grupos de dados)." if led else ""),
                 "red")


def has_quorum(st, group_id: int) -> bool:
    members = st.groups[group_id].members
    return len(alive(st, members)) >= len(members) // 2 + 1
