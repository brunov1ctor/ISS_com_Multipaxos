"""Segment — Criação de segmentos com SNs intercalados (skipping).

Baseado em mirmanager.go createSegments() e skippingsegment.go.

Conceito-chave:
  - O Manager divide o log em segmentos paralelos (um por líder).
  - Cada segmento tem SNs intercalados com distance = nº de líderes.
  - Exemplo com 4 líderes, offset=0, length=4:
      Líder 0: SNs [0, 4, 8, 12]
      Líder 1: SNs [1, 5, 9, 13]
      Líder 2: SNs [2, 6, 10, 14]
      Líder 3: SNs [3, 7, 11, 15]
  - Isso permite paralelismo: cada líder propõe independentemente.
"""

from .types import SegmentInfo


def create_segments(
    leaders: list[int],
    all_nodes: list[int],
    segment_length: int,
    sn_offset: int,
    bucket_assignment: dict[int, list[int]],
    batch_size: int = 4,
    next_seg_id: int = 0,
) -> list[SegmentInfo]:
    """Cria segmentos para uma epoch — um por líder, SNs intercalados.

    Reproduz a lógica de mirmanager.go createSegments():
      distance = len(leaders)
      seg[i].snOffset = sn_offset + i
      seg[i].sns = [snOffset, snOffset+distance, snOffset+2*distance, ...]

    Args:
        leaders: lista de IDs dos líderes desta epoch
        all_nodes: todos os nós do sistema
        segment_length: quantos SNs por segmento
        sn_offset: primeiro SN da epoch
        bucket_assignment: mapa líder → lista de bucket IDs
        batch_size: tamanho máximo do batch
        next_seg_id: ID do próximo segmento a criar

    Returns:
        Lista de SegmentInfo criados
    """
    distance = len(leaders)
    segments = []

    for i, leader in enumerate(leaders):
        # Followers = todos os nós (todos participam do consenso)
        # Rotação: líder primeiro, depois os demais
        followers = [leader] + [n for n in all_nodes if n != leader]

        seg = SegmentInfo(
            seg_id=next_seg_id + i,
            leader=leader,
            sn_offset=sn_offset + i,
            sn_distance=distance,
            sn_length=segment_length,
            followers=followers,
            bucket_ids=bucket_assignment.get(leader, []),
            batch_size=batch_size,
        )
        segments.append(seg)

    return segments
