import os.path
import sys
from collections import defaultdict

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
    output("# Wait for slaves.")
    for s in slaves:
        output("wait for slaves {0} {1}".format(s, numSlaves[s]))
    output("")


def createLogDir(expID):
    output("# Create log directory.")
    output("exec-start __all__ /dev/null mkdir -p experiment-output/{0}/slave-__id__/config".format(expID))
    output("exec-start __all__ /dev/null mkdir -p experiment-output/{0}/slave-__id__/prof".format(expID))
    output("exec-start __all__ /dev/null mkdir -p experiment-output/{0}/slave-__id__/prof-client".format(expID))
    output("exec-start __all__ /dev/null mkdir -p experiment-output/{0}/slave-__id__/client".format(expID))
    output("exec-wait __all__ 2000")
    output("")


def createLocalLogDir(expID):
    output("# Create log directory (local).")
    output("exec-start __all__ /dev/null mkdir -p experiment-output/{0}/slave-__id__/config".format(expID))
    output("exec-start __all__ /dev/null mkdir -p experiment-output/{0}/slave-__id__/prof".format(expID))
    output("exec-start __all__ /dev/null mkdir -p experiment-output/{0}/slave-__id__/prof-client".format(expID))
    output("exec-start __all__ /dev/null mkdir -p experiment-output/{0}/slave-__id__/client".format(expID))
    output("exec-wait __all__ 2000")
    output("")


def addPreflightLogs(expID, slaves):
    """
    IMPORTANT:
    Keep commands SIMPLE (no nested quotes), because the discoverymaster
    command parser tokenizes arguments.
    """
    output("# Preflight logs (debug).")
    for s in slaves:
        # basic identity
        output("exec-start {0} experiment-output/{1}/slave-__id__/whoami.log whoami".format(s, expID))
        output("exec-start {0} experiment-output/{1}/slave-__id__/hostname.log hostname".format(s, expID))
        output("exec-start {0} experiment-output/{1}/slave-__id__/pwd.log pwd".format(s, expID))

        # env essentials
        output("exec-start {0} experiment-output/{1}/slave-__id__/env-gopath.log printenv GOPATH".format(s, expID))
        output("exec-start {0} experiment-output/{1}/slave-__id__/env-path.log printenv PATH".format(s, expID))
        output("exec-start {0} experiment-output/{1}/slave-__id__/env-masterport.log printenv master_port".format(s, expID))
        output("exec-start {0} experiment-output/{1}/slave-__id__/env-ownip.log printenv own_public_ip".format(s, expID))

        # binaries visibility (try common locations; no quotes)
        output("exec-start {0} experiment-output/{1}/slave-__id__/ls-users-go-bin.log ls -la /users/$USER/go/bin".format(s, expID))
        output("exec-start {0} experiment-output/{1}/slave-__id__/ls-gopath-bin.log ls -la $GOPATH/bin".format(s, expID))

        # config presence (will be useful after pushConfigFiles too, but harmless here)
        output("exec-start {0} experiment-output/{1}/slave-__id__/ls-config-dir.log ls -la config".format(s, expID))
        output("exec-start {0} experiment-output/{1}/slave-__id__/ls-config-yml.log ls -la {2}".format(s, expID, SLAVE_CONFIG_FILE))

        output("exec-wait {0} 8000".format(s))

    for s in slaves:
        output("sync {0}".format(s))
    output("")


def pushConfigFiles(expID, slaves):
    output("# Push config files.")
    for s, configFile in slaves.items():
        output(
            "exec-start {0} scp-output-{1}-config.log stubborn-scp.sh {5} -i $ssh_key_file "
            "$own_public_ip:{2}/{3} {4}".format(
                s, expID, MASTER_CONFIG_DIR, configFile, SLAVE_CONFIG_FILE, SCP_RETRY_COUNT
            )
        )
        output(
            "exec-wait {0} 60000 "
            "exec-start {0} experiment-output/{1}/slave-__id__/FAILED echo Could not fetch config; "
            "exec-wait {0} 2000".format(s, expID)
        )

        # after copying, log what actually arrived
        output("exec-start {0} experiment-output/{1}/slave-__id__/post-config-ls.log ls -la {2}".format(
            s, expID, SLAVE_CONFIG_FILE
        ))
        output("exec-start {0} experiment-output/{1}/slave-__id__/post-config-head.log head -n 40 {2}".format(
            s, expID, SLAVE_CONFIG_FILE
        ))
        output("exec-wait {0} 5000".format(s))

    for s in slaves:
        output("sync {0}".format(s))
    output("")


