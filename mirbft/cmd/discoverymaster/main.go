package main

import (
	"bufio"
	"os"
	"strings"
	"sync"

	"github.com/rs/zerolog"
	logger "github.com/rs/zerolog/log"
	"github.com/hyperledger-labs/mirbft/discovery"
	"google.golang.org/grpc"
)

func main() {

	// Configura logger com horário legível no master-log.log
	zerolog.SetGlobalLevel(zerolog.DebugLevel)
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnixMicro
	logger.Logger = logger.Output(zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: "15:04:05.000"})

	if len(os.Args) < 2 {
		logger.Fatal().Msg("Uso: discoverymaster <port> [comandos...]  OU  discoverymaster master <addr> <commands_file>")
	}

	// Cria wait groups
	var srvWg sync.WaitGroup
	srvWg.Add(1)
	var cmdWg sync.WaitGroup
	cmdWg.Add(1)

	// Decide modo de operação:
	//  1) Modo novo: discoverymaster master 0.0.0.0:9999 master-commands.cmd
	//  2) Modo antigo: discoverymaster 9999 file master-commands.cmd
	args := os.Args[1:]

	var port string
	var cmds []string
	var commandsFile string
	newMode := false

	if len(args) >= 3 && args[0] == "master" {
		// Modo novo:
		//   args[0] = "master"
		//   args[1] = "0.0.0.0:9999" (ou semelhante)
		//   args[2] = "master-commands.cmd"
		newMode = true

		addr := args[1]
		// Extrai apenas a parte da porta para RunDiscoveryServer,
		// que espera algo como "9999" e internamente faz net.Listen(":"+port).
		port = extractPort(addr)
		commandsFile = args[2]

		logger.Info().
			Bool("newMode", true).
			Str("addrArg", addr).
			Str("port", port).
			Str("commandsFile", commandsFile).
			Msg("Starting discoverymaster in NEW mode (master <addr> <commands_file>).")
	} else {
		// Modo antigo (compatível com ISS original):
		//   discoverymaster <port> file master-commands.cmd
		port = args[0]
		cmds = args[1:]

		logger.Info().
			Bool("newMode", false).
			Str("port", port).
			Strs("cmdTokens", cmds).
			Msg("Starting discoverymaster in LEGACY mode (<port> [tokens...]).")
	}

	// Inicia servidor gRPC com discovery master
	grpcServer := grpc.NewServer()
	masterSrv := discovery.NewDiscoveryServer()
	go discovery.RunDiscoveryServer(port, grpcServer, masterSrv, &srvWg)

	// Processador de comandos: alimenta masterSrv.ProcessCommands com tokens
	tokenChan := masterSrv.ProcessCommands(&cmdWg)

	if newMode {
		// Lê todos os comandos do arquivo master-commands.cmd
		logger.Info().Str("file", commandsFile).Msg("Reading master commands from file (new mode).")
		processCommandFile(commandsFile, tokenChan)
	} else {
		// Comportamento antigo: interpreta tokens da linha de comando
		for i := 0; i < len(cmds); i++ {
			switch cmds[i] {
			case "-":
				readCommandsFromStdin(tokenChan)
			case "file":
				if i+1 >= len(cmds) {
					logger.Fatal().Msg("Missing command file name after 'file'")
				}
				processCommandFile(cmds[i+1], tokenChan)
				i++
			default:
				tokenChan <- cmds[i]
			}
		}
	}

	// Encerra processador de comandos e espera terminar
	close(tokenChan)
	cmdWg.Wait()

	// Para o servidor gRPC e espera terminar
	grpcServer.Stop()
	srvWg.Wait()

	logger.Info().Msg("discoverymaster finalizado.")
}

// Lê comandos de um arquivo de texto (uma linha por comando),
// repassa para o channel de tokens. Igual ao código original.
func processCommandFile(fileName string, tokenChan chan<- string) {

	logger.Info().Str("file", fileName).Msg("Processing command file.")

	file, err := os.Open(fileName)
	if err != nil {
		logger.Fatal().Err(err).Str("file", fileName).Msg("Could not open command file.")
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		// Ignora linhas em branco e comentários
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Cada linha já é um comando completo consumido por ProcessCommands
		tokenChan <- line
	}

	if err := scanner.Err(); err != nil {
		logger.Fatal().Err(err).Str("file", fileName).Msg("Error reading command file.")
	}
}

// Lê comandos da stdin (modo antigo)
func readCommandsFromStdin(tokenChan chan<- string) {
	logger.Info().Msg("Reading commands from stdin ('-' token).")
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		tokenChan <- scanner.Text()
	}
	if err := scanner.Err(); err != nil {
		logger.Fatal().Err(err).Msg("Error reading commands from stdin.")
	}
}

// Extrai apenas a parte da porta de uma string tipo "0.0.0.0:9999" ou ":9999".
func extractPort(addr string) string {
	// Se não tiver ":", assume que já é só a porta
	if !strings.Contains(addr, ":") {
		return addr
	}
	parts := strings.Split(addr, ":")
	// Pega o último elemento (depois do último ':')
	return parts[len(parts)-1]
}

