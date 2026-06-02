# mirbft/deployment/scripts/generate-master-commands.py
#!/usr/bin/env python3
import os
import sys
from collections import defaultdict

CLIENT_TIMEOUT = 480000  # ms
SIGNAL_DELAY = "5s"
STOP_SLAVES_DELAY = "3s"
SCP_RETRY_COUNT = "10"

CLIENT_TIMEOUT = 480000  # ms
SIGNAL_DELAY = "5s"
STOP_SLAVES_DELAY = "3s"
SCP_RETRY_COUNT = "10"

BASE_DIR = os.environ.get(
    "ISS_BASE_DIR",
    f"/users/{os.environ.get('USER', 'user')}/iss",
)

# CORREÇÃO: usar remote_work_dir consistente com start-slave.sh
_DEFAULT_USER = os.environ.get("USER", "user")
REMOTE_WORK_DIR_DEFAULT = f"/tmp/iss-{_DEFAULT_USER}"

EXPERIMENT_OUTPUT_DIR = os.environ.get(
    "ISS_EXPERIMENT_OUTPUT_DIR",
    f"{os.environ.get('ISS_REMOTE_WORK_DIR', REMOTE_WORK_DIR_DEFAULT)}/experiment-output",
)

MASTER_CONFIG_DIR = f"{BASE_DIR}/experiment-config"
MASTER_EXP_DIR = BASE_DIR

# Tudo absoluto nos slaves
SLAVE_CONFIG_FILE = f"{BASE_DIR}/config/config.yml"

LOCAL_MASTER_STATUS_FILE = "master-status"
LOCAL_IP_ADDRESS = "127.0.0.1"
LOCAL_MASTER_PORT = "9999"
FS_SETTLE_DELAY_MS = 2000
BEST_EFFORT_WAIT_MS = 15000
BEST_EFFORT_TAR_WAIT_MS = 120000
BEST_EFFORT_SCP_WAIT_MS = 180000

# (ALTERAÇÃO MÍNIMA) timeouts mais realistas p/ remote scp + FS lento
FETCH_CONFIG_WAIT_MS = 180000   # era 60000
VERIFY_CONFIG_WAIT_MS = 60000   # era 2000

# REMOTE_WORK_DIR já definido acima para EXPERIMENT_OUTPUT_DIR
REMOTE_WORK_DIR = os.environ.get("ISS_REMOTE_WORK_DIR", REMOTE_WORK_DIR_DEFAULT)
REMOTE_USER = os.environ.get("ISS_REMOTE_USER", _DEFAULT_USER)

REMOTE_BIN_DIR = os.environ.get("ISS_REMOTE_BIN_DIR", f"/users/{REMOTE_USER}/go/bin")
ORDERINGPEER_BIN = os.path.join(REMOTE_BIN_DIR, "orderingpeer")
ORDERINGCLIENT_BIN = os.path.join(REMOTE_BIN_DIR, "orderingclient")

REMOTE_TLS_LINK = f"{REMOTE_WORK_DIR}/tls-data"
REMOTE_TLS_SRC = f"{BASE_DIR}/tls-data"

lastFinished = -1
deploymentSchedule = []
numSlaves = defaultdict(int)
skipAllExisting = False


def output(data: str):
    print(data, file=outFile)

def waitForSlaves(slaves):
    output("# wait slaves")
    for s in slaves:
        output("wait for slaves {0} {1}".format(s, numSlaves[s]))
    output("")

def _exp_slave_dir(expID: str) -> str:
    return f"{EXPERIMENT_OUTPUT_DIR}/{expID}/slave-__id__"

def createLogDir(expID):
    output("# mkdir log dir")
    # Use '-' as output sink: the slave will discard stdout/stderr without creating any file.
    output("exec-start __all__ - mkdir -p {0}".format(_exp_slave_dir(expID)))
    output("exec-wait __all__ {0}".format(FS_SETTLE_DELAY_MS))
    output("")

def ensureConfigDir(slaves):
    output("# ensure config dir")
    for s in slaves:
        output("exec-start {0} - mkdir -p {1}/config".format(s, BASE_DIR))
    for s in slaves:
        output("exec-wait {0} {1}".format(s, FS_SETTLE_DELAY_MS))
    for s in slaves:
        output("sync {0}".format(s))
    output("")

