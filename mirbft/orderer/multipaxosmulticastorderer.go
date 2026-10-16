// MultiPaxosMulticast Orderer - Coordena múltiplos grupos MultiPaxos com atomic multicast.
// Grupo 0 = sequenciador GSN. Grupos 1+ = dados. Cross-group via GSN + META stream.
package orderer

import (
	"encoding/base64"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"github.com/hyperledger-labs/mirbft/config"
	"github.com/hyperledger-labs/mirbft/crypto"
	"github.com/hyperledger-labs/mirbft/log"
	"github.com/hyperledger-labs/mirbft/manager"
	"github.com/hyperledger-labs/mirbft/membership"
	"github.com/hyperledger-labs/mirbft/messenger"
	"github.com/hyperledger-labs/mirbft/request"
	"github.com/hyperledger-labs/mirbft/tracing"
	pb "github.com/hyperledger-labs/mirbft/protobufs"
	logger "github.com/rs/zerolog/log"
)

const (
	SYSTEM_META_STREAM = "SYSTEM:META_STREAM:"
	SYSTEM_GSN_REQUEST = "SYSTEM:GSN_REQUEST:"

	// GSN sequencer leader election (failover)
	SYSTEM_SEQ_VOTE_REQUEST  = "SYSTEM:SEQ_VOTE_REQUEST:"
	SYSTEM_SEQ_VOTE_RESPONSE = "SYSTEM:SEQ_VOTE_RESPONSE:"
	SYSTEM_SEQ_HEARTBEAT     = "SYSTEM:SEQ_HEARTBEAT:"
)

var globalMulticastOrderer *MultiPaxosMulticastOrderer

func GetGlobalMulticastOrderer() *MultiPaxosMulticastOrderer { return globalMulticastOrderer }

type MultiPaxosMulticastOrderer struct {
	groupOrderers  map[uint32]*MultiPaxosOrderer // grupo de dados -> orderer MultiPaxos responsável (grupo 0 é tratado à parte, pelo Sequencer)
	orderersMu     sync.RWMutex                  // protege groupOrderers
	am             *AtomicMulticast              // registro compartilhado de grupos/membros (multicast atômico)
	seq            *Sequencer // Sequenciador dedicado (independente do ISS)
	mgr            manager.Manager               // manager do ISS; usado só para assinar novos segmentos (Start -> SubscribeOrderer)
	groupsFilePath string                        // caminho do YAML com a definição dos grupos (groups.yml)
	// CSMR Output Processing: proxy tracks pending requests
	proxyPending   sync.Map // key: "clientId:clientSn" -> int32 (proxyNodeID)
}

// PendingCommit é um commit já decidido pelo grupo mas ainda esperando o META (ver
// aDeliverInternal em sequencer.go) para poder ser entregue na ordem global correta. A chave
// (gsn, groupID) já fica em s.pendingCommits[groupID][gsn], então não precisa ser duplicada aqui.
type PendingCommit struct {
	batch    *pb.Batch
	announce func(int32, *pb.Batch, []byte)
	sn       int32
	digest   []byte
}

