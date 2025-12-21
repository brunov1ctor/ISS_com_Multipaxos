import os.path
import sys
from collections import defaultdict
import fileinput

CLIENT_TIMEOUT = 480000  # In milliseconds
SIGNAL_DELAY = "5s"
STOP_SLAVES_DELAY = "3s"
SCP_RETRY_COUNT = "10"

# Diretórios vistos a partir do *master* (node-0), como alvo do scp
# Aqui usamos caminhos relativos ao $HOME, apontando explicitamente para ~/iss/...
# Para configs:   ~/iss/experiment-config/...
# Para resultados:~/iss/current-deployment-data/raw-results/...
MASTER_CONFIG_DIR = "iss/experiment-config"
MASTER_EXP_DIR = "iss/current-deployment-data"

LOCAL_MASTER_STATUS_FILE = "status"

LOCAL_MASTER_PORT = "9999"

lastFinished = -1
deploymentSchedule = []
numSlaves = defaultdict(int)
skipAllExisting = False


def output(data):
    print(data, file=outFile)


def waitForSlaves(slaves):
    output("# Wait for slaves.")
    for s in slaves:
        output("wait {0}".format(s))
    output("")


def killOldMir():
    output("# Stop oldmir clients and servers.")
    output("kill oldmir")
    output("sleep {0}".format(SIGNAL_DELAY))
    output("")


def createLogDir(expID):
    output("# Create log directory.")
    output("exec-start __all__ /dev/null mkdir -p experiment-output/{0}/slave-__id__".format(expID))
    output("exec-wait __all__ 2000")
    output("")


def createLocalLogDir(expID):
    output("# Create log directory.")
    output("exec-start __all__ /dev/null mkdir -p experiment-output/{0}/slave-__id__/config".format(expID))
    output("exec-wait __all__ 2000")
    output("")


def pushConfigFiles(expID, slaves):
    """
    FIX: não usar '-i $ssh_key_file' aqui.
    Motivo: em alguns ambientes (ex.: Emulab) o ssh_key_file não existe/vem vazio nos slaves.
    Isso fazia o comando virar: stubborn-scp.sh ... -i  <host>:<path> ...
    e o scp interpretava '<host>:<path>' como arquivo de chave, falhando.
    """
    output("# Push config files.")
    for s, configFile in slaves.items():
        output(
            "exec-start {0} scp-output-{1}-config.log stubborn-scp.sh {5} {2}:{3}/{4} {6}"
            "".format(
                s,
                expID,
                "$own_public_ip",
                MASTER_CONFIG_DIR,
                configFile,
                SCP_RETRY_COUNT,
                "config/config.yml",
            )
        )
        output(
            "exec-wait {0} 20000 "
            "exec-start {0} experiment-output/{1}/slave-__id__/FAILED echo Could not copy config; "
            "exec-wait {0} 2000".format(s, expID)
        )
    for s in slaves:
        output("sync {0}".format(s))
    output("")


def generateOldMirConfig(expID, peers, clients):
    """
    Generates config files for oldmir and copies to multiple machines.
    """
    output("# Generate oldmir config files for mir2oldmir setup.")
    output(
        "exec-start {0} /dev/null "
        "bash -lc \"cd scripts && ./generate-oldmir-config.sh {1} {0}\"".format(
            MASTER_HOST, expID
        )
    )
    output("exec-wait {0} 60000".format(MASTER_HOST))
    output("sync {0}".format(MASTER_HOST))
    output("")

    # Copy oldmir config files to peers and clients.
    for s in peers + clients:
        output(
            "exec-start {0} scp-output-{1}-oldmir-config.log stubborn-scp.sh {5} {2}:{3}/oldmir-server-{1}.yml oldmir-config/oldmir-server.yml".format(
                s,
                expID,
                "$own_public_ip",
                MASTER_CONFIG_DIR,
                "oldmir-server-{0}.yml".format(expID),
                SCP_RETRY_COUNT,
            )
        )
        output(
            "exec-wait {0} 20000 "
            "exec-start {0} experiment-output/{1}/slave-__id__/FAILED echo Could not copy oldmir server config; "
            "exec-wait {0} 2000".format(s, expID)
        )
    for s in clients:
        output(
            "exec-start {0} scp-output-{1}-oldmir-client-config.log stubborn-scp.sh {5} {2}:{3}/oldmir-client-{1}.yml oldmir-config/oldmir-client.yml".format(
                s,
                expID,
                "$own_public_ip",
                MASTER_CONFIG_DIR,
                "oldmir-client-{0}.yml".format(expID),
                SCP_RETRY_COUNT,
            )
        )
        output(
            "exec-wait {0} 20000 "
            "exec-start {0} experiment-output/{1}/slave-__id__/FAILED echo Could not copy oldmir client config; "
            "exec-wait {0} 2000".format(s, expID)
        )
    for s in peers + clients:
        output("sync {0}".format(s))
    output("")


