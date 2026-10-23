package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/rs/zerolog"
	logger "github.com/rs/zerolog/log"
	"github.com/hyperledger-labs/mirbft/config"
	"github.com/hyperledger-labs/mirbft/membership"
	"github.com/hyperledger-labs/mirbft/profiling"
	"github.com/hyperledger-labs/mirbft/request"
)

var (
	// Prefix of the client-specific output files, to which the client ID will be appended.
	outFilePrefix string

	// Used to initialize membership only once.
	membershipInitializer sync.Once
)

// apiVerbs são os comandos despachados direto como "orderingclient <verbo> <config> <addr>
// [args...]" (ver main() e apimode.go) -- mesma lista que o switch de runAPIMode reconhece.
var apiVerbs = map[string]bool{
	"ls": true, "put": true, "get": true, "delete": true, "transfer": true, "multicast": true, "overload": true, "repl": true,
}

// resolveConfigAddr acha config.yml e o endereço do discovery SEM exigir que venham na linha
// de comando, em duas fontes (nessa ordem):
//  1. Variáveis de ambiente ORDERINGCLIENT_CONFIG/ORDERINGCLIENT_ADDR -- pra quem exportou uma
//     vez por sessão de terminal.
//  2. Arquivos de caminho fixo que o deploy já escreve em TODO nó que participou dele:
//     $ISS_BASE_DIR/config/config.yml (mesmo caminho que SLAVE_CONFIG_FILE em
//     generate-master-commands.py) e .../config/master-addr (escrito por writeMasterAddrFile,
//     mesmo script). Então rodando de dentro de um nó já deployado (ex. node-0), os dois já
//     estão lá sozinhos.
//
// ok=false se nenhuma das duas fontes tiver os dois valores completos -- o chamador volta a
// exigir <config> <addr> na linha de comando, como sempre foi.
func resolveConfigAddr() (configFile, addr string, ok bool) {
	if c := os.Getenv("ORDERINGCLIENT_CONFIG"); c != "" {
		if a := os.Getenv("ORDERINGCLIENT_ADDR"); a != "" {
			return c, a, true
		}
	}
	base := os.Getenv("ISS_BASE_DIR")
	if base == "" {
		user := os.Getenv("USER")
		if user == "" {
			user = "user"
		}
		base = fmt.Sprintf("/users/%s/iss", user)
	}
	configFile = filepath.Join(base, "config", "config.yml")
	if st, err := os.Stat(configFile); err != nil || st.Size() == 0 {
		return "", "", false
	}
	addrBytes, err := os.ReadFile(filepath.Join(base, "config", "master-addr"))
	if err != nil {
		return "", "", false
	}
	addr = strings.TrimSpace(string(addrBytes))
	if addr == "" {
		return "", "", false
	}
	// config.yml tem paths relativos (tls-data/ca.pem etc.) resolvidos contra o CWD do
	// processo, não contra ISS_BASE_DIR -- sem isso, resolver config/addr sozinho não bastava,
	// ainda exigia "cd $ISS_BASE_DIR" manual antes de chamar orderingclient. So faz sentido
	// mudar pra "base" aqui (fonte ISS_BASE_DIR): quem usou ORDERINGCLIENT_CONFIG/_ADDR acima
	// já escolheu os caminhos à mão e pode estar rodando de fora do Emulab, onde não existe
	// essa convenção de diretório.
	if err := os.Chdir(base); err != nil {
		logger.Warn().Err(err).Str("dir", base).Msg("Could not chdir into ISS_BASE_DIR; relative paths (tls-data/...) may fail.")
	}
	return configFile, addr, true
}

// CRASH MODEL (CSMR): Aceita primeira resposta (não BFT)
func enoughResponses(n int) bool {
	return n >= 1
}

