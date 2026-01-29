package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	logger "github.com/rs/zerolog/log"

	"github.com/hyperledger-labs/mirbft/checkpoint"
	"github.com/hyperledger-labs/mirbft/config"
	"github.com/hyperledger-labs/mirbft/crypto"
	"github.com/hyperledger-labs/mirbft/discovery"
	"github.com/hyperledger-labs/mirbft/manager"
	"github.com/hyperledger-labs/mirbft/membership"
	"github.com/hyperledger-labs/mirbft/messenger"
	"github.com/hyperledger-labs/mirbft/orderer"
	"github.com/hyperledger-labs/mirbft/profiling"
	"github.com/hyperledger-labs/mirbft/request"
	"github.com/hyperledger-labs/mirbft/statetransfer"
	"github.com/hyperledger-labs/mirbft/tracing"
)

// Flag indicating whether profiling is enabled.
// Used to decide whether the tracer should shut down the process on the INT signal or not.
var profilingEnabled = false

func main() {
	// Sanidade básica de argumentos
	if len(os.Args) < 5 {
		logger.Fatal().
			Str("argv", strings.Join(os.Args, " ")).
			Msg("usage: orderingpeer <configFile> <discoveryAddr> <ownPublicIP> <ownPrivateIP> [traceFile] [profilePrefix]")
	}

	configFileName := os.Args[1]
	discoveryServAddr := os.Args[2]
	ownPublicIP := os.Args[3]
	ownPrivateIP := os.Args[4]

	// Carrega config antes de configurar logger
	config.LoadFile(configFileName)

	// Configure logger
	zerolog.SetGlobalLevel(config.Config.LoggingLevel)
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnixMicro
	logger.Logger = logger.Output(zerolog.ConsoleWriter{
		Out:        os.Stdout,
		NoColor:    true,
		TimeFormat: "15:04:05.000",
	})

	wd, _ := os.Getwd()
	logger.Info().
		Str("argv", strings.Join(os.Args, " ")).
		Str("cwd", wd).
		Str("configFile", configFileName).
		Str("discoveryAddr", discoveryServAddr).
		Str("ownPublicIP", ownPublicIP).
		Str("ownPrivateIP", ownPrivateIP).
		Msg("orderingpeer starting")

	// Initialize packages that need the configuration to be loaded for initialization
	membership.Init()
	request.Init()
	tracing.Init()
	statetransfer.Init()

	// Register with the discovery service and obtain:
	// - Own ID
	// - Identities of all other peers
	// - Private key
	// - Public key for BLS threshold cryptosystem
	// - Private key share for BLS threshold cryptosystem
	ownID, nodeIdentities, privateKey, serializedTBLSPubKey, serializedTBLSPrivKeyShare :=
		discovery.RegisterPeer(discoveryServAddr, ownPublicIP, ownPrivateIP)
	membership.OwnID = ownID
	membership.OwnPrivKey = privateKey
	membership.InitNodeIdentities(nodeIdentities)
	
	logger.Info().
		Int32("ownID", ownID).
		Str("ownPublicIP", ownPublicIP).
		Str("ownPrivateIP", ownPrivateIP).
		Int("numPeers", len(nodeIdentities)).
		Msg("Registered with discovery server.")

	// Deserialize TBLS keys
	TBLSPubKey, err := crypto.TBLSPubKeyFromBytes(serializedTBLSPubKey)
	if err != nil {
		logger.Fatal().Msgf("Could not deserialize TBLS public key %s", err.Error())
	}
	membership.TBLSPublicKey = TBLSPubKey
	TBLSPrivKeyShare, err := crypto.TBLSPrivKeyShareFromBytes(serializedTBLSPrivKeyShare)
	if err != nil {
		logger.Fatal().Msgf("Could not deserialize TBLS private key share %s", err.Error())
	}
	membership.TBLSPrivKeyShare = TBLSPrivKeyShare

	// Start profiler if necessary
	// ATTENTION! We first look for argument 6, and only then check argument 5
	//            (as the presence of profiling influences setting up of tracing).
	if len(os.Args) > 6 {
		profilingEnabled = true // UGLY DIRTY CODE!
		logger.Info().
			Str("profilePrefix", os.Args[6]).
			Msg("Profiling enabled via CLI argument.")
		setUpProfiling(os.Args[6])
	}

	// Set up tracing if necessary
	if len(os.Args) > 5 {
		logger.Info().
			Str("traceFile", os.Args[5]).
			Msg("Tracing enabled via CLI argument.")
		setUpTracing(os.Args[5], ownID)
	} else {
		logger.Warn().
			Msg("No traceFile argument provided; throughput/latency metrics will NOT be available.")
	}

	logger.Info().
		Str("ordererType", config.Config.Orderer).
		Str("managerType", config.Config.Manager).
		Str("checkpointerType", config.Config.Checkpointer).
		Msg("Component types from configuration.")

	// Declare variables for component modules.
	var mngr manager.Manager
	var ord orderer.Orderer
	var chkp checkpoint.Checkpointer
	var rsp *request.Responder

	// Instantiate component modules (with stubs).
	mngr = setManager(config.Config.Manager)

	ord = setOrderer(config.Config.Orderer, ownPrivateIP, configFileName)

	chkp = setCheckpointer(config.Config.Checkpointer)
	rsp = request.NewResponder()
	
	// CSMR: Conecta Replica Mapper e Output Processing
	if mcOrd, ok := ord.(*orderer.MultiPaxosMulticastOrderer); ok {
		rsp.SetIsMemberFunc(mcOrd.IsMember)
		rsp.SetGroupMembersFunc(mcOrd.GetGroupMembers)
		request.SetProxyInterceptor(func(req *request.Request) {
			if req.Msg != nil {
				mcOrd.ProxyInterceptor(req.Msg.GSN, req.Msg.TouchedGroups, req.Msg)
			}
		})
		request.SetGSNBarrierChecker(mcOrd.CanProcessCrossOp)
		request.SetGSNGenerator(mcOrd.GetNextGSN)
		request.SetGroupMembersGetter(mcOrd.GetGroupMembers)
		// ✅ CORREÇÃO: Configura META publisher para evitar duplicação
		request.SetMETAPublisher(mcOrd.PublishMETAOnce)
		// ✅ CRÍTICO: Registra PreprocessRequest para processar requisições de clientes
		request.SetRequestPreprocessor(mcOrd.PreprocessRequest)
	}

	// Initialize modules.
	// No outgoing messages must be produced even after initialization,
	// but the modules must be ready to process incoming messages.
	ord.Init(mngr)
	chkp.Init(mngr)

	// Register message and entry handlers
	fmt.Printf("[MAIN] Registering ClientRequestHandler = request.HandleRequest\n")
	messenger.CheckpointMsgHandler = chkp.HandleMessage
	messenger.OrdererMsgHandler = ord.HandleMessage
	messenger.ClientRequestHandler = request.HandleRequest
	fmt.Printf("[MAIN] ClientRequestHandler registered: %v\n", messenger.ClientRequestHandler != nil)
	messenger.StateTransferMsgHandler = statetransfer.HandleMessage
	statetransfer.OrdererEntryHandler = ord.HandleEntry

	// Create wait group for all the modules that will run as separate goroutines.
	wg := sync.WaitGroup{}
	wg.Add(5) // messenger, checkpointer, orderer, manager, responder

	// Start the messaging subsystem.
	messenger.InitRequestWorkerPool()
	go messenger.Start(&wg)
	messenger.Connect()
	logger.Info().Msg("Connected to all peers.")

	// Synchronize with master again to make sure that all peers finished connecting.
	discovery.SyncPeer(discoveryServAddr, ownID)
	logger.Info().Msg("All peers finished connecting. Starting ISS.")

	// Simulação de falha (mantida do código original, se estiver ativada)
	if config.Config.LeaderPolicy == "SimulatedRandomFailures" {
		crash := true
		for _, l := range manager.NewLeaderPolicy(config.Config.LeaderPolicy).GetLeaders(0) {
			if l == membership.OwnID {
				crash = false
			}
		}
		if crash {
			logger.Info().Msg("Simulating crashed peer. Exiting.")
			return
		}
	}

	// Start all modules.
	go rsp.Start(&wg)
	go chkp.Start(&wg)
	go mngr.Start(&wg)
	go ord.Start(&wg)

	logger.Info().Msg("All modules started. Waiting for termination (SIGINT or process exit).")

	// Wait for all modules to finish.
	wg.Wait()

	logger.Info().Msg("orderingpeer terminated cleanly.")
}

