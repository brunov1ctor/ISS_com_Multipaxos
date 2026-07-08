"""Delivery — ADeliver (entrega atômica) e Proxy/COMMIT_NOTIFY.

Baseado no conceito de CSMR Output Processing do ISS.

Conceitos-chave:

  ADeliver (Atomic Delivery):
    - Cross-ops só são entregues quando o GSN anterior para aquele grupo
      já foi entregue.
    - Garante ordem global: se GSN=5 ainda não foi entregue no grupo G1,
      GSN=6 fica bloqueado mesmo que já tenha sido committed.
    - Single-group ops são entregues imediatamente (não têm GSN).

  Proxy/COMMIT_NOTIFY:
    - O nó que recebe a request do cliente é o "proxy".
    - O proxy NÃO responde ao cliente imediatamente.
    - Só responde após receber COMMIT_NOTIFY do grupo que processou.
    - Isso garante que o cliente só recebe confirmação após consenso.
"""

from dataclasses import dataclass, field


@dataclass
class DeliveryEntry:
    """Uma entrada aguardando entrega."""
    sn: int
    gsn: int  # 0 = single-group (entrega imediata)
    group_id: int
    batch_digest: str
    committed: bool = False
    delivered: bool = False


class AtomicDelivery:
    """Gerencia a entrega atômica baseada em GSN.

    Regra: para cada grupo, cross-ops são entregues em ordem de GSN.
    Se GSN=N não foi entregue, GSN=N+1 fica bloqueado.
    """

    def __init__(self):
        # Último GSN entregue por grupo
        self._last_delivered_gsn: dict[int, int] = {}  # group_id → last_gsn
        # Fila de entries aguardando entrega por grupo
        self._pending: dict[int, list[DeliveryEntry]] = {}  # group_id → [entries]
        # Historico de entregas (para visualizacao)
        self.delivery_history: list[dict] = []  # [{gsn, groups, sn, status}]

    def register_commit(self, entry: DeliveryEntry) -> list[DeliveryEntry]:
        """Registra um commit e retorna entries que podem ser entregues agora.

        Single-group (gsn=0): entrega imediata.
        Cross-group (gsn>0): só entrega se GSN anterior já foi entregue.
        """
        entry.committed = True

        # Single-group: entrega imediata
        if entry.gsn == 0:
            entry.delivered = True
            self.delivery_history.append({
                "gsn": 0, "sn": entry.sn, "group": entry.group_id,
                "groups": [entry.group_id], "status": "delivered", "type": "single",
            })
            if len(self.delivery_history) > 30:
                self.delivery_history = self.delivery_history[-30:]
            return [entry]

        # Cross-group: adiciona à fila e tenta entregar em ordem
        gid = entry.group_id
        if gid not in self._pending:
            self._pending[gid] = []
        self._pending[gid].append(entry)

        delivered = self._try_deliver(gid)
        # Registra no historico
        for d in delivered:
            self.delivery_history.append({
                "gsn": d.gsn, "sn": d.sn, "group": d.group_id,
                "groups": [d.group_id], "status": "delivered", "type": "cross",
            })
        if len(self.delivery_history) > 30:
            self.delivery_history = self.delivery_history[-30:]
        return delivered

    def _try_deliver(self, group_id: int) -> list[DeliveryEntry]:
        """Tenta entregar entries pendentes em ordem de GSN."""
        delivered = []
        last_gsn = self._last_delivered_gsn.get(group_id, 0)

        # Ordena por GSN
        pending = self._pending.get(group_id, [])
        pending.sort(key=lambda e: e.gsn)

        while pending:
            entry = pending[0]
            if not entry.committed:
                break  # Ainda não committed, não pode entregar
            if entry.gsn <= last_gsn + 1:
                # Pode entregar (GSN consecutivo ou igual)
                entry.delivered = True
                if entry.gsn > last_gsn:
                    last_gsn = entry.gsn
                delivered.append(pending.pop(0))
            else:
                # GSN gap: bloqueado até GSN anterior ser entregue
                break

        self._last_delivered_gsn[group_id] = last_gsn
        return delivered

    def get_blocked_entries(self, group_id: int) -> list[DeliveryEntry]:
        """Retorna entries bloqueadas aguardando GSN anterior."""
        last_gsn = self._last_delivered_gsn.get(group_id, 0)
        pending = self._pending.get(group_id, [])
        return [e for e in pending if e.committed and e.gsn > last_gsn + 1]

    def get_next_expected_gsn(self, group_id: int) -> int:
        """Retorna o próximo GSN esperado para entrega neste grupo."""
        return self._last_delivered_gsn.get(group_id, 0) + 1


@dataclass
class ProxyState:
    """Estado do proxy — rastreia requests aguardando COMMIT_NOTIFY.

    O proxy é o nó que recebeu a request do cliente.
    Ele só responde ao cliente após receber COMMIT_NOTIFY.
    """
    # request_key (client_id, client_sn) → proxy_node_id
    pending_responses: dict[tuple[int, int], int] = field(default_factory=dict)

    def register_request(self, client_id: int, client_sn: int, proxy_node: int):
        """Registra que o proxy está aguardando COMMIT_NOTIFY."""
        self.pending_responses[(client_id, client_sn)] = proxy_node

    def notify_commit(self, client_id: int, client_sn: int) -> int | None:
        """Notifica commit. Retorna proxy_node se havia pendência."""
        key = (client_id, client_sn)
        return self.pending_responses.pop(key, None)

    def has_pending(self, client_id: int, client_sn: int) -> bool:
        return (client_id, client_sn) in self.pending_responses