func (o *MultiPaxosMulticastOrderer) Init(mngr manager.Manager) {
	globalMulticastOrderer = o
	o.mgr = mngr
	// Init components
	o.am = NewAtomicMulticast()
	fmt.Printf("[MULTICAST] NewAtomicMulticast: group0 members=%v\n", o.am.GetGroupMembers(0))
	o.groupOrderers = make(map[uint32]*MultiPaxosOrderer)
	// Load groups
	if o.groupsFilePath == "" { o.groupsFilePath = "/tmp/iss-Bruno/config/groups.yml" }
	if err := o.am.LoadGroupsFromYAML(o.groupsFilePath); err != nil {
		logger.Fatal().Err(err).Msg("Failed to load groups")
	}
	o.am.UpdateSequencerGroup()
	fmt.Printf("[MULTICAST] Groups loaded: %v group0=%v\n", o.am.GetDefinedGroups(), o.am.GetGroupMembers(0))
	// Create standalone sequencer (independent from ISS)
	o.seq = NewSequencer(o.am.GetGroupMembers(0))
	o.seq.Start()
	// Create group orderers for DATA groups only (not group 0)
	for _, gid := range o.am.GetDefinedGroups() {
		if gid == 0 { continue } // Group 0 handled by Sequencer
		ord := &MultiPaxosOrderer{am: o.am, ownedGroupID: gid, skipHandlerRegistration: true, commitNotifyCh: make(chan struct{}, 16)}
		ord.segmentChan = make(chan manager.Segment, 64)
		ord.Init(mngr)
		o.groupOrderers[gid] = ord
	}
	// Setup handlers
	messenger.OrdererMsgHandler = o.HandleMessage
	request.SetGSNGenerator(o.seq.GetNextGSN)
	request.SetGroupMembersGetter(o.GetGroupMembers)
	request.SetRequestPreprocessor(o.PreprocessRequest)
}

func (o *MultiPaxosMulticastOrderer) Start(wg *sync.WaitGroup) {
	// Bootstrap: create initial instances so groups are ready before client sends requests.
	// This runs AFTER messenger.Connect() and discovery.SyncPeer(), so peer connections exist.
	numNodes := int32(len(membership.AllNodeIDs()))
	if numNodes < 1 { numNodes = 1 }
	snLength := int32(config.Config.SegmentLength) * numNodes
	initialSeg := &manager.ContiguousSegment{}
	initialSeg.SetFields(0, membership.AllNodeIDs(), membership.AllNodeIDs(), 0, snLength, -1)
	for gid, ord := range o.groupOrderers {
		members := o.am.GetGroupMembers(gid)
		if members == nil { continue }
		// firstSN usa o índice denso 0-based do grupo (não o gid literal), pro mesmo motivo de
		// runSegment: grupo 0 (Sequencer) não entra no stride, senão aquele SN nunca é preenchido.
		firstSN, _ := o.am.GetDataGroupIndex(gid)
		if firstSN < 0 { firstSN = 0 }
		inst := ord.ensureInstance(firstSN)
		inst.setSegment(initialSeg)
		inst.bucketId = gid
		inst.SetMembers(members)
		ord.dispatcher.store(firstSN, inst)
		inst.startWorkers()
		ord.RunSegmentDirect(initialSeg)
		fmt.Printf("[MULTICAST] Bootstrap: group=%d firstSN=%d members=%v ready\n", gid, firstSN, members)
	}
	fmt.Printf("[MULTICAST] Bootstrap: %d data groups ready (SN 0-%d)\n", len(o.groupOrderers), initialSeg.LastSN())

	segCh := o.mgr.SubscribeOrderer()
	go func() {
		for seg := range segCh {
			o.orderersMu.RLock()
			for _, ord := range o.groupOrderers {
				if ord != nil && ord.segmentChan != nil {
					select {
					case ord.segmentChan <- seg:
					default:
					}
				}
			}
			o.orderersMu.RUnlock()
		}
	}()
	o.orderersMu.RLock()
	defer o.orderersMu.RUnlock()
	for _, orderer := range o.groupOrderers { orderer.Start(wg) }
}

