"""Re-export — mantém compatibilidade com imports existentes na UI.

A lógica real agora vive em mirbftview.engine e mirbftview.protocol.
"""

# Re-exporta tudo que a UI usa
from mirbftview.engine import Simulation
from mirbftview.protocol.types import (
    Phase,
    MsgType,
    Node,
    Client,
    Group,
    Message,
    RequestInfo,
    EventLog,
    SegmentInfo,
)

__all__ = [
    "Simulation",
    "Phase",
    "MsgType",
    "Node",
    "Client",
    "Group",
    "Message",
    "RequestInfo",
    "EventLog",
    "SegmentInfo",
]
