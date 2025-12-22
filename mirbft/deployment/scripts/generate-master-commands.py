import os.path
import sys
from collections import defaultdict
import fileinput

CLIENT_TIMEOUT = 480000  # ms
SIGNAL_DELAY = "5s"
STOP_SLAVES_DELAY = "3s"
SCP_RETRY_COUNT = "10"
MASTER_CONFIG_DIR = "experiment-config"
MASTER_EXP_DIR = "current-deployment-data"
SLAVE_CONFIG_FILE = "config/config.yml"
OLDMIR_SERVER_CONFIG = "config/oldmir-config-server.yml"
OLDMIR_CLIENT_CONFIG = "config/oldmir-config-client.yml"
LOCAL_MASTER_STATUS_FILE = "master-status"
LOCAL_IP_ADDRESS = "127.0.0.1"
LOCAL_MASTER_PORT = "9999"

lastFinished = -1
deploymentSchedule = []
numSlaves = defaultdict(int)
skipAllExisting = False


def output(data):
    print(data, file=outFile)


def waitForSlaves(slaves):
    output("# wait slaves")
    for s in slaves:
        output("wait for slaves {0} {1}".format(s, numSlaves[s]))
    output("")


def createLogDir(expID):
    output("# mkdir log dir")
    output("exec-start __all__ /dev/null mkdir -p experiment-output/{0}/slave-__id__".format(expID))
    output("exec-wait __all__ 2000")
    output("")


def createLocalLogDir(expID):
    output("# mkdir local log dir")
    output("exec-start __all__ /dev/null mkdir -p experiment-output/{0}/slave-__id__/config".format(expID))
    output("exec-wait __all__ 2000")
    output("")


def pushConfigFiles(expID, slaves):
    output("# push config")
    for s, configFile in slaves.items():
        if deplType == "remote":
            scp_cmd = (
                "exec-start {0} scp-output-{1}-config.log stubborn-scp.sh {5} "
                "$own_public_ip:{2}/{3} {4}"
            )
        else:
            scp_cmd = (
                "exec-start {0} scp-output-{1}-config.log stubborn-scp.sh {5} "
                "-i $ssh_key_file $own_public_ip:{2}/{3} {4}"
            )
        output(
            scp_cmd.format(
                s,
                expID,
                MASTER_CONFIG_DIR,
                configFile,
                SLAVE_CONFIG_FILE,
                SCP_RETRY_COUNT,
            )
        )
        output(
            "exec-wait {0} 60000 "
            "exec-start {0} experiment-output/{1}/slave-__id__/FAILED echo Could not fetch config; "
            "exec-wait {0} 2000".format(s, expID)
        )
    for s in slaves:
        output("sync {0}".format(s))
    output("")


def pushLocalConfigFiles(expID, slaves):
    output("# push local config")
    for s, configFile in slaves.items():
        output(
            "exec-start {0} /dev/null cp {1}/{2} experiment-output/{3}/slave-__id__/{4}".format(
                s, local_config_dir, configFile, expID, SLAVE_CONFIG_FILE
            )
        )
        output("exec-wait {0} 2000".format(s))
    for s in slaves:
        output("sync {0}".format(s))
    output("")


