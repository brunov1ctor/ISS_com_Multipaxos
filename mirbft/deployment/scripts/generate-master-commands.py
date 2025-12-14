#!/usr/bin/env python3

import os
import re
import sys

# A master command file is the input to the discoverymaster (in master mode).
# Each line defines an action to be executed by a set of slaves or by the master itself.
#
# IMPORTANT NOTE:
#   discoveryslave executes commands using execve() (no shell).
#   Therefore, commands that are helpers in deployment/scripts MUST be referenced
#   with an explicit path containing '/' (e.g., "scripts/stubborn-scp.sh"), otherwise
#   the binary must exist in the slave process $PATH.
#
#   This file generates command lines for config fetch and log submission using:
#     scripts/stubborn-scp.sh
#   which is relative to the slave workdir (remote_work_dir), avoiding PATH issues.

MASTER_IP = "172.20.6.3"
MASTER_WORK_DIR = "/users/Bruno/iss"
MASTER_CONFIG_DIR = f"{MASTER_WORK_DIR}/experiment-config"
MASTER_EXP_DIR = f"{MASTER_WORK_DIR}/current-deployment-data"

LOCAL_IP_ADDRESS = "172.20.6.3"
LOCAL_MASTER_PORT = 9999
LOCAL_MASTER_STATUS_FILE = "status"

SLAVE_CONFIG_FILE = "config/config.yml"

# Delays (ms)
WAIT_FOR_SLAVES_DELAY = 2000
STOP_SLAVES_DELAY = 2000
SIGNAL_DELAY = 5000

# Bandwidth control
BANDWIDTH_DELAY = 2000

# Client timeout (ms)
CLIENT_TIMEOUT = 240000

# scp retries
SCP_RETRY_COUNT = 10

outFile = None

deploymentSchedule = []
numSlaves = {}
lastFinished = -1
skipAllExisting = False


def output(line):
    outFile.write(line + "\n")


def waitForSlaves(slaves):
    output("# Wait for slaves.")
    for s in slaves:
        output("wait-for {0} {1}".format(s, WAIT_FOR_SLAVES_DELAY))
    output("")


def createLogDir(expID):
    output("# Create experiment log dirs.")
    for i in range(0, 1000):
        output("exec-start __all__ /dev/null mkdir -p experiment-output/{0}/slave-__id__".format(expID))
        output("exec-wait __all__ 2000")
        break
    output("")


def createLocalLogDir(expID):
    output("# Create experiment log dirs (local).")
    output("exec-start __all__ /dev/null mkdir -p experiment-output/{0}/slave-__id__".format(expID))
    output("exec-wait __all__ 2000")
    output("")


def addPreflightLogs(expID, slaves):
    output("# Preflight debug logs (env/bin/paths).")
    for s in slaves:
        output(f"exec-start {s} experiment-output/{expID}/slave-__id__/whoami.log whoami")
        output(f"exec-start {s} experiment-output/{expID}/slave-__id__/hostname.log hostname")
        output(f"exec-start {s} experiment-output/{expID}/slave-__id__/pwd.log pwd")
        output(f"exec-start {s} experiment-output/{expID}/slave-__id__/env-gopath.log printenv GOPATH")
        output(f"exec-start {s} experiment-output/{expID}/slave-__id__/env-path.log printenv PATH")
        output(f"exec-start {s} experiment-output/{expID}/slave-__id__/env-masterport.log printenv master_port")
        output(f"exec-start {s} experiment-output/{expID}/slave-__id__/env-ownip.log printenv own_public_ip")
        output(f"exec-start {s} experiment-output/{expID}/slave-__id__/ls-users-go-bin.log ls -la /users/$USER/go/bin")
        output(f"exec-start {s} experiment-output/{expID}/slave-__id__/ls-gopath-bin.log ls -la $GOPATH/bin")
        output(f"exec-start {s} experiment-output/{expID}/slave-__id__/ls-config-dir.log ls -la config")
        output(f"exec-start {s} experiment-output/{expID}/slave-__id__/ls-config-yml.log ls -la config/config.yml")
    output("exec-wait __all__ 20000")
    output("")


def pushConfigFiles(expID, slaves):
    output("# Push config files.")
    for s, configFile in slaves.items():
        output(
            "exec-start {0} scp-output-{1}-config.log scripts/stubborn-scp.sh {5} "
            "$own_public_ip:{2}/{3} {4}".format(
                s, expID, MASTER_CONFIG_DIR, configFile, SLAVE_CONFIG_FILE, SCP_RETRY_COUNT
            )
        )

    for s in slaves:
        output(
            "exec-wait {0} 60000 "
            "exec-start {0} experiment-output/{1}/slave-__id__/FAILED echo Could not fetch config; "
            "exec-wait {0} 2000".format(s, expID)
        )

    for s in slaves:
        output("sync {0}".format(s))

    # post-check
    for s in slaves:
        output(f"exec-start {s} experiment-output/{expID}/slave-__id__/post-config-ls.log ls -la {SLAVE_CONFIG_FILE}")
        output(f"exec-start {s} experiment-output/{expID}/slave-__id__/post-config-head.log head -n 40 {SLAVE_CONFIG_FILE}")
        output(
            "exec-wait {0} 2000 "
            "exec-start {0} experiment-output/{1}/slave-__id__/FAILED echo Could not read config; "
            "exec-wait {0} 2000".format(s, expID)
        )

    output("")


def pushLocalConfigFiles(expID, slaves):
    output("# Push config files (local).")
    for s, configFile in slaves.items():
        output(
            "exec-start {0} /dev/null cp {2}/{3} {4}".format(
                s, expID, "config", configFile, SLAVE_CONFIG_FILE
            )
        )
        output(
            "exec-wait {0} 2000 "
            "exec-start {0} experiment-output/{1}/slave-__id__/FAILED echo Could not fetch config; "
            "exec-wait {0} 2000".format(s, expID)
        )
    output("")


def setBandwidth(expID, bandwidths):
    output("# Set bandwidth.")
    for s, bandwidth in bandwidths.items():
        if bandwidth != "unlimited":
            output(
                "exec-start {0} /dev/null tc qdisc add dev eth1 root tbf rate {1} burst 32kbit latency 400ms".format(
                    s, bandwidth
                )
            )
    output("exec-wait __all__ {0}".format(BANDWIDTH_DELAY))
    output("")


def unsetBandwidth(expID, bandwidths):
    output("# Unset bandwidth.")
    for s, bandwidth in bandwidths.items():
        if bandwidth != "unlimited":
            output("exec-start {0} /dev/null tc qdisc del dev eth1 root || true".format(s))
    output("exec-wait __all__ {0}".format(BANDWIDTH_DELAY))
    output("")


def startPeers(expID, peers):
    output("# Start peers.")
    output("discover-reset")
    for p in peers:
        output(f"exec-start {p} experiment-output/{expID}/slave-__id__/peer-pre.log printenv master_port")
        output(f"exec-start {p} experiment-output/{expID}/slave-__id__/peer-pre.log printenv own_public_ip")
        output(f"exec-start {p} experiment-output/{expID}/slave-__id__/peer-pre.log ls -la orderingpeer")

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


def startLocalPeers(expID, peers):
    output("# Start peers (local).")
    output("discover-reset")
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
            "exec-start {0} scp-output-{1}-logs.log scripts/stubborn-scp.sh {2} "
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

