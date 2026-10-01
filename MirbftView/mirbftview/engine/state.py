"""Estado global da simulação — topologia, counters, controle."""

from mirbftview.protocol.types import (
    Node, Client, Group, Message, RequestInfo, EventLog, Phase,
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
        #   viewChangeTimeouts=60000ms, leaderPolicies="Single" (generate-local-config.sh)
        #   orderers="MultiPaxosMulticast", nodeToLeaderRatios=1
        num_peers = 5
        self.nodes = [Node(i, f"Node {i}", [0]) for i in range(num_peers)]
        self.clients = [Client(i, f"Cliente {i}") for i in range(4)]
        self.groups = [
            Group(0, "Sequenciador (G0)", list(range(num_peers))),
            Group(1, "Dados G1", [0, 1, 2]),
            Group(2, "Dados G2", [2, 3, 4]),
            Group(3, "Dados G3", [0, 4, 1]),
            Group(4, "Dados G4", [1, 3, 4]),
        ]
        # Grupos de cada nó DERIVADOS de groups.yml (antes a lista era escrita à mão e
        # divergia: ex. o Node 1 aparecia sem o G3, embora G3 = [0, 4, 1]).
        for g in self.groups:
            for nid in g.members:
                if g.id != 0 and g.id not in self.nodes[nid].groups:
                    self.nodes[nid].groups.append(g.id)
        # bucketsPerLeader=16, numBuckets real = bucketsPerLeader * numPeers = 80;
        # aqui usamos 16 (minBuckets) só para caber na tela.
        self.num_buckets = 16
        self.segment_length = 16  # segmentLengths=16
        self.batch_size = 4096  # batchsizes=4096
        self.batch_timeout_ticks = 10 * 62  # timeout do CutBatch: 10 s simulados (62 ticks = 1 s)
        # Intervalo de checkpoint (a cada N commits), independente de época/líder:
        # o MultiPaxosMulticastOrderer não redistribui buckets nem troca líder
        # em checkpoints, só permite truncar o log.
        self.checkpoint_interval = 16 * num_peers
        self.cross_op_pct = 0.3  # workload param (não no generate-config)
        # crossOpGroupWeightsList do generate-local-config.sh ("2:100" = todo cross-op
        # toca exatamente 2 grupos; ex.: "2:70,3:25,4:5" mistura 2, 3 e 4 grupos).
        self.cross_op_group_weights = "2:100"
        # leaderPolicies do generate-local-config.sh: sob "Single", SetMembers fixa o
        # líder em members[0] para todo SN do grupo; sob "Simple", members[sn % n].
        self.leader_policy = "Single"
        self.view_change_timeout = 3750  # 60000ms / 16ms per tick ≈ 3750

    def _init_counters(self):
        self.client_sn = 0
        self.gsn = 0
        self.committed = 0
        self.last_checkpoint_sn = -1
        self.checkpoints_done = 0
        # Checkpoint por grupo (ver recordGroupCommit em multipaxosorderer.go): cada grupo de
        # dados conta seus próprios commits e estabiliza seu checkpoint sozinho, sem round-trip
        # de rede nem depender do log intercalado inteiro -- diferente do que o código antigo
        # (e esta simulação, antes do fix) assumia, de checkpoint global confirmado por todo
        # mundo. group_commit_count/group_last_checkpoint_sn são por grupo; checkpoints_done
        # continua global só para o contador agregado exibido na tela (soma de todos os grupos).
        self.group_commit_count: dict[int, int] = {}
        self.group_last_checkpoint_sn: dict[int, int] = {}
        # META stream: historico de publicacoes GSN -> grupos
        self.meta_stream: list[dict] = []  # [{gsn, groups, published_by}]

    def _init_control(self):
        # Ticks da simulação que de fato avançaram (não conta enquanto pausada):
        # os balões de pensamento envelhecem por aqui, e não pelo relógio da tela.
        self.tick_count: int = 0
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
        # Buckets POR NÓ (cada nó guarda o seu, com todos os num_buckets; só usa os dos
        # grupos a que pertence): node_buckets[nó][bucket] -> [labels dos pedidos].
        self.node_buckets: dict[int, list[list[str]]] = {}
        # (nó, bucket, label) -> tick em que a cópia chegou ao bucket daquele nó
        self.bucket_born: dict = {}
        # Animação dos pedidos dentro dos buckets (tempo em ticks da simulação):
        #   bucket_meta[label]  -> {"color"}: cor do pedido
        #   bucket_leaving      -> pedidos que acabaram de sair do bucket de um nó (commit)
        self.bucket_meta: dict[str, dict] = {}
        self.bucket_leaving: list[dict] = []
        # (nó, bucket) -> tick em que chegou a última cross-op àquele bucket (pulso na UI)
        self.cross_pulse: dict = {}
        # Commit history for visual chain
        self.commit_history: list[dict] = []  # [{sn, leader, epoch, hash, is_cross, gsn}]
        # Scenarios toggles
        self.scenarios: dict[str, bool] = {}
        # Pipeline: itens ativos = INSTÂNCIAS de Paxos (uma por grupo, sempre presentes depois
        # de iniciar) + PEDIDOS ainda a caminho dos buckets.
        self.active_requests: list[RequestInfo] = []
        # Instância corrente de cada grupo de dados: group_id -> RequestInfo(kind="instance")
        self.instances: dict[int, RequestInfo] = {}
        # Rodadas (todas as mensagens chegaram e o pipeline avançou uma fase) e chegada de
        # pedidos no modo contínuo: `arrival_burst` pedidos a cada `arrival_every` rodadas,
        # até `max_pending` sem resposta. O batch aparece porque pedidos chegam enquanto a
        # instância do grupo ainda está em consenso.
        self.round_count: int = 0
        self.arrival_every: int = 2
        self.arrival_burst: int = 2
        self.max_pending: int = 14
        # pedidos sem resposta ainda: (client_id, client_sn) -> {"proxy", "color", "name"}
        self.pending_info: dict = {}
        self.spawn_counter: int = 0
        # Mensagens de instâncias que nasceram durante a rodada (PREPARE do sucessor), a serem
        # somadas às da rodada sem atropelar as mensagens do item que está sendo avançado.
        self.extra_messages: list = []
        # Histórico de SNs por grupo (janelinha "SNs por grupo"): group_id -> lista de
        # {"sn", "leader", "born" (tick de criação), "inst" (a própria instância)}
        self.sn_history: dict = {}
        # Requests genuinamente bloqueadas pelo ADeliver (BufferCommit real):
        # group_id -> lista de RequestInfo aguardando o GSN anterior daquele
        # grupo ser entregue. Não fazem parte de active_requests enquanto
        # bloqueadas — o tick.py as libera quando outro commit do mesmo
        # grupo roda _try_deliver de novo e desbloqueia a fila.
        self.blocked_requests: dict[int, list[RequestInfo]] = {}
        # Cenário 'ADeliver bloqueado': META que chega atrasado a um grupo
        #   {gsn, group, nodes, state: 'held'|'sent', wait, age}
        self.meta_late: list[dict] = []
        self.pipeline_size: int = 3
        # Steady-state: após primeiro PREPARE/PROMISE, pula direto para ACCEPT
        self.prepared: bool = False
        # Contador sequencial POR CLIENTE (seqNr), fiel ao cliente real
        # (createPayload usa isso para a chave K{seqNr:08d} e para decidir
        # TX/GET por seqNr%100<CrossOpRatio) — cada cliente tem o seu.
        self.client_seq: dict[int, int] = {}
        # Batch visual: quantas requests acumular antes de cortar
        self.batch_visual_size: int = 3
        # Batch fill level per bucket (for visual progress bar)
        self.batch_fill: dict[int, int] = {}
        # Batch timeout ticks per bucket (simula waitForRequests com timer)
        self.batch_timeout_counter: dict[int, int] = {}
        # Batch timeout limit in ticks (simula minBatchTimeout)
        self.batch_timeout_limit: int = 8  # ~128ms at 16ms/tick
        # Buckets waiting to cut (accumulated enough or timed out)
        self.batch_ready: dict[int, bool] = {}
        # Thought bubbles for buckets: {bucket_id: {"text": str, "color": str, "ttl": int}}
        self.bucket_bubbles: dict[int, dict] = {}
        # Visual flash events: paineis consomem para gerar efeitos
        # [{"type": "bucket_in"|"batch_cut"|"commit", "bucket": int, "sn": int, "ttl": int}]
        self.visual_events: list[dict] = []

    def rebuild_managers(self):
        """(Re)cria os managers do protocolo com base na topologia atual."""
        all_node_ids = [n.id for n in self.nodes]
        # Só os grupos de dados (id != 0) têm log próprio; o grupo 0
        # (sequenciador) não roda MultiPaxos, é tratado à parte (phase_gsn_assign).
        data_groups = {g.id: g.members for g in self.groups if g.id != 0}
        self.epoch_mgr = EpochManager(
            groups=data_groups,
            num_buckets=self.num_buckets,
            segment_length=self.segment_length,
            num_nodes=len(all_node_ids),
            leader_policy=self.leader_policy,
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
        """Reset completo.

        Preserva os cenários (checkboxes) ativados pelo usuário — Reset
        reinicia o estado da simulação, não a configuração de depuração.
        """
        kept_scenarios = dict(self.scenarios)
        self._init_counters()
        self._init_control()
        self.scenarios = kept_scenarios
        self.node_buckets = {}
        self.commit_history = []
        self.meta_stream = []
        self.meta_late = []
        self.batch_fill = {}
        self.batch_timeout_counter = {}
        self.batch_ready = {}
        self.bucket_bubbles = {}
        self.visual_events = []
        self.rebuild_managers()

    def log_event(self, phase: Phase, title: str, detail: str, color_key: str = "text"):
        if getattr(self, '_suppress_log', False):
            return
        req_color = ""
        if self.current_request and self.current_request.color:
            req_color = self.current_request.color
        self.event_log.append(EventLog(phase, title, detail, color_key, req_color))
