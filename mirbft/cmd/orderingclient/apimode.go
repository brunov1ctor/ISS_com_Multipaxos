package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/hyperledger-labs/mirbft/clientlib"
	"github.com/hyperledger-labs/mirbft/config"
	"github.com/hyperledger-labs/mirbft/membership"
	"github.com/hyperledger-labs/mirbft/request"
)

func printTopLevelHelp() {
	fmt.Println(`orderingclient -- cliente do sistema (MirBFT/ISS), dois modos:

1) BENCHMARK (gerador de carga automático, sem comando na frente):
   orderingclient <config_file> <discovery_addr:port> <out_file_prefix> [profiler_output_prefix]

   Conecta, espera a 1a atribuição de bucket e começa a mandar tráfego sintético sozinho,
   conforme RequestRate/CrossOpRatio/ClientRunTime etc. do config.yml. Ainda existe por causa
   dos sweeps automatizados (generate-local-config.sh) e porque "overload" (abaixo) usa esse
   MESMO motor por dentro -- no fluxo normal (deploy só sobe o sistema, fica esperando), você
   usa "overload" em vez de chamar isso direto.

2) COMANDOS (um processo por invocação, você decide quando rodar cada um):
   orderingclient <comando> [config_file] [discovery_addr:port] [args...]

   config/addr são OPCIONAIS rodando num nó que já participou de um deploy (ex. node-0): o
   binário acha os dois sozinho em $ISS_BASE_DIR/config/config.yml e .../master-addr (escritos
   pelo próprio deploy). Fora disso (ex. do seu laptop), digite os dois na mão, ou exporte
   ORDERINGCLIENT_CONFIG/ORDERINGCLIENT_ADDR uma vez por sessão de terminal.
`)
	apiUsage()
	fmt.Println(`
exemplos (de dentro de um nó já deployado -- config/addr resolvidos sozinhos):
  orderingclient ls
  orderingclient put 1 saldo:alice 100
  orderingclient get 1 saldo:alice
  orderingclient put pedido-42 "dados do pedido"
  orderingclient delete 1 saldo:alice
  orderingclient overload all 30
  orderingclient overload 1 30 --api 10000 --delete 10   # carga real no G1, 10k chaves, 10% delete
  orderingclient repl                           # vários comandos, uma conexão só
  orderingclient                                # idem -- "repl" nem precisa ser digitado

exemplos (de fora do Emulab, ou config/addr explícitos):
  orderingclient config.yml 155.98.38.100:9999 ./out
  orderingclient put config.yml 155.98.38.100:9999 1 saldo:alice 100`)
}