def pushConfigFiles(expID, slaves):
    output("# push config")
    for s, configFile in slaves.items():
        dest = SLAVE_CONFIG_FILE

        # ALTERAÇÃO MÍNIMA:
        # O erro estava em usar "-i $ssh_key_file" (no slave esse env não existe -> vira vazio,
        # e o scp trata o SOURCE como arquivo de chave).
        # Então: scp direto, SEM -i.
        scp_opts = "-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=10"

        scp_cmd = (
            "exec-start {0} - scp {1} "
            "$own_public_ip:{2}/{3} {4}"
        )

        output(
            scp_cmd.format(
                s,
                scp_opts,
                MASTER_CONFIG_DIR,
                configFile,
                dest,
            )
        )

        # (ALTERAÇÃO MÍNIMA) era 60000
        output(
            "exec-wait {0} {3} "
            "exec-start {0} {1}/FAILED echo Could not fetch config; "
            "exec-wait {0} {2}".format(
                s, _exp_slave_dir(expID), FS_SETTLE_DELAY_MS, FETCH_CONFIG_WAIT_MS
            )
        )

    output("# verify config arrived")
    for s in slaves:
        output("exec-start {0} - test -s {1}".format(s, SLAVE_CONFIG_FILE))
        # (ALTERAÇÃO MÍNIMA) era 2000
        output(
            "exec-wait {0} {3} "
            "exec-start {0} {1}/FAILED echo Config missing after fetch; "
            "exec-wait {0} {2}".format(
                s, _exp_slave_dir(expID), FS_SETTLE_DELAY_MS, VERIFY_CONFIG_WAIT_MS
            )
        )

    for s in slaves:
        output("sync {0}".format(s))
    output("")

def snapshotConfigNow(expID, slaves):
    output("# snapshot config (per-run)")
    for s in slaves:
        # garante o diretório do slave ANTES do cp
        output("exec-start {0} - mkdir -p {1}".format(s, _exp_slave_dir(expID)))
        output(
            "exec-wait {0} 60000 "
            "exec-start {0} {1}/FAILED echo Could not mkdir snapshot dir; "
            "exec-wait {0} {2}".format(s, _exp_slave_dir(expID), FS_SETTLE_DELAY_MS)
        )

        # agora o snapshot
        output(
            "exec-start {0} - cp {1} {2}/config.yml".format(
                s, SLAVE_CONFIG_FILE, _exp_slave_dir(expID)
            )
        )
        output(
            "exec-wait {0} 60000 "
            "exec-start {0} {1}/FAILED echo Could not snapshot config; "
            "exec-wait {0} {2}".format(s, _exp_slave_dir(expID), FS_SETTLE_DELAY_MS)
        )
    for s in slaves:
        output("sync {0}".format(s))
    output("")

def ensureTlsDataLink(expID, slaves):
    """
    Cria/garante o link ABSOLUTO esperado:
      <remote_work_dir>/tls-data  ->  <base_dir>/tls-data

    Mantém comandos simples (sem bash -lc) para evitar parsing/quoting problemático.
    """
    output("# ensure tls-data link (ABSOLUTO, sem bash -lc)")
    for s in slaves:
        # garante o work dir
        output("exec-start {0} - mkdir -p {1}".format(s, REMOTE_WORK_DIR))
        output(
            "exec-wait {0} 5000 "
            "exec-start {0} {1}/FAILED echo Could not mkdir work dir; "
            "exec-wait {0} {2}".format(s, _exp_slave_dir(expID), FS_SETTLE_DELAY_MS)
        )

        # remove link/dir antigo (se existir)
        output("exec-start {0} - rm -rf {1}".format(s, REMOTE_TLS_LINK))
        output(
            "exec-wait {0} 5000 "
            "exec-start {0} {1}/FAILED echo Could not remove old tls-data; "
            "exec-wait {0} {2}".format(s, _exp_slave_dir(expID), FS_SETTLE_DELAY_MS)
        )

        # cria symlink absoluto
        output("exec-start {0} - ln -s {1} {2}".format(s, REMOTE_TLS_SRC, REMOTE_TLS_LINK))
        output(
            "exec-wait {0} 5000 "
            "exec-start {0} {1}/FAILED echo Could not create tls-data link; "
            "exec-wait {0} {2}".format(s, _exp_slave_dir(expID), FS_SETTLE_DELAY_MS)
        )

        # valida link
        output("exec-start {0} - test -e {1}".format(s, REMOTE_TLS_LINK))
        output(
            "exec-wait {0} 2000 "
            "exec-start {0} {1}/FAILED echo tls-data link missing; "
            "exec-wait {0} {2}".format(s, _exp_slave_dir(expID), FS_SETTLE_DELAY_MS)
        )

    for s in slaves:
        output("sync {0}".format(s))
    output("")

def setBandwidth(expID, bandwidths):
    output("# set bandwidth")
    for s, bandwidth in bandwidths.items():
        if bandwidth != "0" and bandwidth != "unlimited":
            output(
                "exec-start {0} set-bandwidth-{1}.log tc qdisc add dev eth0 root tbf rate {2} burst 320kbit latency 400ms".format(
                    s, expID, bandwidth
                )
            )
            output(
                "exec-wait {0} 2000 "
                "exec-start {0} {1}/FAILED echo Could not set bandwidth; "
                "exec-wait {0} {2}".format(s, _exp_slave_dir(expID), FS_SETTLE_DELAY_MS)
            )
    for s in bandwidths:
        output("sync {0}".format(s))
    output("")