func (o *MultiPaxosMulticastOrderer) HandleMessage(pm *pb.ProtocolMessage) {
	if gsnForward := pm.GetGsnReqForward(); gsnForward != nil {
		payload := string(gsnForward.Req.Payload)
		// GSN_RESPONSE -> Sequencer
		if strings.HasPrefix(payload, "SYSTEM:GSN_RESPONSE:") {
			o.seq.HandleGSNResponse(payload)
			return
		}
		// GSN_REQUEST -> Sequencer (leader processes)
		if strings.HasPrefix(payload, SYSTEM_GSN_REQUEST) {
			o.seq.HandleGSNRequest(payload, pm.SenderId)
			return
		}
		// META_STREAM -> Sequencer
		if strings.HasPrefix(payload, SYSTEM_META_STREAM) {
			req := gsnForward.Req
			o.seq.HandleMETAStream(payload, req.GetTouchedGroups(), req.GetGSN())
			return
		}
		// Sequencer leader election (failover)
		if strings.HasPrefix(payload, SYSTEM_SEQ_VOTE_REQUEST) {
			o.seq.HandleVoteRequest(payload, pm.SenderId)
			return
		}
		if strings.HasPrefix(payload, SYSTEM_SEQ_VOTE_RESPONSE) {
			o.seq.HandleVoteResponse(payload, pm.SenderId)
			return
		}
		if strings.HasPrefix(payload, SYSTEM_SEQ_HEARTBEAT) {
			o.seq.HandleHeartbeat(payload, pm.SenderId)
			return
		}
		// CSMR Output Processing: COMMIT_NOTIFY_BATCH from group replica back to proxy. Cada
		// entrada é "clientId,clientSn" (legado, nunca mudou) ou, pra requisições da API de
		// cliente (ver orderer/statemachine.go), "clientId,clientSn,valorBase64,found,erroBase64"
		// -- mesmo canal de texto de sempre, só com campos extras opcionais por entrada.
		if strings.HasPrefix(payload, "SYSTEM:COMMIT_NOTIFY_BATCH:") {
			// Format: "SYSTEM:COMMIT_NOTIFY_BATCH:orderSn:entry:entry:..."
			parts := strings.SplitN(payload, ":", 4)
			if len(parts) >= 4 {
				var orderSn int32
				fmt.Sscanf(parts[2], "%d", &orderSn)
				entries := strings.Split(parts[3], ":")
				for _, entry := range entries {
					fields := strings.Split(entry, ",")
					if len(fields) < 2 { continue }
					clientId64, err1 := strconv.ParseInt(fields[0], 10, 32)
					clientSn64, err2 := strconv.ParseInt(fields[1], 10, 32)
					if err1 != nil || err2 != nil { continue }
					clientId, clientSn := int32(clientId64), int32(clientSn64)
					if len(fields) >= 5 {
						value, _ := base64.StdEncoding.DecodeString(fields[2])
						errBytes, _ := base64.StdEncoding.DecodeString(fields[4])
						o.handleCommitNotify(clientId, clientSn, orderSn, value, fields[3] == "1", string(errBytes))
					} else {
						o.handleCommitNotify(clientId, clientSn, orderSn, nil, false, "")
					}
				}
			}
			return
		}
		// Legacy single COMMIT_NOTIFY (backward compat)
		if strings.HasPrefix(payload, "SYSTEM:COMMIT_NOTIFY:") {
			var clientId, clientSn, orderSn int32
			if n, _ := fmt.Sscanf(payload, "SYSTEM:COMMIT_NOTIFY:%d:%d:%d", &clientId, &clientSn, &orderSn); n == 3 {
				o.handleCommitNotify(clientId, clientSn, orderSn, nil, false, "")
			}
			return
		}
		// Inject into bucket (data requests)
		req := gsnForward.Req
		if req != nil && req.GetRequestId() != nil {
			if o.am.IsMember(req.GroupId, membership.OwnID) {
				// All forwarded requests bypass Buffer/watermark
				// (watermark is managed by the proxy that received from client)
				request.AddDirectToBucket(req)
			}
		}
		return
	}
	if me := pm.GetMissingEntry(); me != nil {
		var groupID uint32
		if me.Batch != nil && len(me.Batch.Requests) > 0 {
			groupID = me.Batch.Requests[0].GetGroupId()
		} else {
			o.orderersMu.RLock()
			for _, ord := range o.groupOrderers { ord.HandleMessage(pm) }
			o.orderersMu.RUnlock()
			return
		}
		o.orderersMu.RLock()
		if ord := o.groupOrderers[groupID]; ord != nil { ord.HandleMessage(pm) }
		o.orderersMu.RUnlock()
		return
	}
	mpx := pm.GetMultipaxos()
	if mpx == nil {
		o.orderersMu.RLock()
		for _, ord := range o.groupOrderers { ord.HandleMessage(pm) }
		o.orderersMu.RUnlock()
		return
	}
	groupID := extractGroupID(mpx)
	o.orderersMu.RLock()
	if ord := o.groupOrderers[groupID]; ord != nil { ord.HandleMessage(pm) }
	o.orderersMu.RUnlock()
}

