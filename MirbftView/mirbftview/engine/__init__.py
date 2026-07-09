"""Engine — Classe Simulation que a UI consome.

Mantém a mesma API pública que o antigo qt/simulation.py para
compatibilidade com canvas, panels, app.
"""

from mirbftview.protocol.types import (
    Phase, MsgType, Node, Client, Group, Message, RequestInfo, EventLog, SegmentInfo,
)
from mirbftview.engine.state import SimState
from mirbftview.engine.tick import tick as _tick
from mirbftview.engine.phases import phase_client_send, phase_view_change


class Simulation:
    """Fachada pública — mesma interface que o antigo qt/simulation.py."""

    def __init__(self):
        self._st = SimState()
        self._st.rebuild_managers()

    # ─── Propriedades delegadas (UI acessa diretamente) ───────────────────

    @property
    def nodes(self): return self._st.nodes
    @nodes.setter
    def nodes(self, v): self._st.nodes = v

    @property
    def clients(self): return self._st.clients
    @clients.setter
    def clients(self, v): self._st.clients = v

    @property
    def groups(self): return self._st.groups
    @groups.setter
    def groups(self, v): self._st.groups = v

    @property
    def messages(self): return self._st.messages
    @messages.setter
    def messages(self, v): self._st.messages = v

    @property
    def current_request(self): return self._st.current_request

    @property
    def phase(self): return self._st.phase

    @property
    def event_log(self): return self._st.event_log

    @property
    def info_text(self): return self._st.info_text
    @info_text.setter
    def info_text(self, v): self._st.info_text = v

    @property
    def num_buckets(self): return self._st.num_buckets
    @num_buckets.setter
    def num_buckets(self, v): self._st.num_buckets = v

    @property
    def bucket_contents(self): return self._st.bucket_contents
    @bucket_contents.setter
    def bucket_contents(self, v): self._st.bucket_contents = v

    @property
    def paused(self): return self._st.paused

    # Counters expostos para painéis
    @property
    def _sn(self): return self._st.current_request.sn if self._st.current_request else 0
    @property
    def _gsn(self): return self._st.gsn
    @property
    def _committed(self): return self._st.committed
    @property
    def _epoch(self): return self._st.epoch_mgr.epoch
    @property
    def _last_checkpoint(self): return self._st.last_checkpoint_sn
    @property
    def _checkpoint_interval(self): return self._st.checkpoint_interval
    @_checkpoint_interval.setter
    def _checkpoint_interval(self, v): self._st.checkpoint_interval = v

    # Epoch manager exposto para painéis
    @property
    def epoch_mgr(self): return self._st.epoch_mgr

    # Delivery (ADeliver) exposto para painéis
    @property
    def delivery(self): return self._st.delivery

    # GSN counter e META stream expostos para painéis
    @property
    def gsn(self): return self._st.gsn
    @property
    def meta_stream(self): return self._st.meta_stream

    # Commit history for visual chain
    @property
    def commit_history(self): return self._st.commit_history

    # Pipeline: requests ativas
    @property
    def active_requests(self): return self._st.active_requests

    # Steady-state flag
    @property
    def prepared(self): return self._st.prepared

    # Batch fill (visual)
    @property
    def batch_fill(self): return self._st.batch_fill
    @property
    def batch_visual_size(self): return self._st.batch_visual_size

    # Visual events (cross-panel flash effects)
    @property
    def visual_events(self): return self._st.visual_events

    # Bucket thought bubbles
    @property
    def bucket_bubbles(self): return self._st.bucket_bubbles

    # ─── Controles ────────────────────────────────────────────────────────

    def start(self):
        self._st.paused = False
        self._st.step_mode = False
        if self._st.phase in (Phase.IDLE, Phase.DONE):
            from mirbftview.engine.tick import _spawn_batch
            _spawn_batch(self._st)

    def pause(self):
        self._st.paused = True

    def resume(self):
        self._st.paused = False

    def toggle_pause(self):
        if self._st.paused:
            self.resume()
        else:
            self.pause()

    def set_step_mode(self, enabled: bool):
        self._st.step_mode = enabled
        self._st.paused = enabled

    def step_next(self):
        self._st.advance_flag = True

    def set_speed(self, speed: float):
        self._st.speed = max(0.05, min(2.0, speed))

    def reset(self):
        self._st.reset()

    def tick(self):
        _tick(self._st)

    # ─── Ações especiais ──────────────────────────────────────────────────

    def kill_node(self, node_id: int):
        """Simula falha de um nó (para demonstrar view change)."""
        for n in self._st.nodes:
            if n.id == node_id:
                n.is_alive = False
        self._st.view_change_mgr.kill_node(node_id)

    def revive_node(self, node_id: int):
        for n in self._st.nodes:
            if n.id == node_id:
                n.is_alive = True

    def trigger_view_change(self, segment_id: int = 0):
        """Força um view change para demonstração."""
        seg = None
        for s in self._st.epoch_mgr.current_segments:
            if s.seg_id == segment_id:
                seg = s
                break
        if not seg:
            seg = self._st.epoch_mgr.current_segments[0] if self._st.epoch_mgr.current_segments else None
        if seg:
            old_leader = seg.leader
            all_ids = [n.id for n in self._st.nodes]
            old_idx = all_ids.index(old_leader) if old_leader in all_ids else 0
            new_leader = all_ids[(old_idx + 1) % len(all_ids)]
            new_ballot = seg.seg_id + len(all_ids)
            phase_view_change(self._st, old_leader, new_leader, new_ballot)

    # ─── Configuração (usado pelo config_panel) ──────────────────────────

    def apply_config(self, **kwargs):
        """Aplica configuração e reconstrói managers."""
        if 'num_buckets' in kwargs:
            self._st.num_buckets = kwargs['num_buckets']
        if 'segment_length' in kwargs:
            self._st.segment_length = kwargs['segment_length']
        if 'batch_size' in kwargs:
            self._st.batch_size = kwargs['batch_size']
        if 'batch_timeout_ticks' in kwargs:
            self._st.batch_timeout_ticks = kwargs['batch_timeout_ticks']
        if 'checkpoint_interval' in kwargs:
            self._st.checkpoint_interval = kwargs['checkpoint_interval']
        if 'cross_op_pct' in kwargs:
            self._st.cross_op_pct = kwargs['cross_op_pct']
        if 'view_change_timeout' in kwargs:
            self._st.view_change_timeout = kwargs['view_change_timeout']
        self._st.bucket_contents = [[] for _ in range(self._st.num_buckets)]
        self._st.rebuild_managers()

    # Atributo legado para config_panel
    @property
    def _cross_op_pct(self): return self._st.cross_op_pct
    @_cross_op_pct.setter
    def _cross_op_pct(self, v): self._st.cross_op_pct = v

    # ── Cenários (toggles) ─────────────────────────────────────────
    def get_scenario(self, key: str) -> bool:
        return self._st.scenarios.get(key, False)

    def set_scenario(self, key: str, enabled: bool):
        self._st.scenarios[key] = enabled

    # step_mode e paused expostos para ControlBar
    @property
    def _paused(self): return self._st.paused
    @_paused.setter
    def _paused(self, v): self._st.paused = v

    @property
    def _step_mode(self): return self._st.step_mode
    @_step_mode.setter
    def _step_mode(self, v): self._st.step_mode = v
