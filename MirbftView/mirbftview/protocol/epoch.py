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
  - O líder de uma posição do log (SN) dentro de um grupo é escolhido de
    forma determinística entre os membros FIXOS daquele grupo (SetMembers em
    multipaxosinstance.go): `members[sn % n]` sob leaderPolicy=Simple (padrão)
    e `members[0]` para todo SN sob leaderPolicy=Single.
  - Os SNs de um grupo NÃO são consecutivos: o grupo `gid` começa em
    `firstSN + gid` (bootstrap: firstSN=0) e avança de `numGroups` em
    `numGroups` (multipaxosorderer.go, runSegment: `currentSN += numGroups`),
    onde numGroups = nº de grupos DEFINIDOS, contando o grupo 0. Os SNs dos
    grupos ficam assim intercalados no log global.
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
        leader_policy: str = "Simple",
    ):
        # group_id -> membros fixos daquele grupo (de groups.yml)
        self.groups = {gid: list(members) for gid, members in groups.items()}
        self.num_buckets = num_buckets
        self.segment_length = segment_length
        self.leader_policy = leader_policy
        # snLength = SegmentLength * numNodes (multipaxosmulticastorderer.go:94)
        self.sn_length = segment_length * num_nodes
        # numGroups do runSegment real: grupos definidos INCLUINDO o grupo 0
        # (o Sequenciador), que não está em `groups` (só tem grupos de dados).
        self.sn_stride = len(self.groups) + 1
        # Próxima posição do log (SN) a ser proposta em cada grupo:
        # firstSN(=0) + gid, depois de sn_stride em sn_stride.
        self.next_sn: dict[int, int] = {gid: gid for gid in self.groups}

    def leader_for_group(self, group_id: int, sn: int | None = None) -> int:
        """Líder da posição `sn` do grupo (ou da próxima, se `sn` omitido).

        Fórmula real (SetMembers): members[sn % n] sob Simple, members[0]
        sob Single.
        """
        members = self.groups.get(group_id) or [0]
        if self.leader_policy == "Single":
            return members[0]
        if sn is None:
            sn = self.next_sn.get(group_id, group_id)
        return members[sn % len(members)]

    def next_sn_for(self, group_id: int) -> int:
        """Próximo SN ainda não consumido no log deste grupo."""
        return self.next_sn.get(group_id, group_id)

    def advance(self, group_id: int):
        """Consome a posição atual do log deste grupo (um batch foi cortado)."""
        self.next_sn[group_id] = self.next_sn_for(group_id) + self.sn_stride

    def buckets_of_group(self, group_id: int) -> list[int]:
        """Buckets que pertencem a um grupo: g, g+numGroups, g+2*numGroups, ...
        (initGroupBuckets em multipaxosinstance.go). numGroups conta o grupo 0,
        cujos buckets (0, numGroups, ...) ficam com as mensagens de sistema."""
        return list(range(group_id, self.num_buckets, self.sn_stride))

    def bucket_owner(self, bucket_id: int) -> int:
        """Grupo dono de um bucket: bucket % numGroups."""
        return bucket_id % self.sn_stride

    def get_request_bucket(self, client_id: int, client_sn: int, group_id: int | None = None) -> int:
        """GetBucketNr (request.go) no modo MultiPaxos multicast.

        O bucket pertence ao GRUPO da requisição: só existem os buckets
        g, g+numGroups, ...; dentro deles a escolha é
            b = g + numGroups * ((clientId + clientSn) % nBucketsDoGrupo)
        Sem grupo (legado), cai na fórmula simples do PBFT: (clientId+clientSn) % N.
        """
        n = self.num_buckets
        if group_id is None:
            return (client_id + client_sn) % n
        ng = self.sn_stride
        g = max(group_id, 0)
        if g >= ng:
            g %= ng
        max_offset = max(len(range(g, n, ng)), 1)
        b = g + ng * ((client_id + client_sn) % max_offset)
        return b if b < n else g