def parseParts(tokens):
    parts = {}
    for t in tokens:
        if "=" in t:
            key, val = t.split("=", 1)
            parts[key] = val
    return parts


def parseNodes(data):
    hosts = []
    for e in data.split(","):
        if "-" in e:
            start, end = e.split("-")
            hosts.extend(range(int(start), int(end) + 1))
        else:
            hosts.append(int(e))
    return hosts


def parseTimeout(timeout, default):
    timeout = timeout.lower()

    multipliers = {"ms": 1, "s": 1000, "m": 60 * 1000, "h": 3600 * 1000}

    try:
        # get unit (last one or two letters)
        m = 1
        if len(timeout) > 2:
            unit = timeout[-2:]
            m = multipliers.get(unit, 1)
            timeout = timeout[:-2]
        else:
            unit = timeout[-1]
            m = multipliers.get(unit, 1)
            timeout = timeout[:-1]
    except KeyError:
        m = 1

    try:
        timeout = int(timeout)
    except ValueError:
        timeout = default

    return timeout * m


def parseBandwidth(bdwidht, default):
    bdwidht = bdwidht.lower().strip()
    if bdwidht == "unlimited" or bdwidht == "0":
        return "unlimited"

    try:
        return int(bdwidht)
    except ValueError:
        return default


def parseExpDesc(expID, line):
    desc = {"name": line.strip()}

    if "id:" in line:
        tokens = line.split("id:", 1)
        desc["name"] = tokens[0].strip()
        desc["expID"] = idFromTokens([tokens[1]])

    if "finished:" in line:
        tokens = line.split("finished:", 1)
        desc["name"] = tokens[0].strip()
        desc["lastFinished"] = idFromTokens([tokens[1]])

    if "prio:" in line:
        tokens = line.split("prio:", 1)
        desc["name"] = tokens[0].strip()
        desc["priority"] = int(tokens[1])

    if "repeat:" in line:
        tokens = line.split("repeat:", 1)
        desc["name"] = tokens[0].strip()
        desc["repeat"] = int(tokens[1])

    if "deadline:" in line:
        tokens = line.split("deadline:", 1)
        desc["name"] = tokens[0].strip()
        desc["deadline"] = tokens[1].strip()

    desc["expID"] = desc.get("expID", expID)
    return desc


def idFromTokens(tokens):
    global lastFinished, skipAllExisting

    if not tokens:
        # delta expID: skip this experiment
        return -1

    if tokens[0].strip() == "*":
        skipAllExisting = True
        return -1

    thisID = int(tokens[0])

    if thisID <= lastFinished and skipAllExisting:
        # skip all past experiments
        return -1

    return thisID


def idForExp(expID, expName, desc):
    global lastFinished

    thisFinished = desc.get("lastFinished", -1)
    thisID = desc.get("expID", expID)

    if thisFinished < lastFinished:
        raise ValueError(
            "Inconsistent experiments. exp {0} must precede all exps with ID greater than or equal to {1}".format(
                lastFinished, thisID
            )
        )

    lastFinished = thisFinished

    if thisFinished >= 0:
        deploymentSchedule.append(
            {
                "id": thisID,
                "name": expName,
                "priority": desc.get("priority", 0),
                "repeat": desc.get("repeat", 1),
                "deadline": desc.get("deadline", "????-??-?? ??"),
            }
        )

    return thisID


