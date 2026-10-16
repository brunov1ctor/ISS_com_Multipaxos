package request

import (
	"encoding/base64"
	"strconv"
	"strings"
)

// Verbos da API de cliente (clientlib), no mesmo estilo textual que o payload sintético do
// benchmark já usa ("GET K00000005", "TX k1,k2", ...), só com prefixo "API" pra nunca colidir
// com esses (o benchmark nunca produz "APIPUT"/"APIGET"/"APITRANSFER"). Valores são
// base64-codificados no payload pra suportar bytes arbitrários sem ambiguidade com os
// espaços usados como separador de campo.
const (
	VerbPut      = "APIPUT"
	VerbGet      = "APIGET"
	VerbDelete   = "APIDELETE"
	VerbTransfer = "APITRANSFER"
)

func EncodePut(key string, value []byte) []byte {
	return []byte(VerbPut + " " + key + " " + base64.StdEncoding.EncodeToString(value))
}

func EncodeGet(key string) []byte {
	return []byte(VerbGet + " " + key)
}

func EncodeDelete(key string) []byte {
	return []byte(VerbDelete + " " + key)
}

// EncodeTransfer embute os IDs de grupo direto no payload (fromGroup/toGroup), diferente de
// Put/Get: Transfer sempre exige grupo explícito (ver clientlib.Transfer), já que não tem
// como o hash automático decidir sozinho qual lado (débito/crédito) cada réplica deve
// aplicar -- cada réplica só sabe, lendo o payload, se o grupo dela é o "from" ou o "to".
func EncodeTransfer(fromGroup uint32, fromKey string, toGroup uint32, toKey string, amount int64) []byte {
	return []byte(VerbTransfer + " " +
		strconv.FormatUint(uint64(fromGroup), 10) + " " + fromKey + " " +
		strconv.FormatUint(uint64(toGroup), 10) + " " + toKey + " " +
		strconv.FormatInt(amount, 10))
}

// DecodedCommand é o resultado de decodificar um payload da API de cliente.
type DecodedCommand struct {
	Verb  string
	Key   string // APIPUT/APIGET
	Value []byte // APIPUT

	FromGroup uint32 // APITRANSFER
	ToGroup   uint32 // APITRANSFER
	ToKey     string // APITRANSFER (Key acima é o fromKey)
	Amount    int64  // APITRANSFER
}

// DecodeAPICommand reconhece um payload como um dos verbos da API de cliente, ou devolve
// ok=false pra qualquer outra coisa (inclusive todo o payload sintético do benchmark
// existente, que nunca usa esses verbos).
func DecodeAPICommand(payload []byte) (DecodedCommand, bool) {
	parts := strings.Fields(string(payload))
	if len(parts) == 0 {
		return DecodedCommand{}, false
	}
	switch parts[0] {
	case VerbPut:
		if len(parts) < 3 {
			return DecodedCommand{}, false
		}
		value, err := base64.StdEncoding.DecodeString(parts[2])
		if err != nil {
			return DecodedCommand{}, false
		}
		return DecodedCommand{Verb: VerbPut, Key: parts[1], Value: value}, true
	case VerbGet:
		if len(parts) < 2 {
			return DecodedCommand{}, false
		}
		return DecodedCommand{Verb: VerbGet, Key: parts[1]}, true
	case VerbDelete:
		if len(parts) < 2 {
			return DecodedCommand{}, false
		}
		return DecodedCommand{Verb: VerbDelete, Key: parts[1]}, true
	case VerbTransfer:
		// APITRANSFER fromGroup fromKey toGroup toKey amount
		if len(parts) < 6 {
			return DecodedCommand{}, false
		}
		fromGroup, err1 := strconv.ParseUint(parts[1], 10, 32)
		toGroup, err2 := strconv.ParseUint(parts[3], 10, 32)
		amount, err3 := strconv.ParseInt(parts[5], 10, 64)
		if err1 != nil || err2 != nil || err3 != nil {
			return DecodedCommand{}, false
		}
		return DecodedCommand{
			Verb: VerbTransfer, FromGroup: uint32(fromGroup), Key: parts[2],
			ToGroup: uint32(toGroup), ToKey: parts[4], Amount: amount,
		}, true
	}
	return DecodedCommand{}, false
}
