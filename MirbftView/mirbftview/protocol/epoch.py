"""Epoch — Gerenciamento de epochs, leader policy e bucket assignment.

Baseado em mirmanager.go (handleLogEntries, assignBuckets) e leaderpolicies.go.

Conceitos-chave:
  - O protocolo avança por epochs.
  - No fim de cada epoch: checkpoint → leader policy atualiza → redistribuição de buckets.
  - Bucket Assignment real (mirmanager.go assignBuckets):
      1. Distribui buckets round-robin entre TODOS os nós com offset por epoch:
         for idx, node in enumerate(allNodes):
             buckets[node] = [b for b where (idx + epoch) % numNodes == b % numNodes]
      2. Coleta buckets de não-líderes (extra_buckets).
      3. Redistribui extras entre líderes: leader = sorted_leaders[(b + epoch) % len(leaders)]
"""

from .types import SegmentInfo
from .segment import create_segments


class LeaderPolicy:
    """Política de seleção de líderes — Simple por padrão (todos são líderes)."""

    def __init__(self, policy_name: str = "Simple"):
        self.policy_name = policy_name
        self._banned: dict[int, int] = {}  # node_id → banned_until_epoch

    def get_leaders(self, epoch: int, all_nodes: list[int]) -> list[int]:
        if self.policy_name == "Single":
            return [all_nodes[epoch % len(all_nodes)]]
        elif self.policy_name == "Blacklist":
            return [n for n in all_nodes if n not in self._banned or self._banned[n] <= epoch]
        # Simple: todos são líderes
        return list(all_nodes)

    def suspect(self, epoch: int, node_id: int):
        """Marca nó como suspeito — usado por Blacklist/Backoff."""
        if self.policy_name == "Blacklist":
            self._banned[node_id] = epoch + 2
        elif self.policy_name == "Backoff":
            ban = self._banned.get(node_id, 1) * 2
            self._banned[node_id] = epoch + ban