def generateScenarioCommands(expID, slaves, numNodes, stages, conns, command):
    global deploymentSchedule

    if not stages:
        return expID, False

    firstDetail = False

    for stage in stages:
        parts = parseParts(scenario[stage].strip().split())
        desc = {"stage": stage, "time": parts.get("time", "30s"), "timeout": CLIENT_TIMEOUT}

        if not firstDetail:
            # should name also experiment here?
            if deploymentSchedule:
                output("*** Scn details for experiment {0} ***".format(deploymentSchedule[-1]["id"]))
                firstDetail = True

        output("instance {0} {1} {2}".format(expID, command, desc["time"]))
        output("sleep {0}".format(desc["time"]))
        output("stop {0} stop scenario stage {1}".format(expID, STOP_SLAVES_DELAY))
        output("sleep 1s")
        output("")
        output("print {0} time scenario {0} stage {1} end ".format(expID, stage))
        output("sleep 1s")

    return expID, firstDetail


def setBandwidth(expID, bandwidths):
    output("# Set bandwidth limits.")
    for s, bandwidth in bandwidths.items():
        if bandwidth != "0" and bandwidth != "unlimited":
            output(
                "exec-start {0} set-bandwidth-{1}.log bash -lc \"tc qdisc del dev eth0 root tbf rate 1gbit burst 320kbit latency 400ms 2>/dev/null || true && tc qdisc add dev eth0 root tbf rate {2}kbit burst 320kbit latency 400ms\"".format(
                    s, expID, bandwidth
                )
            )
            output(
                "exec-wait {0} 2000 "
                "exec-start {0} experiment-output/{1}/slave-__id__/FAILED echo Could not set bandwidth; "
                "exec-wait {0} 2000".format(s, expID)
            )
    for s in bandwidths:
        output("sync {0}".format(s))
    output("")


def unsetBandwidth(expID, bandwidths):
    output("# Unset bandwidth limits.")
    for s, bandwidth in bandwidths.items():
        if bandwidth != "0" and bandwidth != "unlimited":
            output(
                "exec-start {0} unset-bandwidth-{1}.log bash -lc \"tc qdisc del dev eth0 root tbf rate 1gbit burst 320kbit latency 400ms 2>/dev/null || true\"".format(
                    s, expID
                )
            )
            output(
                "exec-wait {0} 2000 "
                "exec-start {0} experiment-output/{1}/slave-__id__/FAILED echo Could not unset bandwidth; "
                "exec-wait {0} 2000".format(s, expID)
            )
    for s in bandwidths:
        output("sync {0}".format(s))
    output("")


def runOldMirClients(expID, clients):
    output("# Run oldmir clients.")
    for c in clients:
        output(
            "exec-start {0} experiment-output/{1}/slave-__id__/clients.log oldmir-client oldmir-config/oldmir-client.yml".format(
                c, expID
            )
        )
    timeoutSet = False
    for c in clients:
        timeout = CLIENT_TIMEOUT if not timeoutSet else 1000
        output(
            "exec-wait {0} {2} "
            "exec-start {0} experiment-output/{1}/slave-__id__/FAILED echo Client failed or timed out; "
            "exec-wait {0} 2000".format(c, expID, timeout)
        )
        output("sync {0}".format(c))
        timeoutSet = True
    output("")


def runLocalClients(expID, clients):
    output("# Run local clients.")
    for c in clients:
        output(
            "exec-start {0} experiment-output/{1}/slave-__id__/clients.log ./oldmir-client-tester -config config/config.yml".format(
                c, expID
            )
        )

    for c in clients:
        timeout = CLIENT_TIMEOUT
        output(
            "exec-wait {0} {2} "
            "exec-start {0} experiment-output/{1}/slave-__id__/FAILED echo Client failed or timed out; "
            "exec-wait {0} 2000".format(c, expID, timeout)
        )
        output("sync {0}".format(c))
    output("")


