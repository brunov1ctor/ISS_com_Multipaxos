package main

import (
    "fmt"
    "os"
    "sync"
    "time"

    "github.com/rs/zerolog"
    logger "github.com/rs/zerolog/log"

    "github.com/hyperledger-labs/mirbft/checkpoint"
    "github.com/hyperledger-labs/mirbft/config"
    "github.com/hyperledger-labs/mirbft/crypto"
    "github.com/hyperledger-labs/mirbft/discovery"
    "github.com/hyperledger-labs/mirbft/manager"
    mirlog "github.com/hyperledger-labs/mirbft/log"
    "github.com/hyperledger-labs/mirbft/membership"
    "github.com/hyperledger-labs/mirbft/messenger"
    "github.com/hyperledger-labs/mirbft/orderer"
    "github.com/hyperledger-labs/mirbft/profiling"
    "github.com/hyperledger-labs/mirbft/request"
    "github.com/hyperledger-labs/mirbft/statetransfer"
    "github.com/hyperledger-labs/mirbft/tracing"
)

// profilingEnabled is a global variable indicating whether profiling is enabled.
// Used to decide whether the tracer should shut down the process on the INT signal or not.
var profilingEnabled = false

// local interface to avoid importing manager internals
type entryHandlerRegistrar interface {
	RegisterEntryHandler(func(*mirlog.Entry))
}

func main() {

	exe, _ := os.Executable()
	fmt.Printf("[BIN] orderingpeer exe=%s\n", exe)
	fmt.Printf("[ARGS] len=%d args=%v\n", len(os.Args), os.Args)

	if len(os.Args) < 5 {
		fmt.Fprintf(os.Stderr,
			"[ARGS] ERROR: expected at least 5 arguments: <configFile> <discoveryAddr> <ownPublicIP> <ownPrivateIP> [traceFile] [profDir], got %d\n",
			len(os.Args))
		os.Exit(1)
	}

	// Get command line arguments
	configFileName := os.Args[1]
	discoveryServAddr := os.Args[2]
	ownPublicIP := os.Args[3]
	ownPrivateIP := os.Args[4]

	var traceFileName string
	var profDir string

	if len(os.Args) > 5 {
		traceFileName = os.Args[5]
	}
	if len(os.Args) > 6 {
		profDir = os.Args[6]
	}

	fmt.Printf("[ARGS] config=%s discovery=%s ownPub=%s ownPriv=%s traceFile=%s profDir=%s\n",
		configFileName, discoveryServAddr, ownPublicIP, ownPrivateIP, traceFileName, profDir)

	config.LoadFile(configFileName)

	// Configure logger
	zerolog.SetGlobalLevel(config.Config.LoggingLevel)
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnixMicro
	logger.Logger = logger.Output(zerolog.ConsoleWriter{
		Out:        os.Stdout,
		NoColor:    true,
		TimeFormat: "15:04:05.000",
	})

	logger.Info().
		Str("configFile", configFileName).
		Str("discoveryAddr", discoveryServAddr).
		Str("ownPublicIP", ownPublicIP).
		Str("ownPrivateIP", ownPrivateIP).
		Str("traceFileArg", traceFileName).
		Str("profDirArg", profDir).
		Msg("orderingpeer starting with arguments.")

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
		Int("numPeers", len(nodeIdentities)).
		Msg("Registered with discovery server.")

	// Deserialize TBLS keys
	TBLSPubKey, err := crypto.TBLSPubKeyFromBytes(serializedTBLSPubKey)
	if err != nil {
		logger.Fatal().Msgf("Could not deserialize TBLS public key %s", err.Error())
	}
	TBLSPrivKeyShare, err := crypto.TBLSPrivKeyShareFromBytes(serializedTBLSPrivKeyShare)
	if err != nil {
		logger.Fatal().Msgf("Could not deserialize TBLS private key share %s", err.Error())
	}

	membership.TBLSPublicKey = TBLSPubKey
	membership.TBLSPrivateKeyShare = TBLSPrivKeyShare

	logger.Info().Msg("Deserialized TBLS keys.")

	// Initialize channels and connect modules through them.
	// Event dispatcher channel between different modules.
	eventsInChan := make(chan *mirlog.EventList)
	eventsOutChan := make(chan *mirlog.EventList)
	membership.ManagerEventsOut = eventsInChan
	membership.ManagerEventsIn = eventsOutChan
	request.ManagerEventsOut = eventsInChan
	request.ManagerEventsIn = eventsOutChan
	statetransfer.ManagerEventsOut = eventsInChan
	statetransfer.ManagerEventsIn = eventsOutChan

	// Initialize the messenger
	messenger.Init()
	messenger.MainEventLog = mirlog.NewLog(eventsInChan)
	membership.PeerConnections = messenger.PeerConnections
	request.Messenger = messenger.DefaultMessenger
	statetransfer.PeerCommunication = messenger.PeerCommunication
	logger.Info().Msg("Initialized messenger.")

	// Profiling (arg index 6) – influences tracing exit behaviour.
	if profDir != "" {
		profilingEnabled = true
		logger.Info().Str("profDir", profDir).Msg("Profiling enabled.")
		setUpProfiling(profDir)
	} else {
		logger.Info().Msg("Profiling disabled (no profDir argument).")
	}

	// Set up tracing (arg index 5)
	if traceFileName != "" {
		logger.Info().Str("traceFile", traceFileName).Msg("Setting up tracing.")
		ensureTraceDir(traceFileName)
		setUpTracing(traceFileName, ownID)
	} else {
		logger.Warn().Msg("No trace file argument provided. Tracing will be disabled and no .trc file will be written.")
	}

	// Declare variables for component modules.
	var mngr manager.Manager
	var ord orderer.Orderer
	var chkp checkpoint.Checkpointer
	var rsp *request.Responder

	// Instantiate component modules (with stubs).
	mngr = setManager(config.Config.ManagerType)
	chkp = setCheckpointer(config.Config.ManagerType)
	rsp = request.NewResponder()

	// Connect modules through their event handlers.
	// Using type alias in order to use the same function for both Manager and Checkpointer below.
	// Both implement the entryHandlerRegistrar interface defined above.
	var entryHandlerReg entryHandlerRegistrar

	rspRespHandler := rsp.RespHandler
	if config.Config.ManagerType == "Dummy" {
		rspRespHandler = func(request.IssClientID, []request.IssSeqNr, [][]byte) {}
	}

	// Create orderer and connect it to the other modules.
	entryHandlerReg = mngr
	ord = config.InitializeOrderer()
	ord.Connect(
		mirlog.NewRouter(entryHandlerReg.RegisterEntryHandler),
		rspRespHandler,
		statetransfer.NewStateReceiver(),
	)
	logger.Info().Str("orderer", config.Config.OrdererType).Msg("Initialized orderer.")

	// Connect manager to modules
	entryHandlerReg = chkp
	mngr.Connect(
		ord,
		rsp,
		mirlog.NewRouter(entryHandlerReg.RegisterEntryHandler),
		messenger.LocalClientConnection,
	)

	// Connect checkpointer to modules.
	chkp.Connect(
		ord,
		messenger.PeerConnections,
	)

	// Connect modules to state receiver.
	statetransfer.OrdererConfiguration = ord.Configuration
	statetransfer.StateReceiver = chkp

	// Register handlers for messages to this node.
	messenger.PeerMsgHandler = mngr.HandleMessage
	messenger.ClientMsgHandler = rsp.HandleRequest
	messenger.SuspicionTracer = ord.Tracer()

	// Register handler for events to be executed.
	mngr.SetEventHandler(eventsOutChan, mngr)
	messenger.StateTransferMsgHandler = statetransfer.HandleMessage
	statetransfer.OrdererEntryHandler = ord.HandleEntry

	// Create wait group for all the modules that will run as separate goroutines.
	wg := sync.WaitGroup{}
	wg.Add(5) // messenger, checkpointer, orderer, manager, responder

	// Start the messaging subsystem.
	go messenger.Start(&wg)
	messenger.Connect()
	logger.Info().Msg("Connected to all peers.")

	// Synchronize with master again to make sure that all peers finished connecting.
	discovery.SyncPeer(discoveryServAddr, ownID)
	logger.Info().Msg("All peers finished connecting. Starting ISS.")

	// If we are simulating a crashed node, exit immediately.
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

	// Start modules
	go mngr.Run(&wg)
	go chkp.Run(&wg)
	go rsp.Run(&wg)
	go ord.Run(&wg)

	// Handle OS signals to allow a graceful shutdown.
	handleSignals()

	// Wait forever (graceful termination not implemented, exit on signal).
	wg.Wait()
}