def pushLocalConfigFiles(expID, slaves):
    output("# Prepare config file (local).")
    for s, configFile in slaves.items():
        output(
            "exec-start {0} /dev/null cp {1}/{2} {3}".format(
                s, local_config_dir, configFile, SLAVE_CONFIG_FILE
            )
        )
        output("exec-wait {0} 2000".format(s))
        output("exec-start {0} experiment-output/{1}/slave-__id__/post-config-ls.log ls -la {2}".format(
            s, expID, SLAVE_CONFIG_FILE
        ))
        output("exec-start {0} experiment-output/{1}/slave-__id__/post-config-head.log head -n 40 {2}".format(
            s, expID, SLAVE_CONFIG_FILE
        ))
        output("exec-wait {0} 5000".format(s))

    for s in slaves:
        output("sync {0}".format(s))
    output("")


def setBandwidth(expID, bandwidths):
    output("# Set bandwidth limits.")
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
    output("# Unset bandwidth limits.")
    for s, bandwidth in bandwidths.items():
        if bandwidth != "0" and bandwidth != "unlimited":
            output(
                "exec-start {0} unset-bandwidth-{1}.log tc qdisc del dev eth0 root tbf"
                "".format(s, expID)
            )
            output(
                "exec-wait {0} 2000 "
                "exec-start {0} experiment-output/{1}/slave-__id__/FAILED echo Could not unset bandwidth; "
                "exec-wait {0} 2000".format(s, expID)
            )
    for s in bandwidths:
        output("sync {0}".format(s))
    output("")


def startPeers(expID, peers):
    numPeers = sum(numSlaves[p] for p in peers)

    output("# Start peers.")
    output("discover-reset {0}".format(numPeers))

    for p in peers:
        # log the exact invocation and key vars
        output("exec-start {0} experiment-output/{1}/slave-__id__/peer-pre.log printenv master_port".format(p, expID))
        output("exec-start {0} experiment-output/{1}/slave-__id__/peer-pre.log printenv own_public_ip".format(p, expID))
        output("exec-start {0} experiment-output/{1}/slave-__id__/peer-pre.log ls -la orderingpeer".format(p, expID))

        output(
            "exec-start {0} experiment-output/{1}/slave-__id__/peer.log orderingpeer "
            "{2} $own_public_ip:$master_port __public_ip__ __private_ip__ "
            "experiment-output/{1}/slave-__id__/peer.trc experiment-output/{1}/slave-__id__/prof".format(
                p, expID, SLAVE_CONFIG_FILE
            )
        )

    output("discover-wait")
    output("")


def startLocalPeers(expID, peers):
    numPeers = sum(numSlaves[p] for p in peers)

    output("# Start peers (local).")
    output("discover-reset {0}".format(numPeers))

    for p in peers:
        output(
            "exec-start {0} experiment-output/{1}/slave-__id__/peer.log orderingpeer "
            "{2} {3}:{4} {3} {3} "
            "experiment-output/{1}/slave-__id__/peer.trc experiment-output/{1}/slave-__id__/prof".format(
                p, expID, SLAVE_CONFIG_FILE, LOCAL_IP_ADDRESS, LOCAL_MASTER_PORT
            )
        )

    output("discover-wait")
    output("")


def runClients(expID, clients):
    output("# Run clients and wait for them to stop.")

    for c in clients:
        # log before start
        output("exec-start {0} experiment-output/{1}/slave-__id__/client-pre.log printenv master_port".format(c, expID))
        output("exec-start {0} experiment-output/{1}/slave-__id__/client-pre.log printenv own_public_ip".format(c, expID))
        output("exec-start {0} experiment-output/{1}/slave-__id__/client-pre.log ls -la orderingclient".format(c, expID))

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


def stopPeers(peers):
    output("# Stop peers.")
    for p in peers:
        output("exec-signal {0} SIGINT".format(p))
    output("wait for {0}".format(SIGNAL_DELAY))
    output("")


def saveConfig(expID, slaves):
    output("# Save config file.")
    for s in slaves:
        output("exec-start {0} /dev/null cp {1} experiment-output/{2}/slave-__id__".format(
            s, SLAVE_CONFIG_FILE, expID
        ))
        output(
            "exec-wait {0} 2000 "
            "exec-start {0} experiment-output/{1}/slave-__id__/FAILED echo Could not log config file; "
            "exec-wait {0} 2000".format(s, expID)
        )
    output("")