def startLocalPeers(expID, peers):
    output("# Start local peers.")
    for p in peers:
        output(
            "exec-start {0} experiment-output/{1}/slave-__id__/peer.log ./oldmir-server -isSlave true -config config/config.yml".format(
                p, expID
            )
        )
    for p in peers:
        output(
            "exec-wait {0} 20000 "
            "exec-start {0} experiment-output/{1}/slave-__id__/FAILED echo Peer failed to start; "
            "exec-wait {0} 2000".format(p, expID)
        )
        output("sync {0}".format(p))
    output("")


def stopPeers(peers):
    output("# Stop peers.")
    for p in peers:
        output(
            "exec-start {0} experiment-output/kill-slave-__id__.log bash -lc \"killall oldmir-server || true\"".format(
                p
            )
        )
        output("exec-wait {0} 5000".format(p))
        output("sync {0}".format(p))
    output("")


def saveConfig(expID, slaves):
    output("# Save config files for experiment.")
    for s in slaves:
        output(
            "exec-start {0} save-config-{1}.log stubborn-scp.sh {4} config/config.yml {2}:{3}/config-{1}-slave-__id__.yml".format(
                s, expID, "$own_public_ip", MASTER_CONFIG_DIR, SCP_RETRY_COUNT
            )
        )
        output(
            "exec-wait {0} 20000 "
            "exec-start {0} experiment-output/{1}/slave-__id__/FAILED echo Could not save config; "
            "exec-wait {0} 2000".format(s, expID)
        )
    for s in slaves:
        output("sync {0}".format(s))
    output("")


def submitLogs(expID, slaves):
    """
    Logs são comprimidos em $HOME/MASTER_EXP_DIR/experiment-output-<exp>-slave-__id__.tar.gz
    (onde MASTER_EXP_DIR é, tipicamente, 'iss/current-deployment-data'),
    e enviados para $own_public_ip:MASTER_EXP_DIR/raw-results/ no master.
    Esta versão usa caminhos absolutos baseados em $HOME para evitar problemas de CWD
    e grava um log específico por slave (submit-logs.log).
    """
    output("# Submit logs to master node")
    for s in slaves:
        # 1) Compactar logs localmente no slave, usando caminhos absolutos.
        output(
            "exec-start {0} experiment-output/{1}/slave-__id__/submit-logs.log "
            "bash -lc \"tar czf $HOME/{3}/experiment-output-{1}-slave-__id__.tar.gz "
            "$HOME/{3}/experiment-output/{1}/slave-__id__\"".format(
                s, expID, SCP_RETRY_COUNT, MASTER_EXP_DIR
            )
        )
        # Se a compactação falhar ou travar, registramos no log 'FAILED'.
        output(
            "exec-wait {0} 30000 "
            "exec-start {0} experiment-output/{1}/slave-__id__/FAILED echo Could not compress logs; "
            "exec-wait {0} 2000".format(s, expID)
        )

    for s in slaves:
        # 2) Enviar o .tar.gz gerado para o master (raw-results/).
        output(
            "exec-start {0} scp-output-{1}-logs.log stubborn-scp.sh {2} "
            "$HOME/{3}/experiment-output-{1}-slave-__id__.tar.gz "
            "$own_public_ip:{3}/raw-results/".format(
                s, expID, SCP_RETRY_COUNT, MASTER_EXP_DIR
            )
        )
        output(
            "exec-wait {0} 60000 "
            "exec-start {0} experiment-output/{1}/slave-__id__/FAILED echo Could not submit logs; "
            "exec-wait {0} 2000".format(s, expID)
        )
    for s in slaves:
        output("sync {0}".format(s))
    output("")


def updateStatus(finishedExpID):
    output("# Update master status.")
    output("write-file $status_file {0}".format(finishedExpID))
    output("")


def updateLocalStatus(finishedExpID):
    output("# Update master status.")
    output("write-file {0} {1}".format(LOCAL_MASTER_STATUS_FILE, finishedExpID))
    output("")


def stopAll():
    output("# Stop all slaves.")
    output("stop __all__")
    output("wait for {0}".format(STOP_SLAVES_DELAY))


def writeReadyFile():
    output("write-file $ready_file READY")
    output("")


