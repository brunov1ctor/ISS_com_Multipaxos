// Package clientlib é a API de cliente de verdade pro MultiPaxosMulticast: Put/Get/Transfer
// com escolha explícita de grupo (ou automática por hash, via as versões *Auto), lendo e
// escrevendo um valor real através da máquina de estados em orderer/statemachine.go.
//
// Completamente separado do gerador de carga existente (cmd/orderingclient/client.go) -- não
// o modifica nem depende dele; só reaproveita as peças de infraestrutura de conexão já
// usadas por ele (config, membership, discovery, crypto, messenger).
package clientlib

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	logger "github.com/rs/zerolog/log"

	"github.com/hyperledger-labs/mirbft/config"
	"github.com/hyperledger-labs/mirbft/crypto"
	"github.com/hyperledger-labs/mirbft/discovery"
	"github.com/hyperledger-labs/mirbft/membership"
	"github.com/hyperledger-labs/mirbft/messenger"
	pb "github.com/hyperledger-labs/mirbft/protobufs"
	"github.com/hyperledger-labs/mirbft/request"
)

// DefaultTimeout é quanto cada chamada (Put/Get/Transfer) espera por resposta antes de
// desistir, caso Client.Timeout não seja ajustado.
const DefaultTimeout = 10 * time.Second

type Client struct {
	Timeout time.Duration

	ownID      int32
	privKey    interface{}
	reqClients map[int32]pb.Messenger_RequestClient
	orderers   []int32
	nextSn     int32
	rr         int32
	pending    sync.Map // int32 clientSn -> chan *pb.ClientResponse
}

// Connect carrega configFile (mesmo formato usado por cmd/orderingclient), registra no
// servidor de discovery e conecta em todo orderer conhecido (reaproveitando
// messenger.ConnectToOrderers, que já cuida de discar, TLS e do handshake inicial de
// request/resposta dummy -- não precisamos repetir nada disso aqui).
//
// Deliberadamente NÃO chama request.Init(): essa função só monta infraestrutura
// server-side de buckets/workers, irrelevante pra um cliente puro.
func Connect(configFile, discoveryAddr string) (*Client, error) {
	config.LoadFile(configFile)
	membership.Init()

	ownID, identities := discovery.RegisterClient(discoveryAddr)
	membership.InitNodeIdentities(identities)

	c := &Client{ownID: ownID, orderers: membership.AllNodeIDs(), Timeout: DefaultTimeout}
	if len(c.orderers) == 0 {
		return nil, fmt.Errorf("discovery server returned no known orderers")
	}

	if config.Config.SignRequests {
		pk, err := crypto.PrivateKeyFromFile(config.Config.ClientPrivKeyFile)
		if err != nil {
			return nil, fmt.Errorf("load client private key (%s): %w", config.Config.ClientPrivKeyFile, err)
		}
		c.privKey = pk
	}

	reqClients, _, _ := messenger.ConnectToOrderers(ownID, logger.Logger, c.orderers)
	if len(reqClients) == 0 {
		return nil, fmt.Errorf("could not connect to any orderer")
	}
	c.reqClients = reqClients

	for nodeID, rc := range reqClients {
		go c.recvLoop(nodeID, rc)
	}
	return c, nil
}

// recvLoop entrega toda ClientResponse recebida nesta conexão pro chamador que está
// esperando por aquele clientSn (via Client.call). O handshake inicial já foi consumido
// inteiramente dentro de messenger.ConnectToOrderers antes desta goroutine começar, então
// toda resposta que chega aqui é de uma chamada real.
func (c *Client) recvLoop(nodeID int32, rc pb.Messenger_RequestClient) {
	for {
		resp, err := rc.Recv()
		if err != nil {
			return
		}
		if ch, ok := c.pending.Load(resp.ClientSn); ok {
			select {
			case ch.(chan *pb.ClientResponse) <- resp:
			default:
			}
		}
	}
}

func (c *Client) nextClientSn() int32 { return atomic.AddInt32(&c.nextSn, 1) }

// pickProxy escolhe pra qual orderer conectado mandar a requisição desta chamada. Qualquer
// orderer serve de proxy, independente de pertencer ao grupo alvo -- orderer.PreprocessRequest
// cuida do roteamento (ver plano); round-robin só distribui a carga entre os conhecidos,
// mesmo padrão usado por cmd/orderingclient/client.go (guessTargetOrderers).
func (c *Client) pickProxy() pb.Messenger_RequestClient {
	idx := int(atomic.AddInt32(&c.rr, 1)) % len(c.orderers)
	return c.reqClients[c.orderers[idx]]
}

