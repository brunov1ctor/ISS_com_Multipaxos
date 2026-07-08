"""Theme — Liquid Glass Design System (paleta azul/roxo fria)."""

C = {
    "bg":           "rgba(11,18,32,0.0)",
    "panel":        "rgba(17,24,39,0.0)",
    "card":         "rgba(28,46,74,0.55)",
    "card_hover":   "rgba(36,58,94,0.65)",
    "input":        "rgba(10,16,30,0.70)",
    "border":       "rgba(255,255,255,0.10)",
    "glass":        "rgba(28,46,74,0.55)",
    "glass_hover":  "rgba(36,58,94,0.65)",
    "glass_border": "rgba(255,255,255,0.55)",
    "primary":   "#6C63FF",
    "secondary": "#8B7DFF",
    "accent":    "#58D8FF",
    "text":  "#F3F6FF",
    "text2": "#A9B4C8",
    "text3": "#6E7A91",
    "red":       "#EF4444",
    "green":     "#34D399",
    "gold":      "#FACC15",
    "orange":    "#F97316",
    "dark":      "#0B1220",
    # Protocol phases
    "phase_prepare":  "#FACC15",
    "phase_promise":  "#58D8FF",
    "phase_accept":   "#6C63FF",
    "phase_accepted": "#8B7DFF",
    "phase_commit":   "#34D399",
    # Node colors
    "node_leader":   "#FACC15",
    "node_follower": "#58D8FF",
    "node_idle":     "#6E7A91",
    # Group colors
    "group_0": "#EF4444",
    "group_1": "#6C63FF",
    "group_2": "#34D399",
    "group_3": "#F97316",
}

STYLESHEET = """
QMainWindow { background-color: #040814; }
QWidget { background-color: transparent; color: #F3F6FF; font-family: "Segoe UI"; font-size: 10pt; }
QLabel { background: transparent; border: none; color: #F3F6FF; }
QPushButton {
    background-color: rgba(28,46,74,0.55); color: #F3F6FF;
    border: 1px solid rgba(255,255,255,0.12); border-radius: 10px;
    padding: 6px 14px; font-weight: bold;
}
QPushButton:hover { background-color: rgba(36,58,94,0.70); border-color: rgba(255,255,255,0.22); color: #8B7DFF; }
QPushButton:pressed { background-color: rgba(44,60,90,0.80); border-color: #6C63FF; }
QPushButton#primary { background-color: #6C63FF; color: #0B1220; border: 1px solid #8B7DFF; border-radius: 10px; }
QPushButton#primary:hover { background-color: #8B7DFF; border-color: #58D8FF; }
QSlider::groove:horizontal { background: #1A2336; height: 4px; border-radius: 2px; }
QSlider::handle:horizontal { background: #6C63FF; width: 14px; height: 14px; margin: -5px 0; border-radius: 7px; border: 2px solid #8B7DFF; }
QSlider::sub-page:horizontal { background: qlineargradient(x1:0,y1:0,x2:1,y2:0, stop:0 #6C63FF, stop:1 #58D8FF); border-radius: 2px; }
QScrollBar:vertical { background: rgba(10,16,30,0.30); width: 8px; border-radius: 4px; }
QScrollBar::handle:vertical { background: rgba(28,46,74,0.60); min-height: 30px; border-radius: 4px; }
QScrollBar::handle:vertical:hover { background: #6C63FF; }
QScrollBar::add-line:vertical, QScrollBar::sub-line:vertical { height: 0; }
QScrollBar::add-page:vertical, QScrollBar::sub-page:vertical { background: transparent; }
QGraphicsView { background-color: transparent; border: none; }
QToolTip { background-color: rgba(20,32,55,0.92); color: #A9B4C8; border: 1px solid rgba(255,255,255,0.14); border-radius: 10px; padding: 8px 12px; }
"""
