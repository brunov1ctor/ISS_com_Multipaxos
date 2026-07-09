"""Constantes e cores usadas pelo canvas."""

from mirbftview.qt.theme import C
from mirbftview.qt.simulation import MsgType

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

MSG_COLOR_POOL = [
    "#F97316", "#38BDF8", "#A855F7", "#34D399", "#FACC15",
    "#F43F5E", "#818CF8", "#FB923C", "#2DD4BF", "#E879F9",
    "#84CC16", "#F472B6", "#22D3EE", "#FBBF24", "#6366F1",
]

GROUP_COLORS = ["#00E5FF", "#FF6D00", "#76FF03", "#D500F9", "#FFEA00", "#F50057", "#00E676", "#651FFF"]