func (o *MultiPaxosMulticastOrderer) IsMember(groupID uint32, nodeID int32) bool {
	return o.am != nil && o.am.IsMember(groupID, nodeID)
}

func (o *MultiPaxosMulticastOrderer) GetGroupMembers(groupID uint32) []int32 {
	if o.am == nil { return nil }
	return o.am.GetGroupMembers(groupID)
}

// Delegate to Sequencer
func (o *MultiPaxosMulticastOrderer) RegisterGSNMetadata(gsn uint64, touchedGroups []uint32) {
	o.seq.RegisterMetadata(gsn, touchedGroups)
}

func (o *MultiPaxosMulticastOrderer) ADeliver(gsn uint64, groupID uint32, _ *pb.Batch) bool {
	return o.seq.ADeliver(gsn, groupID)
}

func (o *MultiPaxosMulticastOrderer) BufferCommit(gsn uint64, groupID uint32, batch *pb.Batch, announce func(int32, *pb.Batch, []byte), sn int32, digest []byte) {
	o.seq.BufferCommit(gsn, groupID, batch, announce, sn, digest)
}

func (o *MultiPaxosMulticastOrderer) sendToGroup(req *pb.ClientRequest, groupID uint32) {
	members := o.am.GetGroupMembers(groupID)
	if len(members) == 0 { return }
	for _, nodeID := range members {
		if nodeID == membership.OwnID {
			// Bypass Buffer/watermark — inject directly into bucket
			request.AddDirectToBucket(req)
			break
		}
	}
	for _, nodeID := range members {
		if nodeID == membership.OwnID { continue }
		messenger.EnqueueMsg(&pb.ProtocolMessage{SenderId: membership.OwnID, Sn: -1,
			Msg: &pb.ProtocolMessage_GsnReqForward{GsnReqForward: &pb.GSNReqForward{Req: req}}}, nodeID)
	}
}

// CSMR Output Processing: handle COMMIT_NOTIFY from group replicas. value/found/errStr vêm
// vazios (nil/false/"") pro tráfego de benchmark legado; só a API de cliente (ver
// orderer/statemachine.go) preenche eles de verdade.
func (o *MultiPaxosMulticastOrderer) handleCommitNotify(clientId, clientSn, orderSn int32, value []byte, found bool, errStr string) {
	key := fmt.Sprintf("%d:%d", clientId, clientSn)
	if _, loaded := o.proxyPending.LoadAndDelete(key); loaded {
		messenger.RespondToClient(clientId, &pb.ClientResponse{
			OrderSn: orderSn, ClientSn: clientSn,
			Value: value, Found: found, Error: errStr,
		})
		tracing.MainTrace.Event(tracing.RESP_SEND, int64(clientId), int64(clientSn))
	}
}