// Enables and starts the profiler of used resources.
func setUpProfiling(outFilePrefix string) {
	logger.Info().
		Str("outFilePrefix", outFilePrefix).
		Msg("Setting up profiling.")

	profiling.StartProfiler("cpu", outFilePrefix+".cpu", 1)
	profiling.StartProfiler("block", outFilePrefix+".block", 1)
	profiling.StartProfiler("mutex", outFilePrefix+".mutex", 1)

	// Stop profiler on INT signal
	profiling.StopOnSignal(os.Interrupt, true)
}

// Sets up tracing of events.
func setUpTracing(outFileName string, ownID int32) {
	logger.Info().
		Str("traceFile", outFileName).
		Int32("ownID", ownID).
		Msg("Setting up tracing.")

	// Initialize tracing with output file name given at command line
	tracing.MainTrace.Start(outFileName, ownID)

	// TODO: Move the CPU tracing to a more appropriate place
	profiling.StartCPUTracing(tracing.MainTrace, 500*time.Millisecond)

	// Stop tracing on INT signal.
	tracing.MainTrace.StopOnSignal(os.Interrupt, !profilingEnabled)

	logger.Info().
		Str("traceFile", outFileName).
		Msg("Tracing started.")
}

func setManager(managerType string) (mngr manager.Manager) {
	switch managerType {
	case "Dummy":
		mngr = manager.NewDummyManager()
	case "Mir":
		mngr = manager.NewMirManager()
	default:
		logger.Fatal().Str("managerType", managerType).Msg("Unsupported manager type")
	}
	return mngr
}

func setOrderer(ordererType string, ownPrivateIP string, configFileName string) (ord orderer.Orderer) {
	switch ordererType {
	case "Dummy":
		ord = &orderer.DummyOrderer{}
	case "Pbft":
		ord = &orderer.PbftOrderer{}
	case "HotStuff":
		ord = &orderer.HotStuffOrderer{}
	case "Raft":
		ord = &orderer.RaftOrderer{}
	case "MultiPaxos":
		ord = &orderer.MultiPaxosOrderer{}
	case "MultiPaxosMulticast":
		ord = &orderer.MultiPaxosMulticastOrderer{}
	default:
		logger.Fatal().Str("ordererType", ordererType).Msg("Unsupported orderer type")
	}
	return ord
}

func setCheckpointer(checkpointerType string) (chkp checkpoint.Checkpointer) {
	switch checkpointerType {
	case "Dummy":
		chkp = checkpoint.NewDummyCheckpointer()
	case "Simple":
		chkp = checkpoint.NewSimpleCheckpointer()
	case "Signing":
		chkp = checkpoint.NewSigningCheckpointer()
	default:
		logger.Fatal().Str("checkpointerType", checkpointerType).Msg("Unsupported checkpointer type")
	}
	return chkp
}

