"""Widgets glass — estilo Liquid Glass."""

from PySide6.QtWidgets import QWidget
from PySide6.QtCore import Qt, QRectF, QPointF
from PySide6.QtGui import (
    QPainter, QPainterPath, QColor, QLinearGradient, QRadialGradient,
    QPen, QBrush
)


class GlassPanel(QWidget):
    def __init__(self, parent=None, radius=20, tint=None, border_opacity=55):
        super().__init__(parent)
        self.setAttribute(Qt.WA_TranslucentBackground)
        self.setAutoFillBackground(False)
        self._radius = radius
        self._tint = QColor(tint) if tint else QColor(28, 46, 74, 200)
        self._border_opacity = border_opacity

    def paintEvent(self, event):
        p = QPainter(self)
        p.setRenderHint(QPainter.Antialiasing)
        w, h = self.width(), self.height()
        r = QRectF(1, 1, w - 2, h - 2)
        path = QPainterPath()
        path.addRoundedRect(r, self._radius, self._radius)
        p.setClipPath(path)
        p.fillPath(path, self._tint)
        # Gradient overlay
        grad = QLinearGradient(0, 0, 0, h)
        grad.setColorAt(0.0, QColor(255, 255, 255, 10))
        grad.setColorAt(0.5, QColor(255, 255, 255, 1))
        grad.setColorAt(1.0, QColor(0, 0, 0, 6))
        p.fillPath(path, QBrush(grad))
        # Specular highlight
        spec_h = min(self._radius * 1.6, h * 0.28)
        spec = QRectF(self._radius * 0.5, 1.5, w - self._radius, spec_h)
        sp = QPainterPath()
        sp.addRoundedRect(spec, self._radius * 0.6, self._radius * 0.6)
        sg = QLinearGradient(0, spec.top(), 0, spec.bottom())
        sg.setColorAt(0.0, QColor(255, 255, 255, 50))
        sg.setColorAt(0.5, QColor(255, 255, 255, 15))
        sg.setColorAt(1.0, QColor(255, 255, 255, 0))
        p.setPen(Qt.NoPen)
        p.fillPath(sp, QBrush(sg))
        p.setClipping(False)
        # Border
        bc = QColor(255, 255, 255)
        bc.setAlpha(self._border_opacity)
        p.setPen(QPen(bc, 1.0))
        p.drawPath(path)
        p.end()


class AmbientBackground(QWidget):
    _BG = QColor(0x04, 0x08, 0x14)

    def __init__(self, parent=None):
        super().__init__(parent)
        self.setAutoFillBackground(False)
        self.setAttribute(Qt.WA_OpaquePaintEvent)

    def paintEvent(self, event):
        p = QPainter(self)
        p.setRenderHint(QPainter.Antialiasing)
        p.setPen(Qt.NoPen)
        w, h = self.width(), self.height()
        base = max(w, h)
        p.fillRect(self.rect(), self._BG)

        def _radial(cx, cy, radius, r, g, b, alpha):
            gr = QRadialGradient(QPointF(cx, cy), radius)
            gr.setColorAt(0.0, QColor(r, g, b, alpha))
            gr.setColorAt(0.45, QColor(r, g, b, int(alpha * 0.4)))
            gr.setColorAt(1.0, QColor(r, g, b, 0))
            path = QPainterPath()
            path.addEllipse(QPointF(cx, cy), radius, radius)
            p.fillPath(path, gr)

        _radial(w * 0.15, h * 0.35, base * 0.5, 20, 80, 200, 28)
        _radial(w * 0.5, h * 0.9, base * 0.6, 60, 40, 200, 22)
        _radial(w * 0.82, h * 0.4, base * 0.45, 20, 120, 220, 20)
        _radial(w * 0.5, h * 0.45, base * 0.8, 40, 160, 255, 14)

        # Vignette
        vig = QRadialGradient(QPointF(w / 2, h / 2), max(w, h) * 0.68)
        vig.setColorAt(0.40, QColor(0, 0, 0, 0))
        vig.setColorAt(1.0, QColor(0, 0, 0, 130))
        p.setBrush(QBrush(vig))
        p.drawRect(self.rect())
        p.end()