func apiUsage() {
	fmt.Println(`uso: orderingclient <comando> [config_file] [discovery_addr:port] [args...]
     (config/addr opcionais num nó já deployado -- ver acima; ou forma antiga:
     orderingclient api <config_file> <discovery_addr:port> <comando> [args...])

comandos:
  help                                      -- mostra esta ajuda (tambem "-h"/"--help", ou so
                                            -- "orderingclient" sozinho quando config/addr nao
                                            -- resolvem sozinhos -- ver "repl" abaixo)
  ls                                        -- mostra a topologia (nos conhecidos e grupos
                                            -- definidos) -- so bate no proxy e volta, sem consenso
  put [groupID] <key> <value>              -- grava value na key. Com groupID: nesse grupo
                                            -- escolhido. Sem groupID: servidor escolhe por hash
  get [groupID] <key>                      -- le key. Com groupID: desse grupo escolhido.
                                            -- Sem groupID: servidor escolhe por hash
  delete [groupID] <key>                   -- apaga key (idempotente -- nao e erro apagar
                                            -- chave inexistente). Com/sem groupID: mesma
                                            -- regra de put/get
  multicast <grupo[,grupo...]> <mensagem>  -- multicast da mensagem para o(s) grupo(s). A ordem
                                            -- (local/global) e do sistema: --order no deploy
  transfer <fromGroup> <fromKey> <toGroup> <toKey> <amount>
                                            -- move amount de fromKey (fromGroup) pra toKey (toGroup)
  overload <protocolo|grupo> <duracaoSegundos> [--api <tamanhoConjunto> [--write <pct>]
           [--delete <pct>]] [outPrefix]
                                            -- liga o gerador de carga continua por
                                            -- duracaoSegundos. <protocolo|grupo> aceita um nome
                                            -- (multipaxos/pbft/hotstuff/raft/all) so como rotulo
                                            -- de confirmacao contra o Orderer do config.yml, OU
                                            -- um numero de grupo (MultiPaxosMulticast) pra pinar
                                            -- toda a carga gerada nesse grupo.
                                            -- Sem --api: payload sintetico de sempre (GET/TX,
                                            -- RequestRate/CrossOpRatio do config.yml -- nunca
                                            -- toca o estado real da API).
                                            -- Com --api <N>: semeia N chaves de verdade
                                            -- (APIPUT) ANTES de comecar a medir, depois GET
                                            -- (padrao) / PUT (sobrescreve, --write <pct>) /
                                            -- DELETE (--delete <pct>) so sobre essas N chaves
                                            -- ja semeadas -- found=true garantido de verdade.
  repl                                      -- conecta uma vez e fica lendo comando por comando
                                            -- do terminal (ls/put/get/delete/transfer/overload/
                                            -- exit),
                                            -- reaproveitando a mesma conexao -- evita pagar o
                                            -- handshake (discovery + TLS com todo orderer) a
                                            -- cada comando, que o modo normal paga toda vez.
                                            -- "orderingclient" sozinho (sem nenhum argumento) ja
                                            -- entra direto no repl quando config/addr resolvem
                                            -- sozinhos -- nao precisa nem digitar "repl".`)
}