def unsetBandwidth(expID, bandwidths):
    output("# unset bandwidth")
    for s, bandwidth in bandwidths.items():
        if bandwidth != "0" and bandwidth != "unlimited":
            output(
                "exec-start {0} unset-bandwidth-{1}.log tc qdisc del dev eth0 root tbf".format(
                    s, expID
                )
            )
            output("exec-wait {0} 5000".format(s))
    for s in bandwidths:
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
            f"exec-start {p} {_exp_slave_dir(expID)}/peer.log {ORDERINGPEER_BIN} "
            f"{SLAVE_CONFIG_FILE} $own_public_ip:$master_port __public_ip__ __private_ip__ "
            f"{_exp_slave_dir(expID)}/peer.trc {_exp_slave_dir(expID)}/prof"
        )
    output("discover-wait")
    output("")

def runClients(expID, clients):
    output("# run clients")
    for c in clients:
        output(
            f"exec-start {c} {_exp_slave_dir(expID)}/clients.log {ORDERINGCLIENT_BIN} "
            f"{SLAVE_CONFIG_FILE} $own_public_ip:$master_port "
            f"{_exp_slave_dir(expID)}/client {_exp_slave_dir(expID)}/prof-client"
        )

    timeout = CLIENT_TIMEOUT
    for c in clients:
        output(
            "exec-wait {0} {2} "
            "exec-start {0} {1}/FAILED echo Client failed or timed out; "
            "exec-wait {0} {3}".format(c, _exp_slave_dir(expID), timeout, FS_SETTLE_DELAY_MS)
        )
        output("sync {0}".format(c))
    output("")

def stopPeers(peers):
    """Stop orderingpeer without terminating discoveryslave.
    
    Send SIGINT then exec-wait for the process to actually terminate.
    Timeout = CLIENT_TIMEOUT // 4 (peer shutdown should be much faster than
    the client run, but scales with experiment duration).
    """
    shutdown_timeout = max(CLIENT_TIMEOUT // 16, 15000)  # at least 15s
    output("# stop peers (send SIGINT, wait up to {}ms for graceful exit)".format(shutdown_timeout))
    for p in peers:
        output("exec-signal {0} SIGINT".format(p))
    for p in peers:
        output("exec-wait {0} {1}".format(p, shutdown_timeout))
    output("")

def saveConfig(expID, slaves):
    output("# Save config (best-effort, ensure dir exists)")
    for s in slaves:
        output("exec-start {0} - mkdir -p {1}".format(s, _exp_slave_dir(expID)))
    for s in slaves:
        output("exec-wait {0} {1}".format(s, BEST_EFFORT_WAIT_MS))

    for s in slaves:
        output(
            "exec-start {0} - cp {1} {2}/config.final.yml".format(
                s, SLAVE_CONFIG_FILE, _exp_slave_dir(expID)
            )
        )
    for s in slaves:
        output("exec-wait {0} {1}".format(s, BEST_EFFORT_WAIT_MS))

    for s in slaves:
        output("sync {0}".format(s))
    output("")

def submitLogs(expID, slaves):
    # DESATIVADO (não empacota/copiar logs pesados e não cria scp-output-*.log)
    output("# submit logs (DESATIVADO)")
    for s in slaves:
        output("exec-start {0} - true".format(s))
    output("")

def updateStatus(value: str):
    output("# update status")
    output("write-file $status_file {0}".format(value))
    output("")

def writeReadyFile():
    output("write-file $ready_file READY")
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

    ensureConfigDir(slaves)
    pushConfigFiles(expID, configFiles)
    snapshotConfigNow(expID, slaves)

    ensureTlsDataLink(expID, slaves)

    setBandwidth(expID, bandwidths)
    startPeers(expID, list(peers))
    runClients(expID, list(clients))
    stopPeers(list(peers))
    unsetBandwidth(expID, bandwidths)

    saveConfig(expID, slaves)
    submitLogs(expID, slaves)

    updateStatus(expID)
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
                    "generate-master-commands.py: deploy: must specify machine template before token '{0}'".format(
                        tokens[0]
                    )
                )
            tokens = tokens[2:]

def run(expID, tokens):
    global lastFinished
    global idOffset
    global experimentIdDigits
    global skipAllExisting

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
            sys.exit("User abort.")

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
                    sys.exit("generate-master-commands.py: config file not found: {0}".format(configFile))
            else:
                sys.exit(
                    "generate-master-commands.py: run {0}: must specify role and config before token '{1}'".format(
                        expID, tokens[0]
                    )
                )
            tokens = tokens[1:]

    if not skip:
        if deplType in {"cloud", "remote"}:
            generateCommands(expID, peers, clients)
        elif deplType == "local":
            pass
        else:
            sys.exit("generate-master-commands.py: unknown deployment type")

    lastFinished = expID

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
    output("write-file master-ready READY")
    output("")
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
            sys.exit("generate-master-commands.py: unknown token: {0}".format(tokens[0]))