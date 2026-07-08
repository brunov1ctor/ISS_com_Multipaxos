"""Batch — Lógica de CutBatch (acumular requests até size ou timeout).

Baseado em request/bucketgroup.go CutBatch().

Conceito-chave:
  - O líder NÃO propõe cada request individualmente.
  - Ele acumula requests no bucket até atingir batch_size OU batch_timeout.
  - Depois corta o batch (CutBatch) e propõe o batch inteiro num único slot.
  - Cross-ops são propostas imediatamente (batch de 1, sem espera).
"""

import hashlib
from dataclasses import dataclass, field


@dataclass
class PendingRequest:
    """Uma request pendente no bucket aguardando ser incluída num batch."""
    client_id: int
    client_sn: int
    payload_hash: str
    is_cross_group: bool = False
    gsn: int = 0
    touched_groups: list[int] = field(default_factory=list)


@dataclass
class BatchResult:
    """Resultado de um CutBatch."""
    requests: list[PendingRequest]
    digest: str
    bucket_id: int
    cut_reason: str  # "size", "timeout", "cross_op_immediate"

    @property
    def size(self) -> int:
        return len(self.requests)


class BatchCutter:
    """Simula o CutBatch do BucketGroup.

    Lógica real (bucketgroup.go):
      1. Se cross-op chega → corta imediatamente (batch de 1)
      2. Senão, espera até:
         a) totalRequests >= batchSize → corta
         b) timeout expira → corta com o que tem
      3. Requests são removidas do bucket ao cortar
    """

    def __init__(self, bucket_id: int, batch_size: int = 4, batch_timeout_ticks: int = 10):
        self.bucket_id = bucket_id
        self.batch_size = batch_size
        self.batch_timeout_ticks = batch_timeout_ticks

        self._pending: list[PendingRequest] = []
        self._ticks_waiting: int = 0
        self._cutting: bool = False

    @property
    def pending_count(self) -> int:
        return len(self._pending)

    @property
    def pending_labels(self) -> list[str]:
        return [f"sn={r.client_sn} h={r.payload_hash[:8]}" for r in self._pending]

    def add_request(self, req: PendingRequest) -> BatchResult | None:
        """Adiciona request ao bucket. Retorna batch se cross-op (corte imediato)."""
        self._pending.append(req)

        # Cross-op: corte imediato (batch de 1, sem acumulação)
        if req.is_cross_group:
            return self._cut([req], "cross_op_immediate")

        # Atingiu batch_size: corta
        if len(self._pending) >= self.batch_size:
            to_cut = self._pending[:self.batch_size]
            return self._cut(to_cut, "size")

        return None

    def tick(self) -> BatchResult | None:
        """Avança um tick de timeout. Retorna batch se timeout expirou."""
        if not self._pending:
            self._ticks_waiting = 0
            return None

        self._ticks_waiting += 1
        if self._ticks_waiting >= self.batch_timeout_ticks:
            to_cut = list(self._pending)
            return self._cut(to_cut, "timeout")

        return None

    def _cut(self, requests: list[PendingRequest], reason: str) -> BatchResult:
        """Corta um batch com as requests especificadas."""
        for r in requests:
            if r in self._pending:
                self._pending.remove(r)
        self._ticks_waiting = 0

        # Calcula digest do batch
        content = ":".join(r.payload_hash for r in requests)
        digest = hashlib.sha256(content.encode()).hexdigest()[:12]

        return BatchResult(
            requests=requests,
            digest=digest,
            bucket_id=self.bucket_id,
            cut_reason=reason,
        )

    def force_cut(self) -> BatchResult | None:
        """Força corte com tudo que tem (usado em fim de epoch)."""
        if not self._pending:
            return None
        return self._cut(list(self._pending), "forced")