// CSMR Output Processing: called by group orderer after commit to notify proxy.
// Sends ONE batched COMMIT_NOTIFY per commit (not per request).
// Only sends to peers if the proxy is NOT a member of this group.
//
// results é opcional (nil no caminho de benchmark legado, chamado de o.announce): quando
// presente (vindo de orderer.ApplyBatch via applyEntryInOrder, chaveado por
// reqKey(clientId,clientSn)), carrega o valor real de cada requisição da API de cliente, e é
// isso que diferencia uma resposta de verdade de um simples ack de posição de ordem.
func (o *MultiPaxosMulticastOrderer) NotifyProxy(batch *pb.Batch, sn int32, results map[int64]*pb.ClientResponse) {
	if batch == nil { return }

	// Determine which group this batch belongs to
	var groupID uint32
	if len(batch.Requests) > 0 && batch.Requests[0] != nil {
		groupID = batch.Requests[0].GroupId
	}
	groupMembers := o.am.GetGroupMembers(groupID)

	// Collect requests that need remote notification
	var localKeys []struct{ clientId, clientSn int32 }
	var remoteReqs []*pb.RequestID

	for _, req := range batch.Requests {
		if req == nil || req.RequestId == nil { continue }
		if strings.HasPrefix(string(req.Payload), "SYSTEM:") { continue }
		clientId := req.RequestId.ClientId
		clientSn := req.RequestId.ClientSn
		key := fmt.Sprintf("%d:%d", clientId, clientSn)
		if _, isProxy := o.proxyPending.Load(key); isProxy {
			localKeys = append(localKeys, struct{ clientId, clientSn int32 }{clientId, clientSn})
		} else {
			if len(groupMembers) < len(membership.AllNodeIDs()) {
				remoteReqs = append(remoteReqs, req.RequestId)
			}
		}
	}

	resultFor := func(clientId, clientSn int32) (value []byte, found bool, errStr string) {
		if results == nil { return nil, false, "" }
		if r, ok := results[reqKey(clientId, clientSn)]; ok {
			return r.Value, r.Found, r.Error
		}
		return nil, false, ""
	}

	// Handle local proxy responses
	for _, k := range localKeys {
		value, found, errStr := resultFor(k.clientId, k.clientSn)
		o.handleCommitNotify(k.clientId, k.clientSn, sn, value, found, errStr)
	}

	// Send ONE batched COMMIT_NOTIFY to all peers (instead of N messages). Cada entrada é só
	// "clientId,clientSn" (igual sempre foi) quando não há valor real; ganha 3 campos extras
	// em base64 (valor, found, erro) quando há -- ver parsing em HandleMessage.
	if len(remoteReqs) > 0 {
		var sb strings.Builder
		fmt.Fprintf(&sb, "SYSTEM:COMMIT_NOTIFY_BATCH:%d", sn)
		for _, rid := range remoteReqs {
			value, found, errStr := resultFor(rid.ClientId, rid.ClientSn)
			if results != nil {
				fmt.Fprintf(&sb, ":%d,%d,%s,%d,%s", rid.ClientId, rid.ClientSn,
					base64.StdEncoding.EncodeToString(value), boolToInt(found),
					base64.StdEncoding.EncodeToString([]byte(errStr)))
			} else {
				fmt.Fprintf(&sb, ":%d,%d", rid.ClientId, rid.ClientSn)
			}
		}
		notifyPayload := sb.String()
		for _, nodeID := range membership.AllNodeIDs() {
			if nodeID == membership.OwnID { continue }
			messenger.EnqueueMsg(&pb.ProtocolMessage{
				SenderId: membership.OwnID, Sn: sn,
				Msg: &pb.ProtocolMessage_GsnReqForward{GsnReqForward: &pb.GSNReqForward{
					Req: &pb.ClientRequest{
						RequestId: &pb.RequestID{ClientId: membership.OwnID, ClientSn: sn},
						Payload:   []byte(notifyPayload),
					},
				}},
			}, nodeID)
		}
	}
}

func (o *MultiPaxosMulticastOrderer) Sign(data []byte) ([]byte, error) {
	if membership.OwnPrivKey == nil { return nil, fmt.Errorf("private key not initialized") }
	return crypto.Sign(data, membership.OwnPrivKey)
}

