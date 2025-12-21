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

SLAVE_CONFIG_FILE = "config/config.yml"
OLDMIR_SERVER_CONFIG = "config/oldmir-config-server.yml"
OLDMIR_CLIENT_CONFIG = "config/oldmir-config-client.yml"

LOCAL_IP_ADDRESS = "$own_public_ip"
LOCAL_MASTER_PORT = "$master_port"

numSlaves = {}
deploymentSchedule = []


def output(data):
    print(data, file=outFile)


def waitForSlaves(slaves):
    output("# Wait for slaves.")
    for s in slaves:
        output("wait for slaves {0} {1}".format(s, numSlaves[s]))
    output("")


def createLogDir(expID):
    output("# Create log directory.")
    output("exec-start __all__ /dev/null mkdir -p experiment-output/{0}/slave-__id__".format(expID))
    output("exec-wait __all__ 2000")
    output("")


def createLocalLogDir(expID):
    output("# Create local log directory.")
    output(
        "exec-start __all__ /dev/null mkdir -p "
        "experiment-output/{0}/slave-__id__/config".format(expID)
    )
    output("exec-wait __all__ 2000")
    output("")


def resetOldmir(numPeers, numClients):
    output("# Reset oldmir discovery service (if running).")
    output("discover-reset {0}".format(numPeers))
    output("discover-reset {0}".format(numClients))
    output("")


def resetDiscovery(numPeers):
    output("# Reset discovery service.")
    output("discover-reset {0}".format(numPeers))
    output("")


def runPeers(expID, peers):
    output("# Start peers and wait for discovery to stabilize.")
    for p in peers:
        output(
            "exec-start {0} experiment-output/{1}/slave-__id__/peer.log orderingpeer "
            "experiment-output/{1}/slave-__id__/{2} {3}:{4} {3} {3} "
            "experiment-output/{1}/slave-__id__/peer.trc experiment-output/{1}/slave-__id__/prof".format(
                p, expID, SLAVE_CONFIG_FILE, LOCAL_IP_ADDRESS, LOCAL_MASTER_PORT
            )
        )
    output("discover-wait")
    output("")


def runClients(expID, clients):
    output("# Run clients and wait for them to stop.")
    for c in clients:
        output(
            "exec-start {0} experiment-output/{1}/slave-__id__/clients.log orderingclient "
            "{2} $own_public_ip:$master_port experiment-output/{1}/slave-__id__/client "
            "experiment-output/{1}/slave-__id__/prof-client".format(
                c, expID, SLAVE_CONFIG_FILE
            )
        )
    timeout = CLIENT_TIMEOUT
    for c in clients:
        output(
            "exec-wait {0} {2} "
            "exec-start {0} experiment-output/{1}/slave-__id__/FAILED echo Client failed or timed out; "
            "exec-wait {0} 2000".format(c, expID, timeout)
        )
        output("sync {0}".format(c))
        timeout //= 2
    output("")


def generateOldMirConfig(expID, peers, clients):
    output("# Generate oldmir configs from ISS config.")
    output(
        "exec-start {0} experiment-output/{1}/slave-__id__/oldmir-config-gen.log "
        "oldmir-start.sh config {2} {3} {4} {5}".format(
            peers[0], expID, SLAVE_CONFIG_FILE, OLDMIR_SERVER_CONFIG, OLDMIR_CLIENT_CONFIG, expID
        )
    )
    output("discover-wait")

    numPeers = len(peers)
    resetOldmir(numPeers, len(clients))
    output("discover-reset {0}".format(numPeers))
    for p in peers:
        output(
            "exec-start {0} experiment-output/{1}/slave-__id__/config-gen.log "
            "oldmir-start.sh peer $own_public_ip:$master_port {2} {3} "
            "experiment-output/{1}/slave-__id__/peer.log __public_ip__ __private_ip__".format(
                p, expID, SLAVE_CONFIG_FILE, OLDMIR_SERVER_CONFIG
            )
        )
    output("discover-wait")

    for c in clients:
        output(
            "exec-start {0} experiment-output/{1}/slave-__id__/config-gen.log "
            "oldmir-start.sh client $own_public_ip:$master_port {2} {3} ".format(
                c, expID, SLAVE_CONFIG_FILE, OLDMIR_CLIENT_CONFIG
            )
        )
    output("discover-wait")
    output("")


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
    Versão ajustada do submitLogs para evitar problemas de CWD e dar logs
    mais explícitos de compressão/envio.

    Comportamento:
      1) No slave: gera $HOME/{MASTER_EXP_DIR}/experiment-output-<exp>-slave-__id__.tar.gz
      2) No slave: envia esse tar via stubborn-scp.sh para
         $own_public_ip:{MASTER_EXP_DIR}/raw-results/ no master.
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
    output("# Update master status (local).")
    output("write-file status {0}".format(finishedExpID))
    output("")