func main() {
	// "help"/"-h"/"--help" como primeiro argumento -- nenhum uso existente (benchmark ou api)
	// passa essas palavras como caminho de config, então é seguro interceptar aqui, antes de
	// tudo, sem mudar nenhum comportamento de quem já chama com argumentos de verdade.
	if len(os.Args) > 1 && (os.Args[1] == "help" || os.Args[1] == "-h" || os.Args[1] == "--help") {
		printTopLevelHelp()
		return
	}

	// "orderingclient <comando> [config] [addr] [args...]" -- cada verbo da API despachado
	// DIRETO, sem precisar da palavra "api" na frente (o jeito antigo, "api <config> <addr>
	// <comando> ...", continua funcionando logo abaixo, só não é mais o jeito documentado --
	// mantido por compatibilidade, zero custo). Seguro porque nenhum uso de benchmark passa um
	// desses nomes como primeiro argumento (é sempre um caminho de arquivo de config).
	//
	// config/addr são OPCIONAIS: se resolveConfigAddr() achar os dois sozinho (env vars, ou
	// rodando num nó já deployado), TODO o resto de os.Args vira argumento do comando -- não
	// precisa digitar config.yml/endereço nenhuma vez. Só exige na mão quando nenhuma das
	// fontes automáticas está disponível (ex. rodando de fora do Emulab, numa máquina que
	// nunca participou de um deploy).
	if len(os.Args) > 1 && apiVerbs[os.Args[1]] {
		verb := os.Args[1]
		rest := os.Args[2:]
		if cfg, addr, ok := resolveConfigAddr(); ok {
			runAPIMode(append([]string{cfg, addr, verb}, rest...))
			return
		}
		if len(rest) < 2 {
			fmt.Printf("uso: orderingclient %s <config_file> <discovery_addr:port> [args...]\n", verb)
			fmt.Println("(ou defina ORDERINGCLIENT_CONFIG/ORDERINGCLIENT_ADDR, ou rode num nó já deployado -- aí não precisa digitar nenhum dos dois)")
			os.Exit(1)
		}
		runAPIMode(append([]string{rest[0], rest[1], verb}, rest[2:]...))
		return
	}

	// Forma antiga, "api <config> <addr> <comando> ...", mantida funcionando (alguns scripts
	// ou hábito de quem já usava antes) -- equivalente a despachar pelo verbo direto acima.
	if len(os.Args) > 1 && os.Args[1] == "api" {
		runAPIMode(os.Args[2:])
		return
	}

	// Check number of parameters
	if len(os.Args) <= 2 {
		if len(os.Args) <= 1 {
			// "orderingclient" sozinho, sem nenhum argumento: se config/addr resolverem
			// sozinhos (nó já deployado, ou env vars), entra direto no repl -- mais útil do
			// que só mostrar a ajuda, já que dá pra conectar sem digitar nada. Sem isso, cai
			// na ajuda como sempre foi.
			if cfg, addr, ok := resolveConfigAddr(); ok {
				runREPL(cfg, addr)
				return
			}
			printTopLevelHelp()
			return
		}
		logger.Fatal().
			Int("argsGiven", len(os.Args)-1).
			Msg("usage: orderingclient config_file_name discovery_server_addr:port out_file_prefix [profiler_output_prefix] -- ou 'orderingclient help'")
	}

	// Get config file name
	configFileName := os.Args[1]
	config.LoadFile(configFileName)

	// Configure logger.
	zerolog.SetGlobalLevel(config.Config.LoggingLevel)
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnixMicro
	logger.Logger = logger.Output(zerolog.ConsoleWriter{
		Out:        os.Stdout,
		NoColor:    true,
		TimeFormat: "15:04:05.000"})
	//zerolog.TimeFieldFormat = zerolog.TimeFormatUnixMs
	//logger.Logger = logger.Output(zerolog.ConsoleWriter{
	//	Out:        os.Stdout,
	//	NoColor:    true,
	//	TimeFormat: "15:04:05.000",
	//})

	// Get discovery server address from command line.
	// The membership will be obtained from this server
	discoveryAddrForClients = os.Args[2]

	// Get output file prefix from command line.
	// Log and trace files will all begin with this
	outPrefix := os.Args[3]

	// Initialize membership module
	membership.Init()

	// Initialize request module (needed for bucket calculation)
	request.Init()

	// Start profiler if necessary
	if len(os.Args) > 4 {
		setUpProfiling(os.Args[4])
		defer profiling.StopProfiler()
	}

	RunBenchmarkClients(outPrefix)
}

// Enables and starts the profiler of used resources.
func setUpProfiling(outFilePrefix string) {
	profiling.StartProfiler("cpu", outFilePrefix+".cpu", 1)
	profiling.StartProfiler("block", outFilePrefix+".block", 1)
	profiling.StartProfiler("mutex", outFilePrefix+".mutex", 1)
}