func ensureTraceDir(traceFile string) {
	dir := filepath.Dir(traceFile)
	if dir == "." || dir == "" {
		logger.Warn().Str("traceFile", traceFile).Msg("Trace file has no directory component.")
		return
	}
	err := os.MkdirAll(dir, 0o755)
	if err != nil {
		logger.Error().Str("dir", dir).Err(err).Msg("Failed to create trace directory.")
	} else {
		logger.Info().Str("dir", dir).Msg("Ensured trace directory exists.")
	}
}

// handleSignals installs a handler for SIGINT and SIGTERM, just for logging.
func handleSignals() {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		for sig := range sigChan {
			logger.Info().Str("signal", sig.String()).Msg("Received OS signal in orderingpeer.")
		}
	}()
}

func setUpProfiling(profDir string) {
	err := profiling.StartCPUTracing(profDir)
	if err != nil {
		logger.Error().Err(err).Msg("Failed to start CPU profiling.")
	} else {
		logger.Info().Str("profDir", profDir).Msg("CPU profiling started.")
	}
}

func setUpTracing(outFileName string, ownID int32) {
	logger.Info().Str("traceFile", outFileName).Int32("ownID", ownID).Msg("Initializing tracing.")
	tracing.MainTrace.Start(outFileName, ownID)
	tracing.MainTrace.StopOnSignal(os.Interrupt, !profilingEnabled)
	logger.Info().Str("traceFile", outFileName).Msg("Started tracing (StopOnSignal registered).")
}

func setManager(managerType string) (mngr manager.Manager) {
	switch managerType {
	case "Dummy":
		mngr = manager.NewDummyManager()
	case "Mir":
		mngr = manager.NewMirManager()
	default:
		logger.Fatal().Msg("Unsupported manager type")
	}
	return mngr
}

func setCheckpointer(managerType string) (chkp checkpoint.Checkpointer) {
	switch managerType {
	case "Simple":
		chkp = checkpoint.NewSimpleCheckpointer()
	case "Signing":
		chkp = checkpoint.NewSigningCheckpointer()
	default:
		logger.Fatal().Msg("Unsupported manager type")
	}
	return chkp
}

