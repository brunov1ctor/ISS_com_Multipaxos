"""ViewChange — Ballot/View Change quando o líder falha.

Baseado no conceito de view change do MultiPaxos/PBFT.

Conceito-chave:
  - Quando o líder falha (timeout sem progresso), followers suspeitam.
  - Um novo líder propõe um ballot maior (view change).
  - O novo líder coleta estado dos followers e re-propõe valores pendentes.
  - No ISS/Mir: o Manager é notificado do suspect e atualiza a leader policy
    para a próxima epoch (não há view change intra-epoch no sentido PBFT clássico,
    mas o segmento pode abortar e o suspect é registrado).
"""

from dataclasses import dataclass, field


@dataclass
class ViewChangeState:
    """Estado de um view change em andamento."""
    segment_id: int
    old_leader: int
    new_leader: int
    old_ballot: int
    new_ballot: int
    suspects: set[int] = field(default_factory=set)  # nós que suspeitaram
    view_change_sent: set[int] = field(default_factory=set)
    new_view_acks: set[int] = field(default_factory=set)
    completed: bool = False

    @property
    def quorum(self) -> int:
        # Precisa de f+1 suspects para trigger, 2f+1 para completar
        return (len(self.suspects) + len(self.new_view_acks)) // 2 + 1


class ViewChangeManager:
    """Gerencia view changes por segmento."""

    def __init__(self, all_nodes: list[int], view_change_timeout_ticks: int = 30):
        self.all_nodes = list(all_nodes)
        self.timeout_ticks = view_change_timeout_ticks
        self._active: dict[int, ViewChangeState] = {}  # segment_id → state
        self._ticks_without_progress: dict[int, int] = {}  # segment_id → ticks

    def tick_segment(self, segment_id: int, leader: int, has_progress: bool) -> ViewChangeState | None:
        """Avança tick para um segmento. Retorna ViewChangeState se timeout."""
        if segment_id in self._active:
            return None  # Já em view change

        if has_progress:
            self._ticks_without_progress[segment_id] = 0
            return None

        ticks = self._ticks_without_progress.get(segment_id, 0) + 1
        self._ticks_without_progress[segment_id] = ticks

        if ticks >= self.timeout_ticks:
            return self._trigger_view_change(segment_id, leader)

        return None

    def _trigger_view_change(self, segment_id: int, old_leader: int) -> ViewChangeState:
        """Inicia view change — elege novo líder com ballot maior."""
        # Novo líder: próximo nó na lista (round-robin)
        old_idx = self.all_nodes.index(old_leader) if old_leader in self.all_nodes else 0
        new_idx = (old_idx + 1) % len(self.all_nodes)
        new_leader = self.all_nodes[new_idx]

        state = ViewChangeState(
            segment_id=segment_id,
            old_leader=old_leader,
            new_leader=new_leader,
            old_ballot=segment_id,  # simplificação
            new_ballot=segment_id + len(self.all_nodes),
            suspects={n for n in self.all_nodes if n != old_leader},
        )
        self._active[segment_id] = state
        return state

    def complete_view_change(self, segment_id: int):
        """Marca view change como completo."""
        if segment_id in self._active:
            self._active[segment_id].completed = True
            del self._ticks_without_progress[segment_id]

    def get_active(self, segment_id: int) -> ViewChangeState | None:
        return self._active.get(segment_id)

    def is_in_view_change(self, segment_id: int) -> bool:
        return segment_id in self._active and not self._active[segment_id].completed

    def kill_node(self, node_id: int):
        """Marca nó como morto (para simulação de falha)."""
        # Não remove da lista, apenas permite que timeouts disparem
        pass
