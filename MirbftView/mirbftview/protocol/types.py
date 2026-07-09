"""Tipos compartilhados do protocolo ISS/MultiPaxos."""

from dataclasses import dataclass, field
from enum import Enum, auto
from typing import Optional


# ─── Fases da simulação ───────────────────────────────────────────────────────

class Phase(Enum):
    IDLE = auto()
    CLIENT_SEND = auto()
    BUCKET_ASSIGN = auto()
    BATCH_CUT = auto()
    GSN_ASSIGN = auto()
    PREPARE = auto()
    PROMISE = auto()
    ACCEPT = auto()
    ACCEPTED = auto()
    COMMIT = auto()
    COMMIT_NOTIFY = auto()
    ADELIVER = auto()
    CHECKPOINT = auto()
    EPOCH_TRANSITION = auto()
    VIEW_CHANGE = auto()
    RETRANSMIT = auto()         # NOVO: retransmissão após timeout
    DONE = auto()


# ─── Tipos de mensagem ────────────────────────────────────────────────────────

class MsgType(Enum):
    CLIENT_REQUEST = auto()
    GSN_REQUEST = auto()
    GSN_RESPONSE = auto()
    META_STREAM = auto()
    PREPARE = auto()
    PROMISE = auto()
    ACCEPT = auto()
    ACCEPTED = auto()
    COMMIT = auto()
    COMMIT_NOTIFY = auto()      # NOVO
    CHECKPOINT = auto()
    VIEW_CHANGE = auto()        # NOVO
    NEW_VIEW = auto()           # NOVO


# ─── Entidades da rede ────────────────────────────────────────────────────────

@dataclass
class Node:
    id: int
    name: str
    groups: list[int] = field(default_factory=list)
    is_alive: bool = True       # NOVO: para simular falhas


@dataclass
class Client:
    id: int
    name: str


@dataclass
class Group:
    id: int
    name: str
    members: list[int] = field(default_factory=list)


# ─── Mensagem visual ─────────────────────────────────────────────────────────

@dataclass
class Message:
    msg_type: MsgType
    from_id: int
    to_id: int
    from_is_client: bool = False
    to_is_client: bool = False      # NOVO: destino é um cliente
    progress: float = 0.0
    label: str = ""
    detail: str = ""
    segment_id: int = -1
    sn: int = -1                    # NOVO: SN associado
    ballot: int = -1                # NOVO: ballot associado
    batch_digest: str = ""          # NOVO: digest do batch
    color: str = ""                 # Cor fixa atribuída na criação


# ─── Segment (SNs intercalados) ──────────────────────────────────────────────

@dataclass
class SegmentInfo:
    """Um segmento do log — conforme mirmanager.go/skippingsegment.go.

    Cada líder recebe um segmento com SNs intercalados:
      snOffset, snOffset+distance, snOffset+2*distance, ...
    onde distance = número de líderes (parallelism).
    """
    seg_id: int
    leader: int
    sn_offset: int              # primeiro SN deste segmento
    sn_distance: int            # distância entre SNs (= nº de líderes)
    sn_length: int              # quantos SNs neste segmento
    followers: list[int] = field(default_factory=list)
    bucket_ids: list[int] = field(default_factory=list)
    batch_size: int = 4

    @property
    def sns(self) -> list[int]:
        """Lista de SNs deste segmento (skipping)."""
        return [self.sn_offset + i * self.sn_distance for i in range(self.sn_length)]

    @property
    def first_sn(self) -> int:
        return self.sn_offset

    @property
    def last_sn(self) -> int:
        return self.sn_offset + (self.sn_length - 1) * self.sn_distance


# ─── Request ─────────────────────────────────────────────────────────────────

@dataclass
class RequestInfo:
    """Informações completas de uma request sendo processada."""
    client_id: int = 0
    client_sn: int = 0
    payload: str = ""
    payload_hash: str = ""
    bucket_id: int = -1
    group_id: int = 0
    gsn: int = 0
    sn: int = 0
    segment_id: int = -1        # NOVO: segmento que processa esta request
    leader: int = -1
    ballot: int = 0
    quorum: int = 2
    promises_received: int = 0
    accepted_received: int = 0
    is_cross_group: bool = False
    touched_groups: list[int] = field(default_factory=list)
    batch_digest: str = ""
    proxy_node: int = -1        # NOVO: nó que recebeu do cliente (proxy)
    batch_requests: int = 1     # NOVO: quantas requests no batch
    phase: "Phase" = None       # fase individual desta request (pipeline)
    color: str = ""              # cor unica desta request (para barra de progresso)
    _retransmit_done: bool = False  # controle de retransmissão (pipeline)
    _failure_done: bool = False     # controle de falha simulada (pipeline)


# ─── Event Log ────────────────────────────────────────────────────────────────

@dataclass
class EventLog:
    """Um evento no log visual."""
    phase: Phase
    title: str
    detail: str
    color_key: str = "text"
