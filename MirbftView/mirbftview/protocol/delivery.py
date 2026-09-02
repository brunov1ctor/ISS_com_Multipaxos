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

    Regra: para cada grupo, cross-ops são entregues na ordem em que o META
    (register_touch) as apresentou àquele grupo — não uma sequência de
    inteiros consecutivos, já que o GSN é global e compartilhado entre
    pares de grupos diferentes; um grupo só vê os GSNs que de fato o
    tocam, exatamente como groupGSNQueue no código real (sequencer.go).
    """

    def __init__(self):
        # Último GSN entregue por grupo
        self._last_delivered_gsn: dict[int, int] = {}  # group_id → last_gsn
        # Fila de entries por grupo, na ordem em que o META anunciou (pode
        # conter placeholders ainda não commitados — register_touch).
        self._pending: dict[int, list[DeliveryEntry]] = {}  # group_id → [entries]
        # Historico de entregas (para visualizacao)
        self.delivery_history: list[dict] = []  # [{gsn, groups, sn, status}]

    def register_touch(self, gsn: int, group_id: int):
        """Anuncia que este GSN toca este grupo — chamado assim que o META
        é conhecido (GSN_ASSIGN), ANTES do commit em si. Espelha
        RegisterMetadata/groupGSNQueue reais: a ordem de entrega de um grupo
        é fixada pela chegada do META, não pela hora em que aquele grupo
        específico termina seu próprio consenso."""
        pending = self._pending.setdefault(group_id, [])
        if any(e.gsn == gsn for e in pending):
            return
        pending.append(DeliveryEntry(sn=-1, gsn=gsn, group_id=group_id, batch_digest="", committed=False))
        pending.sort(key=lambda e: e.gsn)

    def register_commit(self, entry: DeliveryEntry) -> list[DeliveryEntry]:
        """Registra um commit e retorna entries que podem ser entregues agora.

        Single-group (gsn=0): entrega imediata.
        Cross-group (gsn>0): só entrega quando estiver na frente da fila
        deste grupo (ou seja, todo GSN anterior que também o toca já foi
        entregue) — substitui o placeholder de register_touch, se existir.
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

        # Cross-group: substitui o placeholder (register_touch) pela entrada
        # real do commit, ou adiciona se por algum motivo não existia ainda.
        gid = entry.group_id
        pending = self._pending.setdefault(gid, [])
        for i, e in enumerate(pending):
            if e.gsn == entry.gsn and not e.committed:
                pending[i] = entry
                break
        else:
            pending.append(entry)
            pending.sort(key=lambda e: e.gsn)

        delivered = self._try_deliver(gid)
        for d in delivered:
            self.delivery_history.append({
                "gsn": d.gsn, "sn": d.sn, "group": d.group_id,
                "groups": [d.group_id], "status": "delivered", "type": "cross",
            })
        if len(self.delivery_history) > 30:
            self.delivery_history = self.delivery_history[-30:]
        return delivered

    def _try_deliver(self, group_id: int) -> list[DeliveryEntry]:
        """Entrega, em ordem, todo prefixo já commitado da fila deste grupo.

        Como a fila está sempre ordenada por GSN (pela ordem de chegada do
        META, via register_touch), basta parar no primeiro ainda não
        commitado — sem assumir que os GSNs sejam inteiros consecutivos.
        """
        delivered = []
        last_gsn = self._last_delivered_gsn.get(group_id, 0)
        pending = self._pending.get(group_id, [])

        while pending and pending[0].committed:
            entry = pending.pop(0)
            entry.delivered = True
            last_gsn = entry.gsn
            delivered.append(entry)

        self._last_delivered_gsn[group_id] = last_gsn
        return delivered

    def get_blocked_entries(self, group_id: int) -> list[DeliveryEntry]:
        """Retorna entries já commitadas mas ainda atrás de um GSN anterior
        (não commitado ou ainda não anunciado) na fila deste grupo."""
        pending = self._pending.get(group_id, [])
        return [e for e in pending if e.committed]

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