func (o *MultiPaxosMulticastOrderer) CheckSig(data []byte, senderID int32, signature []byte) error {
	if !config.Config.SignRequests { return nil }
	ni := membership.NodeIdentity(senderID)
	if ni == nil || ni.PubKey == nil { return fmt.Errorf("public key not found for node %d", senderID) }
	pubKey, err := crypto.PublicKeyFromBytes(ni.PubKey)
	if err != nil { return err }
	return crypto.CheckSig(data, pubKey, signature)
}

func (o *MultiPaxosMulticastOrderer) HandleEntry(entry *log.Entry) {
	if entry == nil { return }
	var groupID uint32
	if entry.Batch != nil && len(entry.Batch.Requests) > 0 { groupID = entry.Batch.Requests[0].GetGroupId() }
	o.orderersMu.RLock()
	if ord := o.groupOrderers[groupID]; ord != nil { ord.HandleEntry(entry) }
	o.orderersMu.RUnlock()
}



func containsUint32(list []uint32, v uint32) bool {
	for _, x := range list { if x == v { return true } }
	return false
}

func boolToInt(b bool) int {
	if b { return 1 }
	return 0
}

// topologyDump monta o texto que "orderingclient ls" imprime: todo nó conhecido (com seu
// endereço, via membership -- preenchido no handshake de discovery, igual pra todo mundo) e
// todo grupo definido (groups.yml, carregado uma vez no Start() -- ver o.am), com seus membros.
// Puramente informativo: lê só estado local já carregado neste nó, nunca entra em consenso.
func (o *MultiPaxosMulticastOrderer) topologyDump() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Orderer: %s\n\n", config.Config.Orderer)

	fmt.Fprintf(&b, "Nos (%d):\n", len(membership.AllNodeIDs()))
	for _, id := range membership.AllNodeIDs() {
		if ni := membership.NodeIdentity(id); ni != nil {
			fmt.Fprintf(&b, "  Node %d: %s (privado %s)\n", id, ni.PublicAddr, ni.PrivateAddr)
		} else {
			fmt.Fprintf(&b, "  Node %d\n", id)
		}
	}

	fmt.Fprintf(&b, "\nGrupos (%d):\n", len(o.am.GetDefinedGroups()))
	for _, gid := range o.am.GetDefinedGroups() {
		label := "dados"
		if gid == 0 {
			label = "sequenciador GSN"
		}
		fmt.Fprintf(&b, "  G%d (%s): membros %v\n", gid, label, o.am.GetGroupMembers(gid))
	}
	return b.String()
}