class EpochManager:
    """Gerencia transições de epoch, criação de segmentos e bucket assignment.

    Reproduz o ciclo de vida do MirManager:
      epoch 0 → issueSegments → handleLogEntries → checkpoint → epoch 1 → ...
    """

    def __init__(
        self,
        all_nodes: list[int],
        num_buckets: int,
        segment_length: int = 4,
        batch_size: int = 4,
        leader_policy: str = "Simple",
    ):
        self.all_nodes = list(all_nodes)
        self.num_buckets = num_buckets
        self.segment_length = segment_length
        self.batch_size = batch_size
        self.leader_policy = LeaderPolicy(leader_policy)

        self.epoch: int = 0
        self._next_seg_id: int = 0
        self._sn_offset: int = 0

        # Estado atual
        self.current_leaders: list[int] = []
        self.current_segments: list[SegmentInfo] = []
        self.bucket_assignment: dict[int, list[int]] = {}  # leader → [bucket_ids]
        self.bucket_owner: dict[int, int] = {}  # bucket_id → leader que o possui

        # Inicializa epoch 0
        self._issue_epoch()

    def _issue_epoch(self):
        """Emite segmentos para a epoch atual."""
        self.current_leaders = self.leader_policy.get_leaders(self.epoch, self.all_nodes)
        if not self.current_leaders:
            self.current_leaders = list(self.all_nodes)

        self.bucket_assignment = self._assign_buckets(self.current_leaders)

        # Mapa reverso: bucket → líder
        self.bucket_owner = {}
        for leader, buckets in self.bucket_assignment.items():
            for b in buckets:
                self.bucket_owner[b] = leader

        self.current_segments = create_segments(
            leaders=self.current_leaders,
            all_nodes=self.all_nodes,
            segment_length=self.segment_length,
            sn_offset=self._sn_offset,
            bucket_assignment=self.bucket_assignment,
            batch_size=self.batch_size,
            next_seg_id=self._next_seg_id,
        )
        self._next_seg_id += len(self.current_segments)

    def advance_epoch(self) -> dict:
        """Avança para a próxima epoch. Retorna info da transição.

        Ciclo real (mirmanager.go handleLogEntries):
          1. Último SN da epoch é committed
          2. Checkpoint protocol triggered
          3. Watermarks avançam, buckets podados
          4. epoch++
          5. Novos líderes calculados (leader policy)
          6. Novos segmentos emitidos com novos buckets
        """
        old_epoch = self.epoch
        old_leaders = list(self.current_leaders)
        old_assignment = dict(self.bucket_assignment)

        # Avança
        self._sn_offset += self.segment_length * len(self.current_leaders)
        self.epoch += 1
        self._issue_epoch()

        return {
            "old_epoch": old_epoch,
            "new_epoch": self.epoch,
            "old_leaders": old_leaders,
            "new_leaders": self.current_leaders,
            "old_bucket_assignment": old_assignment,
            "new_bucket_assignment": self.bucket_assignment,
            "sn_offset": self._sn_offset,
        }

    def _assign_buckets(self, leaders: list[int]) -> dict[int, list[int]]:
        """Atribui buckets aos líderes — reproduz mirmanager.go assignBuckets().

        Algoritmo real:
          1. Distribui TODOS os buckets entre TODOS os nós (round-robin com offset por epoch):
             for idx, node in enumerate(allNodes):
                 node gets bucket b where (idx + epoch) % numNodes == b % numNodes
          2. Coleta buckets de nós que NÃO são líderes (extra_buckets).
          3. Redistribui extras entre líderes:
             leader = sorted_leaders[(b + epoch) % len(leaders)]
        """
        num_nodes = len(self.all_nodes)
        is_leader = set(leaders)
        sorted_leaders = sorted(leaders)

        # Passo 1: distribuição inicial round-robin com offset por epoch
        init_buckets: dict[int, list[int]] = {n: [] for n in self.all_nodes}
        for b in range(self.num_buckets):
            # Qual nó recebe este bucket? O nó cujo (idx + epoch) % numNodes == b % numNodes
            owner_idx = (b - self.epoch) % num_nodes
            # Garante índice positivo
            if owner_idx < 0:
                owner_idx += num_nodes
            owner = self.all_nodes[owner_idx % num_nodes]
            init_buckets[owner].append(b)

        # Passo 2: coleta buckets de não-líderes
        extra_buckets = []
        for node in self.all_nodes:
            if node not in is_leader:
                extra_buckets.extend(init_buckets[node])

        # Passo 3: inicializa com buckets próprios dos líderes
        final_buckets: dict[int, list[int]] = {}
        for leader in sorted_leaders:
            final_buckets[leader] = list(init_buckets[leader])

        # Passo 4: redistribui extras entre líderes
        for b in extra_buckets:
            target = sorted_leaders[(b + self.epoch) % len(sorted_leaders)]
            final_buckets[target].append(b)

        return final_buckets

    def get_segment_for_leader(self, leader_id: int) -> SegmentInfo | None:
        """Retorna o segmento do líder especificado."""
        for seg in self.current_segments:
            if seg.leader == leader_id:
                return seg
        return None

    def get_leader_for_bucket(self, bucket_id: int) -> int:
        """Retorna o líder responsável por um bucket."""
        return self.bucket_owner.get(bucket_id, self.current_leaders[0])

    def get_request_bucket(self, client_id: int, client_sn: int) -> int:
        """Atribui request ao bucket — esta é a fórmula request→bucket.

        Fórmula: (clientID + clientSN) % numBuckets
        (Isso NÃO muda com epoch — é fixo por request.)
        """
        return (client_id + client_sn) % self.num_buckets

    @property
    def epoch_length(self) -> int:
        """Tamanho total da epoch em SNs."""
        return self.segment_length * len(self.current_leaders)

    @property
    def last_epoch_sn(self) -> int:
        """Último SN da epoch atual."""
        return self._sn_offset + self.epoch_length - 1