def setBandwidth(expID, bandwidths):
    output("# set bandwidth")
    for s, bandwidth in bandwidths.items():
        if bandwidth != "0" and bandwidth != "unlimited":
            output(
                "exec-start {0} set-bandwidth-{1}.log tc qdisc add dev eth0 root tbf rate {2} burst 320kbit latency 400ms"
                "".format(s, expID, bandwidth)
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
    output("# unset bandwidth")
    for s, bandwidth in bandwidths.items():
        if bandwidth != "0" and bandwidth != "unlimited":
            output(
                "exec-start {0} unset-bandwidth-{1}.log tc qdisc del dev eth0 root tbf rate {2} burst 320kbit latency 400ms"
                "".format(s, expID, bandwidth)
            )
            output(
                "exec-wait {0} 2000 "
                "exec-start {0} experiment-output/{1}/slave-__id__/FAILED echo Could not unset bandwidth; "
                "exec-wait {0} 2000".format(s, expID)
            )
    for s, bandwidth in bandwidths.items():
        output("sync {0}".format(s))
    output("")


def startPeers(expID, peers):
    numPeers = 0
    for p in peers:
        numPeers += numSlaves[p]

    output("# start peers")
    output("discover-reset {0}".format(numPeers))
    for p in peers:
        output(
            "exec-start {0} experiment-output/{1}/slave-__id__/peer.log orderingpeer "
            "{2} $own_public_ip:$master_port __public_ip__ __private_ip__ "
            "experiment-output/{1}/slave-__id__/peer.trc experiment-output/{1}/slave-__id__/prof".format(
                p, expID, SLAVE_CONFIG_FILE
            )
        )
    output("discover-wait")
    output("")


def startOldMirPeers(expID, peers):
    output("# start oldmir peers")
    for p in peers:
        output(
            "exec-start {0} experiment-output/{1}/slave-__id__/peer.log server {2} ".format(
                p, expID, OLDMIR_SERVER_CONFIG
            )
        )
    output("")


def startLocalPeers(expID, peers):
    numPeers = 0
    for p in peers:
        numPeers += numSlaves[p]

    output("# start local peers")
    output("discover-reset {0}".format(numPeers))
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
    output("# run clients")
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
    numPeers = 0
    for p in peers:
        numPeers += numSlaves[p]

    output("# oldmir config")

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
    timeoutSet = False
    for p in peers:
        if not timeoutSet:
            timeout = CLIENT_TIMEOUT
        else:
            timeout = 1000
        output(
            "exec-wait {0} {2} "
            "exec-start {0} experiment-output/{1}/slave-__id__/FAILED echo Failed to generate configuration; "
            "exec-wait {0} 2000".format(p, expID, timeout)
        )
        output("sync {0}".format(p))
    timeoutSet = False
    for c in clients:
        if not timeoutSet:
            timeout = CLIENT_TIMEOUT
        else:
            timeout = 1000
        output(
            "exec-wait {0} {2} "
            "exec-start {0} experiment-output/{1}/slave-__id__/FAILED echo Failed to generate configuration; "
            "exec-wait {0} 2000".format(c, expID, timeout)
        )
        output("sync {0}".format(c))
    output("")


def runOldMirClients(expID, clients):
    output("# run oldmir clients")
    for c in clients:
        output(
            "exec-start {0} experiment-output/{1}/slave-__id__/clients.log client {2} ".format(
                c, expID, OLDMIR_CLIENT_CONFIG
            )
        )
    timeoutSet = False
    for c in clients:
        if not timeoutSet:
            timeout = CLIENT_TIMEOUT
        else:
            timeout = 1000
        output(
            "exec-wait {0} {2} "
            "exec-start {0} experiment-output/{1}/slave-__id__/FAILED echo Client failed or timed out; "
            "exec-wait {0} 2000".format(c, expID, timeout)
        )
        output("sync {0}".format(c))
    output("")


def runLocalClients(expID, clients):
    output("wait for 2s")
    output("# run local clients")

    for c in clients:
        output(
            "exec-start {0} experiment-output/{1}/slave-__id__/clients.log orderingclient "
            "experiment-output/{1}/slave-__id__/{2} {3}:{4} experiment-output/{1}/slave-__id__/client "
            "experiment-output/{1}/slave-__id__/prof-client".format(
                c, expID, SLAVE_CONFIG_FILE, LOCAL_IP_ADDRESS, LOCAL_MASTER_PORT
            )
        )
    timeoutSet = False
    for c in clients:
        if not timeoutSet:
            timeout = CLIENT_TIMEOUT
        else:
            timeout = 1000
        output(
            "exec-wait {0} {2} "
            "exec-start {0} experiment-output/{1}/slave-__id__/FAILED echo Client failed or timed out; "
            "exec-wait {0} 2000".format(c, expID, timeout)
        )
        output("sync {0}".format(c))
    output("")


def stopPeers(peers):
    output("# stop peers")
    for p in peers:
        output("exec-signal {0} SIGINT".format(p))
    output("wait for {0}".format(SIGNAL_DELAY))
    output("")


def saveConfig(expID, slaves):
    output("# save config")
    for s in slaves:
        output(
            "exec-start {0} /dev/null cp {1} experiment-output/{2}/slave-__id__".format(
                s, SLAVE_CONFIG_FILE, expID
            )
        )
        output(
            "exec-wait {0} 2000 "
            "exec-start {0} experiment-output/{1}/slave-__id__/FAILED echo Could not log config file; "
            "exec-wait {0} 2000".format(s, expID)
        )
    output("")


def submitLogs(expID, slaves):
    output("# submit logs")
    for s in slaves:
        output(
            "exec-start {0} /dev/null tar czf experiment-output-{1}-slave-__id__.tar.gz "
            "experiment-output/{1}/slave-__id__".format(
                s, expID
            )
        )
        output(
            "exec-wait {0} 30000 "
            "exec-start {0} experiment-output/{1}/slave-__id__/FAILED echo Could not compress logs; "
            "exec-wait {0} 2000".format(s, expID)
        )

    for s in slaves:
        if deplType == "remote":
            scp_cmd = (
                "exec-start {0} scp-output-{1}-logs.log stubborn-scp.sh {2} "
                "experiment-output-{1}-slave-__id__.tar.gz $own_public_ip:{3}/raw-results/"
            )
        else:
            scp_cmd = (
                "exec-start {0} scp-output-{1}-logs.log stubborn-scp.sh {2} "
                "-i $ssh_key_file experiment-output-{1}-slave-__id__.tar.gz $own_public_ip:{3}/raw-results/"
            )

        output(
            scp_cmd.format(
                s,
                expID,
                SCP_RETRY_COUNT,
                MASTER_EXP_DIR,
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
    output("# update status")
    output("write-file $status_file {0}".format(finishedExpID))
    output("")


def updateLocalStatus(finishedExpID):
    output("# update local status")
    output("write-file {0} {1}".format(LOCAL_MASTER_STATUS_FILE, finishedExpID))
    output("")


def stopAll():
    output("# stop all")
    output("stop __all__")
    output("wait for {0}".format(STOP_SLAVES_DELAY))


def writeReadyFile():
    output("write-file $ready_file READY")
    output("")


def writeLocalReadyFile():
    output("write-file master-ready READY")
    output("")


def generateCommands(expID, peers, clients):
    output("#========================================")
    output("# run {0}".format(expID))
    output("#========================================")
    output("")

    config = peers.copy()
    config.update(clients)

    configFiles = {key: val[0] for key, val in config.items()}
    bandwidths = {key: val[1] for key, val in config.items()}

    slaves = list(peers) + list(clients)

    waitForSlaves(slaves)
    createLogDir(expID)
    pushConfigFiles(expID, configFiles)
    setBandwidth(expID, bandwidths)
    startPeers(expID, list(peers))
    runClients(expID, list(clients))
    stopPeers(list(peers))
    unsetBandwidth(expID, bandwidths)
    saveConfig(expID, slaves)
    submitLogs(expID, slaves)
    updateStatus(expID)

    output("")


def generateOldMirCommands(expID, peers, clients):
    output("#========================================")
    output("# run {0} oldmir".format(expID))
    output("#========================================")
    output("")

    config = peers.copy()
    config.update(clients)

    configFiles = {key: val[0] for key, val in config.items()}
    bandwidths = {key: val[1] for key, val in config.items()}

    slaves = list(peers) + list(clients)

    waitForSlaves(slaves)
    createLogDir(expID)
    pushConfigFiles(expID, configFiles)
    setBandwidth(expID, bandwidths)
    generateOldMirConfig(expID, list(peers), list(clients))
    runOldMirClients(expID, list(clients))
    stopPeers(list(peers))
    unsetBandwidth(expID, bandwidths)
    saveConfig(expID, slaves)
    submitLogs(expID, slaves)
    updateStatus(expID)

    output("")


def generateLocalCommands(expID, peers, clients):
    output("#========================================")
    output("# run {0} local".format(expID))
    output("#========================================")
    output("")

    config = peers.copy()
    config.update(clients)

    configFiles = {key: val[0] for key, val in config.items()}
    bandwidths = {key: val[1] for key, val in config.items()}

    slaves = list(peers) + list(clients)

    waitForSlaves(slaves)
    createLocalLogDir(expID)
    pushLocalConfigFiles(expID, configFiles)
    startLocalPeers(expID, list(peers))
    runLocalClients(expID, list(clients))
    stopPeers(list(peers))
    updateLocalStatus(expID)

    output("")


def deploy(tokens):
    global defaultMachine
    global deploymentSchedule
    global numSlaves
    global lastFinished

    machine = defaultMachine

    while tokens:
        if tokens[0] == "machine:":
            machine = tokens[1]
            tokens = tokens[2:]
        else:
            if machine != "":
                n = int(tokens[0])
                tag = tokens[1]
                templateFile = machine

                numSlaves[tag] += n
                deploymentSchedule.append((lastFinished, n, tag, templateFile))
            else:
                sys.exit(
                    "ic-parse-experiment.py: deploy: must specify machine template before token '{0}'".format(
                        tokens[0]
                    )
                )
            tokens = tokens[2:]


def run(expID, tokens):
    global lastFinished
    global idOffset
    global experimentIdDigits
    global skipAllExisting

    runOldMir = False
    if tokens[0] == "oldmir":
        runOldMir = True
        tokens = tokens[1:]

    if expID == "next":
        expID = ("{:0" + str(experimentIdDigits) + "d}").format(idOffset)
        idOffset += 1
    else:
        experimentIdDigits = len(expID)
        idOffset = int(expID) + 1

    skip = False
    outdir = "{0}/experiment-output/{1}".format(local_exp_data, expID)

    if os.path.isdir(outdir) and skipAllExisting:
        skip = True
    elif os.path.isdir(outdir):
        sys.stderr.write("{0} already exists. (S)kip / skip (A)ll / (C)ancel? : ".format(outdir))
        sys.stderr.flush()
        answer = sys.stdin.readline().strip()
        while answer not in {"s", "S", "a", "A", "c", "C"}:
            sys.stderr.write("Please answer a, s, or c : ")
            sys.stderr.flush()
            answer = sys.stdin.readline().strip()

        if answer in {"s", "S"}:
            skip = True
        elif answer in {"a", "A"}:
            skip = True
            skipAllExisting = True
        elif answer in {"c", "C"}:
            os.exit("User abort.")

    clients = {}
    peers = {}
    config = defaultConfig
    bandwidth = defaultBandwidth
    role = None

    while tokens:
        if tokens[0] == "config:":
            config = tokens[1]
            tokens = tokens[2:]
        if tokens and tokens[0] == "bandwidth:":
            bandwidth = tokens[1]
            tokens = tokens[2:]
        elif tokens[0] == "peers:":
            role = peers
            tokens = tokens[1:]
        elif tokens[0] == "clients:":
            role = clients
            tokens = tokens[1:]
        else:
            if config != "" and role is not None:
                configFile = "{0}/{1}/{2}".format(local_exp_data, local_config_dir, config)
                if os.path.isfile(configFile):
                    role[tokens[0]] = (config, bandwidth)
                else:
                    sys.exit(
                        "ic-parse-experiment.py: config file not found: {0}".format(
                            configFile
                        )
                    )
            else:
                sys.exit(
                    "ic-parse-experiment.py: run {0}: must specify role and config before token '{1}'".format(
                        expID, tokens[0]
                    )
                )
            tokens = tokens[1:]

    if not skip:
        if runOldMir:
            generateOldMirCommands(expID, peers, clients)
        elif deplType in {"cloud", "remote"}:
            generateCommands(expID, peers, clients)
        elif deplType == "local":
            generateLocalCommands(expID, peers, clients)
        else:
            sys.exit(
                "generate-master-commands.py: unknown deployment type: {0} only know 'local' and 'cloud'".format(
                    deplType
                )
            )

    lastFinished = expID


def printDeploymentSchedule():
    for expID, n, tag, templateFile in deploymentSchedule:
        print("{0} {1} {2} {3}".format(expID, n, tag, templateFile))


# ============================================================
# main
# ============================================================

deplType = sys.argv[1]
if deplType not in {"local", "cloud", "remote"}:
    sys.exit("generate-master-commands.py: first argument must be one of 'local', 'cloud', and 'remote'")

if deplType == "remote":
    MASTER_CONFIG_DIR = "experiment-config"
    MASTER_EXP_DIR = "iss/current-deployment-data"

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
            sys.exit("ic-parse-experiment.py: Unsupported command: {0}".format(tokens[0]))

output("#========================================")
output("# wrap up")
output("#========================================")
output("")
output("# wait all slaves")
waitForSlaves(numSlaves.keys())

stopAll()

printDeploymentSchedule()

outFile.close()

