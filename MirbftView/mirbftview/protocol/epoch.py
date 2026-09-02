"""GroupLog — estado do log contínuo de cada grupo de dados.

Baseado no MultiPaxosMulticastOrderer real (orderer/multipaxosmulticastorderer.go
e orderer/multipaxosinstance.go), não no ISS clássico (mirmanager.go):

  - Os grupos de dados são ESTÁTICOS, definidos externamente (groups.yml).
    Não há "leader policy" trocando quem são os líderes, nem redistribuição
    de buckets entre épocas: buckets pertencem permanentemente ao grupo para
    o qual a chave da requisição foi mapeada.
  - Cada grupo roda um único segmento contínuo cobrindo `SegmentLength *
    numNodes` SNs (multipaxosmulticastorderer.go: `Start()`, `ContiguousSegment`),
    ao contrário do ISS clássico, que divide o log em vários segmentos
    intercalados (um por líder) e os reemite a cada época.
  - O líder de uma posição do log (SN) dentro de um grupo é escolhido por
    rodízio determinístico entre os membros FIXOS daquele grupo:
    `i.leader = i.members[i.sn % n]` (multipaxosinstance.go:606). Isso não
    depende de época nem de "leader policy" — é sempre assim, para qualquer
    SN, em qualquer grupo.
"""


class EpochManager:
    """Mantém, por grupo de dados, o próximo SN a ser proposto e seu líder.

    Nome mantido por compatibilidade com o resto do simulador (state.py,
    phases.py, tick.py já chamam `st.epoch_mgr.*`), mas não há mais noção
    de "época" que troque líderes ou redistribua buckets.
    """

    def __init__(
        self,
        groups: dict[int, list[int]],
        num_buckets: int,
        segment_length: int,
        num_nodes: int,
    ):
        # group_id -> membros fixos daquele grupo (de groups.yml)
        self.groups = {gid: list(members) for gid, members in groups.items()}
        self.num_buckets = num_buckets
        self.segment_length = segment_length
        # snLength = SegmentLength * numNodes (multipaxosmulticastorderer.go:94)
        self.sn_length = segment_length * num_nodes
        # Próxima posição do log (SN) a ser proposta em cada grupo.
        self.next_sn: dict[int, int] = {gid: 0 for gid in self.groups}

    def leader_for_group(self, group_id: int, sn: int | None = None) -> int:
        """Líder da posição `sn` do grupo (ou da próxima, se `sn` omitido).

        Fórmula real: members[sn % n] (multipaxosinstance.go:606).
        """
        members = self.groups.get(group_id) or [0]
        if sn is None:
            sn = self.next_sn.get(group_id, 0)
        return members[sn % len(members)]

    def next_sn_for(self, group_id: int) -> int:
        """Próximo SN ainda não consumido no log deste grupo."""
        return self.next_sn.get(group_id, 0)

    def advance(self, group_id: int):
        """Consome a posição atual do log deste grupo (um batch foi cortado)."""
        self.next_sn[group_id] = self.next_sn.get(group_id, 0) + 1

    def get_request_bucket(self, client_id: int, client_sn: int) -> int:
        """(clientID + clientSN) % numBuckets — GetBucketNr em request.go.

        Não muda com o grupo nem com época: é a mesma fórmula em qualquer
        grupo, aplicada apenas dentro do conjunto de buckets locais dele.
        """
        return (client_id + client_sn) % self.num_buckets
