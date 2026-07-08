"""Ordering — Fases do consenso MultiPaxos para um slot.

Baseado em orderer/multipaxosinstance.go.

Conceito-chave:
  - Cada slot (SN) passa por: PREPARE → PROMISE → ACCEPT → ACCEPTED → COMMIT
  - No MultiPaxos estável, PREPARE/PROMISE são feitos uma vez por segmento
    (o líder mantém liderança para todos os SNs do segmento).
  - Aqui mostramos todas as fases para fins educacionais.
"""

from dataclasses import dataclass, field
from enum import Enum, auto


class SlotState(Enum):
    EMPTY = auto()
    PREPARING = auto()
    PROMISED = auto()
    ACCEPTING = auto()
    ACCEPTED = auto()
    COMMITTED = auto()


@dataclass
class SlotInstance:
    """Estado de consenso para um único slot (SN)."""
    sn: int
    segment_id: int
    leader: int
    ballot: int
    group_members: list[int] = field(default_factory=list)
    state: SlotState = SlotState.EMPTY
    batch_digest: str = ""
    promises: set[int] = field(default_factory=set)
    accepted: set[int] = field(default_factory=set)

    @property
    def quorum(self) -> int:
        return len(self.group_members) // 2 + 1

    @property
    def has_promise_quorum(self) -> bool:
        return len(self.promises) >= self.quorum

    @property
    def has_accepted_quorum(self) -> bool:
        return len(self.accepted) >= self.quorum

    def receive_promise(self, from_node: int) -> bool:
        """Recebe PROMISE de um nó. Retorna True se quorum atingido."""
        self.promises.add(from_node)
        if self.has_promise_quorum and self.state == SlotState.PREPARING:
            self.state = SlotState.PROMISED
            return True
        return False

    def receive_accepted(self, from_node: int) -> bool:
        """Recebe ACCEPTED de um nó. Retorna True se quorum atingido."""
        self.accepted.add(from_node)
        if self.has_accepted_quorum and self.state == SlotState.ACCEPTING:
            self.state = SlotState.ACCEPTED
            return True
        return False

    def start_prepare(self):
        self.state = SlotState.PREPARING

    def start_accept(self, digest: str):
        self.batch_digest = digest
        self.state = SlotState.ACCEPTING

    def commit(self):
        self.state = SlotState.COMMITTED
