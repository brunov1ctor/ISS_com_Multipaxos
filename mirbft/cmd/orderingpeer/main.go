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

func extractPort(addr string) string {
	if idx := strings.LastIndex(addr, ":"); idx != -1 && idx < len(addr)-1 {
		return addr[idx+1:]
	}
	return addr
}

func main() {
	// Configura logger "bonitinho" no stdout.
	zerolog.SetGlobalLevel(zerolog.InfoLevel)
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnixMicro
	logger.Logger = logger.Output(zerolog.ConsoleWriter{
		Out:        os.Stdout,
		TimeFormat: "15:04:05.000",
	})

	if len(os.Args) < 2 {
		logger.Fatal().
			Int("argsGiven", len(os.Args)-1).
			Msg("usage: discoverymaster port [commands...] OR: discoverymaster master addr:port master-commands.cmd")
	}

	args := os.Args[1:]

	var (
		port string
		cmds []string
	)

	// Novo modo: "discoverymaster master 0.0.0.0:9999 master-commands.cmd"
	if len(args) >= 3 && args[0] == "master" {
		port = extractPort(args[1])
		// Internamente, convertemos para o modo antigo:
		// discoverymaster <port> file <commandsFile>
		cmds = []string{"file", args[2]}

		logger.Info().
			Str("mode", "master").
			Str("listenAddr", args[1]).
			Str("port", port).
			Str("commandsFile", args[2]).
			Msg("Starting discoverymaster in MASTER mode (file-based commands).")

	} else {
		// Modo legado: discoverymaster <port> [tokens...]
		port = args[0]
		if len(args) > 1 {
			cmds = args[1:]
		} else {
			cmds = nil
		}

		logger.Info().
			Str("mode", "legacy").
			Str("port", port).
			Msg("Starting discoverymaster in LEGACY mode.")
	}

	// WaitGroups para servidor gRPC e processor de comandos.
	var srvWg sync.WaitGroup
	srvWg.Add(1)
	var cmdWg sync.WaitGroup
	cmdWg.Add(1)

	// Sobe servidor gRPC de discovery.
	grpcServer := grpc.NewServer()
	masterSrv := discovery.NewDiscoveryServer()
	go discovery.RunDiscoveryServer(port, grpcServer, masterSrv, &srvWg)

	logger.Info().Str("port", port).Msg("Discovery gRPC server started.")

	// Inicia goroutine que processa comandos recebidos (via tokenChannel).
	tokenChan := masterSrv.ProcessCommands(&cmdWg)

	// Alimenta o tokenChan com os comandos vindos da linha de comando.
	for i := 0; i < len(cmds); i++ {
		switch cmds[i] {
		case "-":
			logger.Info().Msg("Reading commands from stdin.")
			readCommandsFromStdin(tokenChan)

		case "file":
			// Esperamos mais um argumento com o nome do arquivo.
			if i+1 >= len(cmds) {
				logger.Error().Msg("Token 'file' sem nome de arquivo; ignorando.")
				continue
			}
			fileName := cmds[i+1]
			logger.Info().Str("file", fileName).Msg("Processing command file.")
			processCommandFile(fileName, tokenChan)
			i++ // pula o nome do arquivo

		default:
			// Qualquer outro token é passado diretamente para o parser.
			tokenChan <- cmds[i]
		}
	}

	// Não vamos mais enviar comandos.
	close(tokenChan)

	// Espera o processamento de todos os comandos terminar.
	cmdWg.Wait()

	// Encerra o servidor gRPC e espera finalizar.
	grpcServer.Stop()
	srvWg.Wait()

	logger.Info().Msg("discoverymaster terminated cleanly.")
}

// processCommandFile lê um arquivo linha a linha e, para cada linha,
// chama discovery.ParseCommandStr(cmdStr, tokenChannel).
func processCommandFile(fileName string, tokenChannel chan string) {
	var (
		file *os.File
		err  error
	)

	if file, err = os.Open(fileName); err != nil {
		logger.Error().
			Err(err).
			Str("fileName", fileName).
			Msg("Couldn't open command file. Ignoring.")
		return
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Split(bufio.ScanLines)

	for scanner.Scan() {
		cmdStr := scanner.Text()
		// ParseCommandStr já ignora linhas vazias e comentários.
		discovery.ParseCommandStr(cmdStr, tokenChannel)
	}

	if err := scanner.Err(); err != nil {
		logger.Error().
			Err(err).
			Str("fileName", fileName).
			Msg("Error while scanning command file.")
	}

	logger.Info().Str("fileName", fileName).Msg("Finished reading commands from file.")
}

// readCommandsFromStdin faz o mesmo que processCommandFile, só que lendo do stdin.
func readCommandsFromStdin(tokenChannel chan string) {
	logger.Info().Msg("Reading master commands from stdin.")

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Split(bufio.ScanLines)

	for scanner.Scan() {
		cmdStr := scanner.Text()
		discovery.ParseCommandStr(cmdStr, tokenChannel)
	}

	if err := scanner.Err(); err != nil {
		logger.Error().Err(err).Msg("Error while scanning input from stdin.")
	}

	logger.Info().Msg("Finished reading commands from stdin.")
}
