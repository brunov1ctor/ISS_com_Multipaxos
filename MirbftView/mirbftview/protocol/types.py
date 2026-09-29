"""Tipos compartilhados do protocolo ISS/MultiPaxos."""

from dataclasses import dataclass, field
from enum import Enum, auto
from typing import Callable, Optional


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
    VIEW_CHANGE = auto()
    RETRANSMIT = auto()         # NOVO: retransmissão após timeout
    DONE = auto()
    SCENARIO = auto()           # NOVO: evento de log de ligar/desligar cenário (não é fase da simulação)


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
    # Snapshot dos campos do ClientRequest real (protobuf) no instante em que
    # esta mensagem foi emitida — permite o popup mostrar a evolução da
    # struct como um teste de mesa visual dinâmico.
    req_snapshot: Optional[dict] = None
    # Mensagem que só começa a voar depois que esta (e todas as demais em
    # trânsito) chegarem: encadeia dois saltos, ex.: líder -> proxy e depois
    # proxy -> cliente (COMMIT_NOTIFY_BATCH seguido de RespondToClient).
    then: object = None   # Message ou lista de Message (segundo salto encadeado)
    # Chamado UMA vez quando a mensagem chega ao destino (tick.py): é assim que cada nó
    # recebe a sua cópia do pedido no bucket local só na chegada da mensagem dele.
    on_arrive: Optional[Callable] = None
    fired: bool = False


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
    # "request": pedido de cliente (CLIENT_SEND -> GSN -> BUCKET_ASSIGN; depois espera nos
    #            buckets até algum líder cortá-lo num batch).
    # "instance": UMA instância de Paxos de um grupo (um SN): PREPARE -> PROMISE -> corte do
    #            batch -> ACCEPT -> ... -> NOTIFY. Cada grupo roda uma instância por vez.
    kind: str = "request"
    # Só em instâncias: os pedidos que entraram no batch (dicts com client_id, client_sn,
    # proxy, color, gsn, cross, label, payload).
    members: list = field(default_factory=list)


def key_to_group(key: str, num_data_groups: int) -> int:
    """Mesmo algoritmo de keyToGroup em request.go:357-371:
    1 + (CRC32(key) % numDataGroups) — grupos numerados 1..numDataGroups,
    excluindo o grupo 0 (sequenciador). zlib.crc32 usa o mesmo polinômio
    IEEE 802.3 que crc32.ChecksumIEEE do Go, então o resultado é idêntico
    byte a byte para a mesma chave."""
    import zlib
    if not key or num_data_groups < 1:
        return 1
    return 1 + (zlib.crc32(key.encode()) % num_data_groups)


def parse_cross_op_group_weights(spec: str) -> list[tuple[int, int]]:
    """Espelha parseCrossOpGroupWeights (config.go): "2:70,3:25,4:5" -> lista de
    (nº de grupos, limiar acumulado). Entradas com < 2 grupos ou peso <= 0 são
    ignoradas; spec vazia/inválida cai em "2:100" (todo cross-op toca 2 grupos)."""
    parsed: list[tuple[int, int]] = []
    cumulative = 0
    for part in (spec or "").split(","):
        part = part.strip()
        if not part or ":" not in part:
            continue
        g_s, w_s = part.split(":", 1)
        try:
            groups, weight = int(g_s.strip()), int(w_s.strip())
        except ValueError:
            continue
        if groups < 2 or weight <= 0:
            continue
        cumulative += weight
        parsed.append((groups, cumulative))
    return parsed or [(2, 1)]


def pick_cross_op_group_count(seq_nr: int, cumulative: list[tuple[int, int]]) -> int:
    """Espelha PickCrossOpGroupCount (config.go): escolha determinística de quantos
    grupos o cross-op de número `seq_nr` toca, por CRC32("crossopgroups-<seqNr>")."""
    import zlib
    h = zlib.crc32(f"crossopgroups-{seq_nr}".encode())
    pick = h % cumulative[-1][1]
    for groups, threshold in cumulative:
        if pick < threshold:
            return groups
    return cumulative[-1][0]


def snapshot_client_request(req: "RequestInfo") -> dict:
    """Espelha os 7 campos reais de protobufs.ClientRequest (request.pb.go)
    no instante atual desta request, para o popup de mensagem mostrar a
    struct evoluindo como um teste de mesa visual dinâmico.

    Pubkey/Signature não são criptografia real (o simulador não assina nada);
    são derivadas de forma determinística só para preencher os campos e
    deixar claro, no rótulo, que são ilustrativas.
    """
    import hashlib
    pk = hashlib.sha256(f"pubkey:{req.client_id}".encode()).hexdigest()[:16]
    sig = hashlib.sha256(f"sig:{req.client_id}:{req.client_sn}:{req.payload}".encode()).hexdigest()[:16]
    return {
        "client_id": req.client_id,
        "client_sn": req.client_sn,
        "payload": req.payload,
        "pubkey": pk,
        "signature": sig,
        "group_id": req.group_id,
        "touched_groups": list(req.touched_groups),
        "gsn": req.gsn,
    }


# ─── Event Log ────────────────────────────────────────────────────────────────

@dataclass
class EventLog:
    """Um evento no log visual."""
    phase: Phase
    title: str
    detail: str
    color_key: str = "text"
    req_color: str = ""