def submitLogs(expID, slaves):
    output("# Submit logs to master node")
    for s in slaves:
        output(
            "exec-start {0} /dev/null tar czf experiment-output-{1}-slave-__id__.tar.gz "
            "experiment-output/{1}/slave-__id__".format(s, expID)
        )
        output(
            "exec-wait {0} 30000 "
            "exec-start {0} experiment-output/{1}/slave-__id__/FAILED echo Could not compress logs; "
            "exec-wait {0} 2000".format(s, expID)
        )

    for s in slaves:
        output(
            "exec-start {0} scp-output-{1}-logs.log stubborn-scp.sh {2} -i $ssh_key_file "
            "experiment-output-{1}-slave-__id__.tar.gz $own_public_ip:{3}/raw-results/".format(
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
    output("write-file {0} {1}".format(LOCAL_MASTER_STATUS_FILE, finishedExpID))
    output("")


def stopAll():
    output("# Stop all slaves.")
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
    output("# {0}".format(expID))
    output("#========================================")
    output("\n")

    config = peers.copy()
    config.update(clients)

    configFiles = {key: val[0] for key, val in config.items()}
    bandwidths = {key: val[1] for key, val in config.items()}
    slaves = list(peers) + list(clients)

    waitForSlaves(slaves)
    createLogDir(expID)

    # Preflight BEFORE config push (helps debug env/bin paths)
    addPreflightLogs(expID, slaves)

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


def generateLocalCommands(expID, peers, clients):
    output("#========================================")
    output("# {0} (local)".format(expID))
    output("#========================================")
    output("\n")

    config = peers.copy()
    config.update(clients)

    configFiles = {key: val[0] for key, val in config.items()}
    slaves = list(peers) + list(clients)

    waitForSlaves(slaves)
    createLocalLogDir(expID)
    pushLocalConfigFiles(expID, configFiles)
    startLocalPeers(expID, list(peers))

    # local uses orderingclient too
    runClients(expID, list(clients))
    stopPeers(list(peers))
    updateLocalStatus(expID)
    output("")


def deploy(tokens):
    global defaultMachine, deploymentSchedule, numSlaves, lastFinished
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
                sys.exit("generate-master-commands.py: deploy: must specify machine template before token '{0}'".format(tokens[0]))
            tokens = tokens[2:]


def run(expID, tokens):
    global lastFinished, idOffset, experimentIdDigits, skipAllExisting

    if expID == "next":
        expID = ("{:0" + str(experimentIdDigits) + "d}").format(idOffset)
        idOffset += 1
    else:
        experimentIdDigits = len(expID)
        idOffset = int(expID) + 1

    clients = {}
    peers = {}
    config = defaultConfig
    bandwidth = defaultBandwidth
    role = None

    while tokens:
        if tokens[0] == "config:":
            config = tokens[1]; tokens = tokens[2:]; continue
        if tokens[0] == "bandwidth:":
            bandwidth = tokens[1]; tokens = tokens[2:]; continue
        if tokens[0] == "peers:":
            role = peers; tokens = tokens[1:]; continue
        if tokens[0] == "clients:":
            role = clients; tokens = tokens[1:]; continue

        if config != "" and role is not None:
            configFile = "{0}/{1}/{2}".format(local_exp_data, local_config_dir, config)
            if os.path.isfile(configFile):
                role[tokens[0]] = (config, bandwidth)
            else:
                sys.exit("generate-master-commands.py: config file not found: {0}".format(configFile))
        else:
            sys.exit("generate-master-commands.py: run {0}: must specify role and config before token '{1}'".format(expID, tokens[0]))

        tokens = tokens[1:]

    if deplType in {"cloud", "remote"}:
        generateCommands(expID, peers, clients)
    elif deplType == "local":
        generateLocalCommands(expID, peers, clients)
    else:
        sys.exit("generate-master-commands.py: unknown deployment type: {0}".format(deplType))

    lastFinished = expID


def printDeploymentSchedule():
    for expID, n, tag, templateFile in deploymentSchedule:
        print("{0} {1} {2} {3}".format(expID, n, tag, templateFile))


# ================= main =================

deplType = sys.argv[1]
if deplType not in {"local", "cloud", "remote"}:
    sys.exit("generate-master-commands.py: first argument must be one of 'local', 'cloud', and 'remote'")

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
            sys.exit("generate-master-commands.py: Unsupported command: {0}".format(tokens[0]))

output("#========================================")
output("# Wrap up                                ")
output("#========================================")
output("")
output("# Wait for all slaves, even if they were not involved in experiments.")
waitForSlaves(numSlaves.keys())
stopAll()
printDeploymentSchedule()
outFile.close()

