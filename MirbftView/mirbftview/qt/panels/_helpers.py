"""Helpers compartilhados entre os painéis."""

from PySide6.QtWidgets import QLabel
from PySide6.QtCore import Qt

from mirbftview.qt.theme import C


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
    lbl.setTextInteractionFlags(Qt.TextSelectableByMouse)
    return lbl