func runAPIMode(args []string) {
	if len(args) < 3 {
		apiUsage()
		os.Exit(1)
	}
	configFile, discoveryAddr, cmd := args[0], args[1], args[2]
	rest := args[3:]

	// overload não usa clientlib (não é uma operação única da máquina de estados) -- ele liga
	// por dentro o MESMO gerador de carga contínuo do benchmark (RunBenchmarkClients, ver
	// client.go), que tem sua própria pilha de conexão (newClient/discovery.RegisterClient).
	// Tratado antes do clientlib.Connect pra não abrir duas conexões/registros à toa.
	if cmd == "overload" {
		if err := runOverload(configFile, discoveryAddr, rest); err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		return
	}

	if cmd == "repl" {
		runREPL(configFile, discoveryAddr)
		return
	}

	c, err := clientlib.Connect(configFile, discoveryAddr)
	if err != nil {
		fmt.Println("connect:", err)
		os.Exit(1)
	}
	defer c.Close()

	if err := dispatchCommand(c, cmd, rest); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

// dispatchCommand nunca chama os.Exit -- só devolve erro, pra cada chamador decidir o que
// fazer (runAPIMode sai do processo; runREPL só imprime e continua o laço).
func dispatchCommand(c *clientlib.Client, cmd string, rest []string) error {
	switch cmd {
	case "ls":
		if len(rest) != 0 {
			return fmt.Errorf("uso: ls (sem argumentos)")
		}
		topology, err := c.Ls()
		if err != nil {
			return fmt.Errorf("ls failed: %w", err)
		}
		fmt.Println(topology)

	// grupo OPCIONAL: o tamanho de rest desambigua sozinho (2 args = hash automático, 3 = grupo
	// explícito na frente).
	case "put":
		switch len(rest) {
		case 2: // put <key> <value> -- automático (hash)
			if err := c.PutAuto(rest[0], []byte(rest[1])); err != nil {
				return fmt.Errorf("put failed: %w", err)
			}
		case 3: // put <groupID> <key> <value> -- grupo explícito
			gid, err := parseUint32(rest[0])
			if err != nil {
				return err
			}
			if err := c.Put(gid, rest[1], []byte(rest[2])); err != nil {
				return fmt.Errorf("put failed: %w", err)
			}
		default:
			return fmt.Errorf("uso: put [groupID] <key> <value>")
		}
		fmt.Println("OK")

	case "get":
		var value []byte
		var found bool
		var err error
		switch len(rest) {
		case 1: // get <key> -- automático (hash)
			value, found, err = c.GetAuto(rest[0])
		case 2: // get <groupID> <key> -- grupo explícito
			var gid uint32
			if gid, err = parseUint32(rest[0]); err != nil {
				return err
			}
			value, found, err = c.Get(gid, rest[1])
		default:
			return fmt.Errorf("uso: get [groupID] <key>")
		}
		if err != nil {
			return fmt.Errorf("get failed: %w", err)
		}
		fmt.Printf("found=%v value=%q\n", found, value)

	// delete: mesma desambiguação de put/get pelo tamanho de rest.
	case "delete":
		var existed bool
		var err error
		switch len(rest) {
		case 1: // delete <key> -- automático (hash)
			existed, err = c.DeleteAuto(rest[0])
		case 2: // delete <groupID> <key> -- grupo explícito
			var gid uint32
			if gid, err = parseUint32(rest[0]); err != nil {
				return err
			}
			existed, err = c.Delete(gid, rest[1])
		default:
			return fmt.Errorf("uso: delete [groupID] <key>")
		}
		if err != nil {
			return fmt.Errorf("delete failed: %w", err)
		}
		fmt.Printf("deleted=%v\n", existed)

	case "multicast":
		if len(rest) < 2 {
			return fmt.Errorf("uso: multicast <grupo[,grupo...]> <mensagem>")
		}
		var groups []uint32
		for _, g := range strings.Split(rest[0], ",") {
			gid, err := parseUint32(g)
			if err != nil {
				return err
			}
			groups = append(groups, gid)
		}
		if _, err := c.MulticastGroups(groups, []byte(strings.Join(rest[1:], " "))); err != nil {
			return fmt.Errorf("multicast failed: %w", err)
		}
		fmt.Println("OK")

	case "transfer":
		if len(rest) != 5 {
			return fmt.Errorf("uso: transfer <fromGroup> <fromKey> <toGroup> <toKey> <amount>")
		}
		fromGroup, err := parseUint32(rest[0])
		if err != nil {
			return err
		}
		toGroup, err := parseUint32(rest[2])
		if err != nil {
			return err
		}
		amount, err := strconv.ParseInt(rest[4], 10, 64)
		if err != nil {
			return fmt.Errorf("amount invalido: %v", err)
		}
		if err := c.Transfer(fromGroup, rest[1], toGroup, rest[3], amount); err != nil {
			return fmt.Errorf("transfer failed: %w", err)
		}
		fmt.Println("OK")

	default:
		return fmt.Errorf("comando desconhecido: %q (use ls/put/get/delete/multicast/transfer/overload/help)", cmd)
	}
	return nil
}

// splitReplLine tokeniza a linha digitada no repl, respeitando aspas duplas como um token só
// (ex. put 1 pedido-42 "dados do pedido" -> 4 tokens, nao 6). Fora do repl isso nao e um
// problema porque o shell do SO ja faz essa tokenizacao antes do programa receber argv --
// aqui, lendo a linha crua via bufio.Scanner, ninguem faz isso por nos.
func splitReplLine(line string) []string {
	var tokens []string
	var cur strings.Builder
	inQuotes := false
	hasToken := false
	for _, r := range line {
		switch {
		case r == '"':
			inQuotes = !inQuotes
			hasToken = true
		case unicode.IsSpace(r) && !inQuotes:
			if hasToken {
				tokens = append(tokens, cur.String())
				cur.Reset()
				hasToken = false
			}
		default:
			cur.WriteRune(r)
			hasToken = true
		}
	}
	if hasToken {
		tokens = append(tokens, cur.String())
	}
	return tokens
}

// "overload" dentro do REPL não reaproveita a conexão do REPL -- usa seu próprio motor
// (RunBenchmarkClients), igual fora do REPL; a linha fica bloqueada até a duração acabar.
func runREPL(configFile, discoveryAddr string) {
	c, err := clientlib.Connect(configFile, discoveryAddr)
	if err != nil {
		fmt.Println("connect:", err)
		os.Exit(1)
	}
	defer c.Close()

	fmt.Println("orderingclient repl -- conectado. Comandos: ls / put / get / delete / multicast / transfer / overload / exit")
	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("> ")
		if !scanner.Scan() {
			break // EOF (Ctrl+D)
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if line == "exit" || line == "quit" {
			break
		}
		parts := splitReplLine(line)
		cmd, rest := parts[0], parts[1:]
		if cmd == "overload" {
			if err := runOverload(configFile, discoveryAddr, rest); err != nil {
				fmt.Println(err)
			}
			continue
		}
		if err := dispatchCommand(c, cmd, rest); err != nil {
			fmt.Println(err)
		}
	}
}

// ordererAliases mapeia os nomes de protocolo aceitos no comando overload (minúsculos) pro
// valor literal de config.Config.Orderer (ver cmd/orderingpeer/main.go:setOrderer -- é de lá
// que vêm esses literais exatos: "Pbft", não "PBFT").
var ordererAliases = map[string]string{
	"multipaxos": "MultiPaxos", // cobre tanto "MultiPaxos" quanto "MultiPaxosMulticast" (prefixo)
	"pbft":       "Pbft",
	"hotstuff":   "HotStuff",
	"raft":       "Raft",
}

// runOverload nunca chama os.Exit -- devolve erro pro chamador decidir (runAPIMode sai do
// processo; runREPL só imprime e volta a esperar o próximo comando, sem matar a sessão).
func runOverload(configFile, discoveryAddr string, rest []string) error {
	if len(rest) < 2 {
		return fmt.Errorf("uso: overload <protocolo|grupo> <duracaoSegundos> " +
			"[--api <tamanhoConjunto> [--write <pct>] [--delete <pct>]] [outPrefix]")
	}
	target := rest[0]
	durationSec, err := strconv.ParseFloat(rest[1], 64)
	if err != nil || durationSec <= 0 {
		return fmt.Errorf("duração inválida: %s", rest[1])
	}

	// Flags (--api/--write/--delete) e o outPrefix posicional opcional podem vir em qualquer
	// ordem depois de <protocolo|grupo> <duração> -- escaneia o resto procurando as flags
	// conhecidas; o que sobrar (no máximo 1 token) é o outPrefix.
	apiWorkingSet := 0
	writeRatio := 0
	deleteRatio := 0
	var positional []string
	for i := 2; i < len(rest); i++ {
		switch rest[i] {
		case "--api":
			if i+1 >= len(rest) {
				return fmt.Errorf("--api precisa de um tamanho de conjunto (ex.: --api 10000)")
			}
			apiWorkingSet, err = strconv.Atoi(rest[i+1])
			if err != nil || apiWorkingSet <= 0 {
				return fmt.Errorf("tamanho de conjunto inválido pra --api: %s", rest[i+1])
			}
			i++
		case "--write":
			if i+1 >= len(rest) {
				return fmt.Errorf("--write precisa de uma porcentagem (ex.: --write 10)")
			}
			writeRatio, err = strconv.Atoi(rest[i+1])
			if err != nil || writeRatio < 0 || writeRatio > 100 {
				return fmt.Errorf("porcentagem inválida pra --write: %s", rest[i+1])
			}
			i++
		case "--delete":
			if i+1 >= len(rest) {
				return fmt.Errorf("--delete precisa de uma porcentagem (ex.: --delete 10)")
			}
			deleteRatio, err = strconv.Atoi(rest[i+1])
			if err != nil || deleteRatio < 0 || deleteRatio > 100 {
				return fmt.Errorf("porcentagem inválida pra --delete: %s", rest[i+1])
			}
			i++
		default:
			positional = append(positional, rest[i])
		}
	}
	if writeRatio+deleteRatio > 100 {
		return fmt.Errorf("--write (%d) + --delete (%d) passa de 100%%", writeRatio, deleteRatio)
	}
	if (writeRatio > 0 || deleteRatio > 0) && apiWorkingSet == 0 {
		return fmt.Errorf("--write/--delete só fazem sentido junto com --api <tamanhoConjunto>")
	}
	if len(positional) > 1 {
		return fmt.Errorf("argumento(s) não reconhecido(s): %v", positional[1:])
	}
	outPrefix := fmt.Sprintf("overload-%d", time.Now().Unix())
	if len(positional) == 1 {
		outPrefix = positional[0]
	}

	config.LoadFile(configFile)

	// Reseta o estado de overload da invocação anterior -- importante no repl, onde o mesmo
	// processo (e os mesmos campos em memória de config.Config) atende vários comandos em
	// sequência; sem isso, um "overload 1 10 --api 100" deixaria o grupo e o modo --api
	// vazando pro próximo "overload all 10" sem --api nenhum.
	config.Config.OverloadGroupOverride = nil
	config.Config.ApiWorkingSetSize = apiWorkingSet
	config.Config.ApiWriteRatio = writeRatio
	config.Config.ApiDeleteRatio = deleteRatio

	if groupID, numErr := strconv.ParseUint(target, 10, 32); numErr == nil {
		// Grupo explícito: só faz sentido em MultiPaxosMulticast (único orderer com o conceito
		// de grupo de dados -- ver escopo documentado em orderer/statemachine.go).
		if config.Config.Orderer != "MultiPaxosMulticast" {
			return fmt.Errorf("grupo explícito (%d) pedido, mas config.yml está configurado como "+
				"Orderer=%q -- só MultiPaxosMulticast tem grupos de dados. Use um nome de "+
				"protocolo em vez de um número (ex.: 'all').", groupID, config.Config.Orderer)
		}
		g := uint32(groupID)
		config.Config.OverloadGroupOverride = &g
	} else if strings.ToLower(target) != "all" {
		// Nome de protocolo: só um rótulo de confirmação -- o protocolo de fato em uso é
		// sempre o que já está no config.yml carregado; aqui só avisamos se não bater.
		want, known := ordererAliases[strings.ToLower(target)]
		if !known {
			return fmt.Errorf("protocolo/grupo desconhecido: %q (use multipaxos/pbft/hotstuff/raft/all "+
				"ou um número de grupo)", target)
		}
		if !strings.HasPrefix(config.Config.Orderer, want) {
			fmt.Printf("aviso: pediu overload %q, mas config.yml está configurado como "+
				"Orderer=%q -- a carga vai sair mesmo assim, roteada como esse config.yml manda.\n",
				target, config.Config.Orderer)
		}
	}
	// "all": nenhuma checagem, nenhum override -- roteamento automático/hash, igual o benchmark
	// de hoje, independente de qual orderer está de fato configurado.

	config.Config.ClientRunTime = int(durationSec * 1000)
	discoveryAddrForClients = discoveryAddr

	membership.Init()
	request.Init()

	if apiWorkingSet > 0 {
		fmt.Printf("overload: %s por %.0fs (Orderer=%s) -- modo --api: semeando %d chaves, "+
			"depois write=%d%% delete=%d%% get=%d%%...\n",
			target, durationSec, config.Config.Orderer, apiWorkingSet,
			writeRatio, deleteRatio, 100-writeRatio-deleteRatio)
	} else {
		fmt.Printf("overload: %s por %.0fs (Orderer=%s)...\n", target, durationSec, config.Config.Orderer)
	}
	RunBenchmarkClients(outPrefix)
	fmt.Println("OK")
	return nil
}

func parseUint32(s string) (uint32, error) {
	v, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("groupID inválido: %v", err)
	}
	return uint32(v), nil
}