func (o *MultiPaxosMulticastOrderer) PreprocessRequest(req *pb.ClientRequest) bool {
	// "orderingclient ls": consulta de topologia, não é operação da máquina de estados -- não
	// precisa de consenso nenhum (todo orderer já carregou o MESMO groups.yml estático no
	// início, não é estado replicado que muda). Respondida direto por QUALQUER nó que receber
	// (o proxy escolhido por round-robin, igual qualquer outro comando), sem passar por
	// sendToGroup/ReplicaMapper -- só esse nó bate e volta.
	if string(req.Payload) == "APILIST" && req.GetRequestId() != nil {
		messenger.RespondToClient(req.RequestId.ClientId, &pb.ClientResponse{
			ClientSn: req.RequestId.ClientSn,
			Value:    []byte(o.topologyDump()),
			Found:    true,
		})
		return true
	}
	// System messages bypass preprocessor
	if strings.HasPrefix(string(req.Payload), SYSTEM_GSN_REQUEST) || strings.HasPrefix(string(req.Payload), SYSTEM_META_STREAM) {
		return request.AddSystemMessage(req)
	}
	// COMMIT_NOTIFY bypass
	if strings.HasPrefix(string(req.Payload), "SYSTEM:COMMIT_NOTIFY") { return true }
	if req.GetRequestId() == nil { return true }

	// Já foi preprocessada antes — cada guard cobre um caso de requisição que já foi roteada
	// por este método anteriormente (ex: reenviada de outro nó) e não deve ser processada de novo:
	//   1) cross-group que já tem GSN (já passou pelo sequenciador e foi despachada);
	//   2) single-group que já foi roteada pro seu único grupo;
	//   3) cross-group que ainda não tem GSN, mas já foi marcada com GroupId (está em trânsito
	//      pra um grupo específico, aguardando o GSN chegar por outro caminho).
	if req.GroupId > 0 && req.GSN > 0 { return false }
	if req.GroupId > 0 && len(req.TouchedGroups) == 1 { return false }
	if req.GroupId > 0 && len(req.TouchedGroups) > 1 && req.GSN == 0 { return false }

	// Block other SYSTEM messages
	if strings.HasPrefix(string(req.Payload), "SYSTEM:") { return true }

	// API de cliente: se o cliente já escolheu os grupos explicitamente (TouchedGroups
	// preenchido antes de chegar aqui), valida que todos existem de verdade antes de
	// registrar o proxy -- nada disso roda pro caminho de benchmark existente, que sempre
	// chega aqui com TouchedGroups vazio.
	if len(req.TouchedGroups) > 0 {
		defined := o.am.GetDefinedGroups()
		for _, g := range req.TouchedGroups {
			if g == 0 || !containsUint32(defined, g) {
				logger.Error().Uint32("groupId", g).Int32("clientId", req.RequestId.ClientId).
					Msg("PreprocessRequest: rejeitando grupo nao definido escolhido pelo cliente")
				return true
			}
		}
	}

	// CSMR: Register this node as proxy for this request (Output Processing)
	key := fmt.Sprintf("%d:%d", req.RequestId.ClientId, req.RequestId.ClientSn)
	o.proxyPending.Store(key, membership.OwnID)

	// Map to groups via ReplicaMapper
	if len(req.TouchedGroups) == 0 {
		req.TouchedGroups = request.ReplicaMapper(req.Payload)
		sort.Slice(req.TouchedGroups, func(i, j int) bool { return req.TouchedGroups[i] < req.TouchedGroups[j] })
	}
	// Remove group 0
	filtered := req.TouchedGroups[:0]
	for _, g := range req.TouchedGroups { if g != 0 { filtered = append(filtered, g) } }
	req.TouchedGroups = filtered
	if len(req.TouchedGroups) == 0 { return true }

	// Single-group
	if len(req.TouchedGroups) == 1 {
		req.GroupId = req.TouchedGroups[0]
		// All paths use AddDirectToBucket (bypass Buffer/watermark)
		// Watermark is managed by PROXY-RESPOND/COMMIT_NOTIFY, not by peer Buffer
		o.sendToGroup(req, req.GroupId)
		return true
	}

	// Cross-group: requisição toca mais de um grupo, então não dá pra confiar só no consenso
	// de um grupo pra ordená-la. Em vez disso: (1) pede um número de sequência global (GSN) ao
	// sequenciador — é isso que garante uma ordem única entre operações de grupos diferentes;
	// (2) publica um META avisando todos os grupos envolvidos qual GSN foi atribuído; (3) manda
	// uma cópia da requisição pra cada grupo tocado. Cada grupo roda seu próprio MultiPaxos
	// independente, e o GSN + META é o que permite depois reconstruir a ordem relativa correta
	// entre eles sem precisar de uma única instância de consenso global.
	gsn := o.seq.GetNextGSN()
	if gsn == 0 { return true }
	o.seq.PublishMETA(gsn, req.TouchedGroups)
	for _, groupID := range req.TouchedGroups {
		o.sendToGroup(&pb.ClientRequest{
			RequestId: req.RequestId, Payload: req.Payload, Signature: req.Signature,
			Pubkey: req.Pubkey, GroupId: groupID, TouchedGroups: req.TouchedGroups, GSN: gsn,
		}, groupID)
	}
	return true
}
