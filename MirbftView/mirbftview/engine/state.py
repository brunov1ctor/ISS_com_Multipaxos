"""Estado global da simulação — topologia, counters, controle."""

from mirbftview.protocol.types import (
    Node, Client, Group, Message, RequestInfo, EventLog, Phase, SegmentInfo,
)
from mirbftview.protocol.epoch import EpochManager
from mirbftview.protocol.batch import BatchCutter
from mirbftview.protocol.delivery import AtomicDelivery, ProxyState
from mirbftview.protocol.viewchange import ViewChangeManager


class SimState:
    """Todo o estado mutável da simulação, sem lógica de transição."""

    def __init__(self):
        self._init_topology()
        self._init_counters()
        self._init_control()

    def _init_topology(self):
        # Defaults from generate-config.sh:
        #   systemSizes=5, clients1=4, bucketsPerLeader=16, minBuckets=16
        #   batchsizes=4096, minBatchTimeout=1000ms, segmentLengths=16
        #   viewChangeTimeouts=60000ms, leaderPolicies="Simple"
        #   orderers="MultiPaxosMulticast", nodeToLeaderRatios=1
        num_peers = 5
        self.nodes = [Node(i, f"Node {i}", [0]) for i in range(num_peers)]
        # From groups.yml: 4 data groups, each with 3 nodes
        # Node 0: [1, 3], Node 1: [1, 4], Node 2: [1, 2], Node 3: [2, 4], Node 4: [3, 4]
        self.nodes[0].groups.extend([1, 3])
        self.nodes[1].groups.extend([1, 4])
        self.nodes[2].groups.extend([1, 2])
        self.nodes[3].groups.extend([2, 4])
        self.nodes[4].groups.extend([3, 4])

        self.clients = [Client(i, f"Cliente {i}") for i in range(4)]
        self.groups = [
            Group(0, "Sequenciador (G0)", list(range(num_peers))),
            Group(1, "Dados G1", [0, 1, 2]),
            Group(2, "Dados G2", [2, 3, 4]),
            Group(3, "Dados G3", [0, 4, 1]),
            Group(4, "Dados G4", [1, 3, 4]),
        ]
        # bucketsPerLeader=16, numBuckets = max(bucketsPerLeader * numPeers, minBuckets)
        self.num_buckets = max(16 * num_peers, 16)  # = 80 (real), mas para visualização usamos 16
        self.num_buckets = 16  # Visualização: 16 buckets (minBuckets)
        self.segment_length = 16  # segmentLengths=16
        self.batch_size = 4096  # batchsizes=4096
        self.batch_timeout_ticks = 62  # minBatchTimeout=1000ms / 16ms per tick ≈ 62
        self.checkpoint_interval = 16 * num_peers  # epoch = segmentLength * numPeers = 80
        self.cross_op_pct = 0.3  # workload param (não no generate-config)
        self.view_change_timeout = 3750  # 60000ms / 16ms per tick ≈ 3750

    def _init_counters(self):
        self.client_sn = 0
        self.gsn = 0
        self.committed = 0
        self.last_checkpoint_sn = -1
        # META stream: historico de publicacoes GSN -> grupos
        self.meta_stream: list[dict] = []  # [{gsn, groups, published_by}]

    def _init_control(self):
        self.messages: list[Message] = []
        self.current_request: RequestInfo | None = None
        self.phase: Phase = Phase.IDLE
        self.event_log: list[EventLog] = []
        self.info_text: str = "Pressione ▶ Iniciar para começar a simulação."
        self.paused: bool = True
        self.speed: float = 0.3
        self.step_mode: bool = True
        self.advance_flag: bool = False
        # Bucket visual contents (for UI)
        self.bucket_contents: list[list[str]] = [[] for _ in range(self.num_buckets)]
        # Commit history for visual chain
        self.commit_history: list[dict] = []  # [{sn, leader, epoch, hash, is_cross, gsn}]
        # Scenarios toggles
        self.scenarios: dict[str, bool] = {}
        # Pipeline: multiplas requests em paralelo
        self.active_requests: list[RequestInfo] = []
        self.pipeline_size: int = 3
        # Batch visual: quantas requests acumular antes de cortar
        self.batch_visual_size: int = 3
        # Batch fill level per bucket (for visual progress bar)
        self.batch_fill: dict[int, int] = {}
        # Visual flash events: paineis consomem para gerar efeitos
        # [{"type": "bucket_in"|"batch_cut"|"commit", "bucket": int, "sn": int, "ttl": int}]
        self.visual_events: list[dict] = []

    def rebuild_managers(self):
        """(Re)cria os managers do protocolo com base na topologia atual."""
        all_node_ids = [n.id for n in self.nodes]
        self.epoch_mgr = EpochManager(
            all_nodes=all_node_ids,
            num_buckets=self.num_buckets,
            segment_length=self.segment_length,
            batch_size=self.batch_size,
            leader_policy="Simple",
        )
        # Um BatchCutter por bucket
        self.batch_cutters: dict[int, BatchCutter] = {
            b: BatchCutter(b, self.batch_size, self.batch_timeout_ticks)
            for b in range(self.num_buckets)
        }
        self.delivery = AtomicDelivery()
        self.proxy = ProxyState()
        self.view_change_mgr = ViewChangeManager(all_node_ids, self.view_change_timeout)

    def reset(self):
        """Reset completo."""
        self._init_counters()
        self._init_control()
        self.bucket_contents = [[] for _ in range(self.num_buckets)]
        self.commit_history = []
        self.meta_stream = []
        self.batch_fill = {}
        self.visual_events = []
        self.rebuild_managers()

    def log_event(self, phase: Phase, title: str, detail: str, color_key: str = "text"):
        self.event_log.append(EventLog(phase, title, detail, color_key))
        if len(self.event_log) > 50:
            self.event_log = self.event_log[-50:]