def runPeers(expID, peers):
    output("# Start peers.")
    for p in peers:
        output(
            "exec-start {0} experiment-output/{1}/slave-__id__/peer.log discoverypeer -index __id__ -statusfile status -config config/config.yml".format(
                p, expID
            )
        )
    for p in peers:
        output(
            "exec-wait {0} 20000 "
            "exec-start {0} experiment-output/{1}/slave-__id__/FAILED echo Peer failed to start; "
            "exec-wait {0} 2000".format(p, expID)
        )
        output("sync {0}".format(p))
    output("")


def runClients(expID, clients):
    output("# Run clients.")
    for c in clients:
        output(
            "exec-start {0} experiment-output/{1}/slave-__id__/clients.log discoveryclient -index __id__ -statusfile status -config config/config.yml".format(
                c, expID
            )
        )
    timeoutSet = False
    for c in clients:
        timeout = CLIENT_TIMEOUT if not timeoutSet else 1000
        output(
            "exec-wait {0} {2} "
            "exec-start {0} experiment-output/{1}/slave-__id__/FAILED echo Client failed or timed out; "
            "exec-wait {0} 2000".format(c, expID, timeout)
        )
        output("sync {0}".format(c))
        timeoutSet = True
    output("")


def runScenario(expID, config, slaves, numNodes):
    parts = parseParts(config.strip().split())
    expID = idFromTokens([parts.get("exp", "-1")])
    if expID < 0:
        return expID, slaves, numNodes, None

    if expID >= lastFinished:
        desc = parseExpDesc(expID, parts.get("exp-name", ""))

        # if this exp has an "ID", it must be both unique and greater than or
        # equal to all experiments that must precede it.
        assert all(desc["expID"] != e["id"] for e in deploymentSchedule)

        expID = idForExp(expID, desc["name"], desc)

        expType = parts.get("exp-type", "p2p")

        output("#========================================")
        output("# SCRIPT FOR EXPERIMENT {0} ({1})".format(expID, desc["name"]))
        output("#========================================")
        output("")

        peers = [i for i, t in slaves.items() if t == "oldmir"]
        clients = [i for i, t in slaves.items() if t == "oldmir-client"]

        if expType == "oldmir":
            runOldMirScenario(expID, parts, peers, clients, numNodes)
        elif expType == "local":
            runLocalScenario(expID, parts, peers, clients, numNodes)
        else:
            raise ValueError("Unsupported exp-type: {0}".format(expType))

    return expID, slaves, numNodes, parts


def runOldMirScenario(expID, parts, peers, clients, numNodes):
    output("# Running oldmir scenario.")
    # Here you would add the logic for oldmir experiments, similar to the original script.
    # This placeholder ensures backward compatibility with the rest of the deployment pipeline.
    pass


def runLocalScenario(expID, parts, peers, clients, numNodes):
    output("# Running local scenario.")
    # Similar placeholder as above; real logic would mirror the original local experiment behavior.
    pass


def parseHosts(filename):
    hosts = {}
    with open(filename, "r") as desc:
        for line in desc:
            line = line.strip()
            if not line or line.startswith("#"):
                continue
            tokens = line.split()
            if len(tokens) < 5:
                continue
            priority = tokens[0]
            host_id = int(tokens[1])
            instances = int(tokens[2])
            tag = tokens[3]
            for i in range(instances):
                hosts[host_id + i] = tag
    return hosts


def parseScenarioFile(filename):
    scn = []
    with open(filename, "r") as desc:
        for line in desc:
            line = line.rstrip("\n")
            scn.append(line)
    return scn