// call monta e envia o ClientRequest com payload já codificado (ver request/command.go;
// TouchedGroups vazio quando touchedGroups é nil -- deixa o servidor decidir por hash, ver
// orderer.ReplicaMapper), e espera a resposta real (com valor, ver orderer/statemachine.go)
// ou timeout.
func (c *Client) call(touchedGroups []uint32, payload []byte) (*pb.ClientResponse, error) {
	sn := c.nextClientSn()
	req := &pb.ClientRequest{
		RequestId:     &pb.RequestID{ClientId: c.ownID, ClientSn: sn},
		Payload:       payload,
		TouchedGroups: touchedGroups, // GroupId deliberadamente deixado 0 -- setado
		// server-side (orderer/multipaxosmulticastorderer.go PreprocessRequest), igual já
		// acontece com o cliente de benchmark pra TouchedGroups vazio.
	}
	var err error
	if config.Config.SignRequests {
		req.Signature, err = crypto.Sign(request.Digest(req), c.privKey)
		if err != nil {
			return nil, err
		}
	}

	ch := make(chan *pb.ClientResponse, 1)
	c.pending.Store(sn, ch)
	defer c.pending.Delete(sn)

	if err := c.pickProxy().Send(req); err != nil {
		return nil, err
	}

	select {
	case resp := <-ch:
		if resp.Error != "" {
			return resp, fmt.Errorf("application error: %s", resp.Error)
		}
		return resp, nil
	case <-time.After(c.Timeout):
		return nil, fmt.Errorf("timed out waiting for response to clientSn=%d", sn)
	}
}

// Ls pede a topologia (nós conhecidos + grupos definidos) pro proxy -- payload "APILIST" é
// reconhecido e respondido direto em orderer/multipaxosmulticastorderer.go:PreprocessRequest,
// sem passar por consenso (é config estática, igual em todo nó, não estado replicado).
func (c *Client) Ls() (string, error) {
	resp, err := c.call(nil, []byte("APILIST"))
	if err != nil {
		return "", err
	}
	return string(resp.Value), nil
}

// Put grava value na key, dentro do groupID escolhido explicitamente pelo chamador.
func (c *Client) Put(groupID uint32, key string, value []byte) error {
	_, err := c.call([]uint32{groupID}, request.EncodePut(key, value))
	return err
}

// PutAuto grava value na key, deixando o servidor escolher o grupo por hash da chave
// (TouchedGroups vazio -> orderer.ReplicaMapper), igual o comportamento padrão de hoje.
func (c *Client) PutAuto(key string, value []byte) error {
	_, err := c.call(nil, request.EncodePut(key, value))
	return err
}

// Get lê key de dentro do groupID escolhido explicitamente.
func (c *Client) Get(groupID uint32, key string) ([]byte, bool, error) {
	resp, err := c.call([]uint32{groupID}, request.EncodeGet(key))
	if err != nil {
		return nil, false, err
	}
	return resp.Value, resp.Found, nil
}

// GetAuto lê key, deixando o servidor escolher o grupo por hash (mesmo esquema de PutAuto).
func (c *Client) GetAuto(key string) ([]byte, bool, error) {
	resp, err := c.call(nil, request.EncodeGet(key))
	if err != nil {
		return nil, false, err
	}
	return resp.Value, resp.Found, nil
}

// Delete remove key do groupID escolhido explicitamente. Devolve se a chave existia antes --
// não é erro apagar uma chave que não existe (idempotente, igual delete() de map do Go).
func (c *Client) Delete(groupID uint32, key string) (bool, error) {
	resp, err := c.call([]uint32{groupID}, request.EncodeDelete(key))
	if err != nil {
		return false, err
	}
	return resp.Found, nil
}

// DeleteAuto remove key, deixando o servidor escolher o grupo por hash (mesmo esquema de
// PutAuto/GetAuto).
func (c *Client) DeleteAuto(key string) (bool, error) {
	resp, err := c.call(nil, request.EncodeDelete(key))
	if err != nil {
		return false, err
	}
	return resp.Found, nil
}

// Transfer move amount de fromKey (no grupo fromGroup) pra toKey (no grupo toGroup). Ver
// limitação documentada no plano: sem atomicidade/rollback entre grupos -- débito e crédito
// aplicam incondicionalmente, só com ordem relativa consistente garantida (via GSN/ADeliver).
func (c *Client) Transfer(fromGroup uint32, fromKey string, toGroup uint32, toKey string, amount int64) error {
	groups := []uint32{fromGroup, toGroup}
	if fromGroup > toGroup {
		groups = []uint32{toGroup, fromGroup}
	}
	_, err := c.call(groups, request.EncodeTransfer(fromGroup, fromKey, toGroup, toKey, amount))
	return err
}

// Multicast envia payload para o grupo. A ordem (local ou global) é do sistema, definida no
// deploy (TotalOrder no config dos nós), não pelo cliente. Payloads no formato
// da API (PUT/GET...) são aplicados na máquina de estados; os demais só são ordenados e
// entregues (log GDELIVER nos nós, no modo global).
func (c *Client) Multicast(groupID uint32, payload []byte) (*pb.ClientResponse, error) {
	return c.MulticastGroups([]uint32{groupID}, payload)
}

// MulticastGroups envia payload para vários grupos ao mesmo tempo.
func (c *Client) MulticastGroups(groups []uint32, payload []byte) (*pb.ClientResponse, error) {
	if len(groups) == 0 {
		return nil, fmt.Errorf("multicast sem grupo de destino")
	}
	return c.call(groups, payload)
}

func (c *Client) Close() {}