def stopAll():
    output("# Stop all slaves.")
    output("stop __all__")
    output("wait for slaves __all__ 0")
    output("sleep {0}".format(STOP_SLAVES_DELAY))
    output("")


def writeReadyFile():
    output("write-file $ready_file READY")
    output("")


def writeLocalReadyFile():
    output("write-file ready READY")
    output("")


def setDefaultConfig(configFile):
    global defaultConfig
    defaultConfig = configFile


def setDefaultMachine(machine):
    global defaultMachine
    defaultMachine = machine


def setDefaultBandwidth(bandwidth):
    global defaultBandwidth
    defaultBandwidth = bandwidth


def deploy(params):
    hostType = params[0]
    hostIds = params[1]
    numInstances = int(params[2])
    tag = params[3]
    machineTemplate = params[4]

    hosts = []
    for hostIdRange in hostIds.split(","):
        hostIdRange = hostIdRange.strip()
        if "-" in hostIdRange:
            start, end = map(int, hostIdRange.split("-"))
            hosts.extend(range(start, end + 1))
        else:
            hosts.append(int(hostIdRange))

    for hostId in hosts:
        numSlaves[hostId] = numInstances
        output(
            "deploy {0} {1} {2} {3} {4}".format(
                hostType, hostId, numInstances, tag, machineTemplate
            )
        )


def run(expID, params):
    global numSlaves, deploymentSchedule

    configFile = defaultConfig
    machineTemplate = defaultMachine
    bandwidth = defaultBandwidth

    peers = []
    clients = []
    bandwidths = {}

    for param in params:
        key, value = param.split("=", 1)
        if key == "config":
            configFile = value
        elif key == "machine":
            machineTemplate = value
        elif key == "bandwidth":
            for mapping in value.split(","):
                hostId, bw = mapping.split(":")
                bandwidths[int(hostId)] = int(bw)
        elif key == "peers":
            peers = [int(x) for x in value.split(",")]
        elif key == "clients":
            clients = [int(x) for x in value.split(",")]

    numPeers = sum(numSlaves.get(p, 0) for p in peers)
    if numPeers == 0:
        return

    numClients = sum(numSlaves.get(c, 0) for c in clients)

    deploymentSchedule.append(
        {
            "id": expID,
            "config": configFile,
            "machine": machineTemplate,
            "bandwidth": bandwidths,
            "peers": peers,
            "clients": clients,
        }
    )

    createLogDir(expID)
    createLocalLogDir(expID)

    if bandwidths:
        setBandwidth(expID, bandwidths)

    generateOldMirConfig(expID, peers, clients)

    resetDiscovery(numPeers)
    runPeers(expID, peers)
    runClients(expID, clients)
    submitLogs(expID, peers + clients)

    if bandwidths:
        unsetBandwidth(expID, bandwidths)

    updateStatus(expID)
    updateLocalStatus(expID)


def printDeploymentSchedule():
    if not deploymentSchedule:
        return

    print("#" * 80, file=sys.stderr)
    print("# Deployment schedule:", file=sys.stderr)
    print("#" * 80, file=sys.stderr)
    for exp in deploymentSchedule:
        print(
            "Experiment {id}: config={config}, machine={machine}, peers={peers}, clients={clients}".format(
                **exp
            ),
            file=sys.stderr,
        )
    print("#" * 80, file=sys.stderr)


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
defaultBandwidth = "unlimited"

experimentIdDigits = 3
idOffset = 0

if deplType == "local":
    writeLocalReadyFile()
else:
    writeReadyFile()

with open(inFileName) as inFile:
    for line in inFile:
        if line.strip() == "" or line.strip().startswith("#"):
            continue

        tokens = line.split()

        if tokens[0] == "deploy":
            deploy(tokens[1:])
        elif tokens[0] == "run":
            run(tokens[1], tokens[2:])
        elif tokens[0] == "config:":
            defaultConfig = tokens[1]
        elif tokens[0] == "machine:":
            defaultMachine = tokens[1]
        elif tokens[0] == "bandwidth:":
            defaultBandwidth = tokens[1]
        else:
            sys.exit(
                "ic-parse-experiment.py: Unsupported command: {0}".format(tokens[0])
            )

output("#========================================")
output("# Wrap up                                ")
output("#========================================")
output("")
output("# Wait for all slaves, even if they were not involved in experiments.")
waitForSlaves(numSlaves.keys())
stopAll()

printDeploymentSchedule()
outFile.close()