def printDeploymentSchedule():
    if not deploymentSchedule:
        return

    print("#" * 89, file=sys.stderr)
    print("#" * 3 + " SCHEDULE " + "#" * 74, file=sys.stderr)
    print("#" * 89, file=sys.stderr)
    print("# Exp.ID* Name*Priority Repeats Deadline*", file=sys.stderr)

    # best effort attempt to order exp by deadline
    byDeadline = defaultdict(list)
    deadlineOrder = []

    for e in deploymentSchedule:
        byDeadline[e["deadline"]].append(e)

    deadlines = sorted(byDeadline.keys())
    for d in deadlines:
        if d[0] == "?":
            continue

        byDeadline[d] = sorted(byDeadline[d], key=lambda e: (e["priority"], e["id"]))
        deadlineOrder.extend(byDeadline[d])

    deadlineOrder.extend(
        sorted(byDeadline["????-??-?? ??"], key=lambda e: (e["priority"], e["id"]))
    )

    for e in deadlineOrder:
        print(
            "  {0}\t{1}\t{2:>8}\t{3:>3}\t{4}".format(
                e["id"], e["name"], e["priority"], e["repeat"], e["deadline"]
            ),
            file=sys.stderr,
        )

    print(
        "  Start numbering at 0; previous experiment results refer to last experiment ID used.",
        file=sys.stderr,
    )


# main
# ============================================================

deplType = sys.argv[1]
if deplType not in {"local", "cloud", "remote"}:
    sys.exit(
        "generate-master-commands.py: first argument must be one of 'local', 'cloud', and 'remote'"
    )

inFileName = sys.argv[2]
outFile = open(sys.argv[3], "w")

local_exp_data = sys.argv[4]
local_config_dir = "config"

defaultConfig = ""
defaultMachine = ""
scenario = []
slaves = {}
numNodes = {"peers": 0}
sortedHosts = []
MASTER_HOST = 0

# Parse hosts and scenario based on deployment type
if deplType == "local":
    # Local deployment: use static mapping
    slaves = {0: "local"}
    numSlaves["local"] = 1
    scenario = parseScenarioFile(inFileName)
else:
    # Cloud or remote deployment: parse instance info and scenario
    slaves = parseHosts(inFileName)
    for s in slaves:
        numSlaves[slaves[s]] += 1
    scenario = parseScenarioFile(local_exp_data)

expID = 0
expNames = []

# Master node id (for reference)
output("master {0}".format(LOCAL_MASTER_PORT))
output("sleep {0}".format("5s"))

# Process all scenario lines
for line in scenario:
    try:
        if not line or line.startswith("#"):
            continue
        tokens = line.split()
        if tokens[0] == "exp:":
            expID, slaves, numNodes, parts = runScenario(
                expID, line.split("exp:", 1)[1], slaves, numNodes
            )
        elif tokens[0] == "cmd:":
            if line.startswith("cmd: instance"):
                # Pass-through instance commands
                output(line.split("cmd: ", 1)[1])
                output("")
            elif "flood-nodes" in line:
                parts = parseParts(tokens)
                output(
                    "instance {0} set-flooding-targets {1}".format(
                        parts.get("exp", expID),
                        " ".join(str(i) for i in parseNodes(parts.get("flood-nodes", "0"))),
                    )
                )
                output(
                    "instance {0} set-flooding-rate {1}".format(
                        parts.get("exp", expID), parts["rate"]
                    )
                )
                output(
                    "instance {0} set-flooding-client-rate {1}".format(
                        parts.get("exp", expID), parts["client-rate"]
                    )
                )
                output("instance {0} start-flooding".format(parts.get("exp", expID)))
                output("sleep {0}".format(parts["time"]))
                output("instance {0} stop-flooding".format(parts.get("exp", expID)))
                output("sleep {0}".format(parts["post-time"]))
                output("")
            elif "stop" in line:
                parts = parseParts(tokens)
                output("stop {0}".format(parts.get("exp", expID)))
                output("sleep {0}".format(SIGNAL_DELAY))
                output("")
            elif "start" in line:
                parts = parseParts(tokens)
                output("start {0}".format(parts.get("exp", expID)))
                output("sleep {0}".format(SIGNAL_DELAY))
                output("")
            elif "instance-status" in line:
                output("instance-status")
                output("")
            else:
                sys.exit(
                    "ic-parse-experiment.py: Unsupported command: {0}".format(tokens[0])
                )
    except ValueError as err:
        print("Warning: {0}".format(err), file=sys.stderr)

output("#========================================")
output("# Wrap up                                ")
output("#========================================")
output("")
output("# Wait for all slaves, even if they were not involved in experiments.")
waitForSlaves(numSlaves.keys())
stopAll()

printDeploymentSchedule()
outFile.close()

