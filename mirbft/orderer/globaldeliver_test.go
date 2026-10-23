package orderer

import (
	"reflect"
	"testing"

	pb "github.com/hyperledger-labs/mirbft/protobufs"
)

// Nó em G1 e G2 (como p1/p2 do exemplo do orientador). G3 não é dele.
func newTestDeliverer(got *[]uint64) *GlobalDeliverer {
	mine := map[uint32]bool{1: true, 2: true}
	return NewGlobalDeliverer(func(g uint32) bool { return mine[g] },
		func(d GlobalDelivery) { *got = append(*got, d.GSN) })
}

func req(gsn uint64, groups ...uint32) *pb.ClientRequest {
	return &pb.ClientRequest{GSN: gsn, TouchedGroups: groups, GroupId: groups[0]}
}

type ev struct {
	meta bool
	r    *pb.ClientRequest
}

func run(evs []ev) []uint64 {
	var got []uint64
	d := newTestDeliverer(&got)
	for _, e := range evs {
		if e.meta {
			d.OnMeta(e.r.GSN, e.r.TouchedGroups)
		} else {
			d.OnCommit(e.r)
		}
	}
	return got
}

// G1 decide GSNs 1 e 3, G2 decide 2 e 4; em qualquer intercalação de chegada, a entrega
// tem que ser 1,2,3,4 -- é o que faz dois nós em G1∩G2 concordarem.
func TestGlobalOrderAnyInterleaving(t *testing.T) {
	m := []*pb.ClientRequest{req(1, 1), req(2, 2), req(3, 1), req(4, 2)}
	metas := []ev{{true, m[0]}, {true, m[1]}, {true, m[2]}, {true, m[3]}}
	orders := [][]int{
		{0, 2, 1, 3}, // fila G1 inteira antes de G2
		{1, 3, 0, 2}, // fila G2 inteira antes de G1
		{3, 2, 1, 0}, // tudo ao contrário
		{0, 1, 2, 3},
	}
	for _, o := range orders {
		evs := append([]ev{}, metas...)
		for _, i := range o {
			evs = append(evs, ev{false, m[i]})
		}
		if got := run(evs); !reflect.DeepEqual(got, []uint64{1, 2, 3, 4}) {
			t.Fatalf("ordem de chegada %v: entregue %v, esperado [1 2 3 4]", o, got)
		}
	}
}

// META fora de ordem: não pode entregar o GSN 2 antes de saber o que é o GSN 1.
func TestMetaOutOfOrder(t *testing.T) {
	got := run([]ev{
		{true, req(2, 2)}, {false, req(2, 2)},
		{false, req(1, 1)},
		{true, req(1, 1)},
	})
	if !reflect.DeepEqual(got, []uint64{1, 2}) {
		t.Fatalf("entregue %v, esperado [1 2]", got)
	}
}

// GSN só de G3 (não é deste nó) é pulado sem travar.
func TestSkipForeignGroup(t *testing.T) {
	got := run([]ev{
		{true, req(1, 3)}, {true, req(2, 1)}, {false, req(2, 1)},
	})
	if !reflect.DeepEqual(got, []uint64{2}) {
		t.Fatalf("entregue %v, esperado [2]", got)
	}
}

// Mensagem para G1 e G2 chega pelos dois grupos: entrega uma vez só.
func TestCrossGroupDeliveredOnce(t *testing.T) {
	r := req(1, 1, 2)
	got := run([]ev{{true, r}, {false, r}, {false, r}})
	if !reflect.DeepEqual(got, []uint64{1}) {
		t.Fatalf("entregue %v, esperado [1]", got)
	}
}

// Lotes cruzados (G1 decide {1,4} juntos, G2 decide {2,3} juntos): por requisição não trava.
func TestCrossedBatchesNoDeadlock(t *testing.T) {
	got := run([]ev{
		{true, req(1, 1)}, {true, req(2, 2)}, {true, req(3, 2)}, {true, req(4, 1)},
		{false, req(1, 1)}, {false, req(4, 1)}, // lote de G1
		{false, req(2, 2)}, {false, req(3, 2)}, // lote de G2
	})
	if !reflect.DeepEqual(got, []uint64{1, 2, 3, 4}) {
		t.Fatalf("entregue %v, esperado [1 2 3 4]", got)
	}
}
