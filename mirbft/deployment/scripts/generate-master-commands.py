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
    # output("sleep 5s")
    output("")


def stopAll():
    output("# Stop everything on the slaves.")
    output("stop __all__ stop all {0}".format(STOP_SLAVES_DELAY))
    output("sleep 2s")
    output("")


def idForExp(expID, expName, desc):
    global lastFinished

    thisFinished = desc.get("lastFinished", -1)
    thisID = desc.get("expID", expID)

    if thisFinished < lastFinished:
        raise ValueError("Inconsistent experiments. exp {0} must precede all exps with ID greater than or equal to {1}"
                         .format(lastFinished, thisID))

    lastFinished = thisFinished

    if thisFinished >= 0:
        deploymentSchedule.append({
            "id": thisID,
            "name": expName,
            "priority": desc.get("priority", 0),
            "repeat": desc.get("repeat", 1),
            "deadline": desc.get("deadline", "????-??-?? ??")
        })

    return thisID


def idFromTokens(tokens):
    global lastFinished, skipAllExisting

    if not tokens:
        # delta expID: skip this experiment
        return -1

    if tokens[0] == "*":
        skipAllExisting = True
        return -1

    thisID = int(tokens[0])

    if thisID <= lastFinished and skipAllExisting:
        # skip all past experiments
        return -1

    return thisID


def parseParts(tokens):
    parts = {}
    for t in tokens:
        words = t.split("=")
        if len(words) == 2:
            key = words[0]
            value = words[1]
            # parse list of values
            if key.endswith("s") and "," in value:
                parts[key] = value.split(",")
            else:
                parts[key] = words[1]
    return parts


def parseTimeout(timeout, default):
    timeout = timeout.lower()

    multipliers = {"ms": 1,
                   "s": 1000,
                   "m": 60 * 1000,
                   "h": 3600 * 1000}

    try:
        # get unit (last one or two letterms)
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


def parseNodes(data):
    results = []

    for s in data.split(","):
        s = s.strip()
        if "-" in s:
            tokens = [int(t.strip()) for t in s.split("-", 1)]
            s_id = tokens[0]

            if len(tokens) == 1:
                s_last = s_id
            else:
                s_last = tokens[1]

            for i in range(s_id, s_last+1):
                results.append(i)
        else:
            results.append(int(s))

    return sorted(list(set(results)))


def generateExpCommands(config, slaves, numNodes):
    '''
        exp: experiment-id
        exp-name: experiment-name
        exp-type: experiment-type
        master-nodes: main nodes will be created on all listed nodes
        nodes: all nodes that will participate in the experiment.
        client-nodes: nodes where clients are started. If it is set to __all__ clients
                      are started in all nodes.
    '''  # noqa
    global deploymentSchedule

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

        if parts.get("exp-type", "p2p") == "p2p":
            expType = "peers"
        else:
            expType = parts["exp-type"]

        output("#========================================")
        output("# SCRIPT FOR EXPERIMENT {0} ({1})".format(
               expID, desc["name"]))
        output("#========================================")
        output("")

        if "connect" in parts:
            conns = parseNodes(parts["connect"])
        else:
            conns = [0]

        if "nodes" in parts:
            nodes = [slaves[i] for i in parseNodes(parts["nodes"])]

            try:
                ndict = dict(
                    (slaves[i], i) for i in parseNodes(parts["nodes"]))
                conns = [ndict[slaves[i]] if slaves[i] in ndict else conns[0]
                         for i in conns]
            except Exception:
                conns = [0]
        else:
            nodes = slaves

        if "client-nodes" in parts:
            if parts["client-nodes"] == "__all__":
                clientNodes = slaves
            else:
                clientNodes = [slaves[i]
                               for i in parseNodes(parts["client-nodes"])]
        elif expType == "client":
            clientNodes = nodes
        else:
            clientNodes = []

        numNodes[expType] = len(nodes)
        # TODO: this is a lame naming convention.
        numNodes["instance"] = numNodes.get("instances", 0)

        output(
            "# EXPERIMENT {0} nodes: {1} {2} {3} {4}".format(
                expID, expType, nodes, clientNodes, conns
            )
        )

        output("# Prepare instance.")
        output("instance {0} node-id {1}".format(expID, expID))
        output("instance {0} set-main-nodes {1}".format(expID, " ".join(
            str(s) for s in nodes)))
        output("instance {0} set-other-nodes {1}".format(expID, " ".join(
            str(s) for s in clientNodes)))

        output("instance {0} set-connection assign {1}".format(
            expID,
            " ".join(
                ["{0},{1}".format(slaves[i], conns[i])
                 for i in range(len(nodes))])))

        if "nr-links" in parts:
            nrLinks = parseNodes(parts["nr-links"])
            detail = "*** Connections ***"
            expID, firstDetail = generateScenarioCommands(
                expID, slaves, numNodes, nrLinks, conns, "set-connections")
            if firstDetail:
                detail = "*** Connection details for experiment 0 ***"
            output(detail)

        output("")
        output("instance {0} set-log-dir logs/LOG-{0}".format(expID))
        output("instance {0} set-terminate-on-kill true".format(expID))
        output("instance {0} set-signal-delay {1}".format(expID, SIGNAL_DELAY))
        output("instance {0} set-topology on".format(expID))

        if "rate" in parts:
            output(
                "instance {0} set-rate {1}".format(
                    expID,
                    parts["rate"]
                )
            )

        bandwidths = {}
        if "bandwidth" in parts:
            bandwidth = parseBandwidth(
                parts["bandwidth"], default="0")  # default: unlimited
            for s in slaves:
                bandwidths[s] = bandwidth
        elif "node-bandwidth" in parts:
            for kvpair in parts["node-bandwidth"]:
                k, v = kvpair.split(":", 1)
                bandwidths[slaves[int(k)]] = parseBandwidth(v, default="0")

        if bandwidths:
            setBandwidth(expID, bandwidths)

        if "stage-seq" in parts:
            expID, firstDetail = generateScenarioCommands(
                expID, slaves, numNodes,
                parseNodes(parts["stage-seq"]),
                conns, "set-stage")
        else:
            stage = parts.get("stage", "-1")
            if stage[0] == "+":
                parts["stage"] = int(stage)
                stage = parts["stage"]
                output("instance {0} set-stage {1}".format(expID, stage))
                output("")
            else:
                parts["stage"] = -1

        output("start {0}".format(expID))
        output("sleep {0}".format(parts.get("time", "30s")))
        output("stop {0} stop {0} 3s".format(expID))

        # also show correlations with the end of the experiments
        output("print {0} time experiment {0} finished".format(expID))
        output("sleep 5s")
        output("")

        for s in bandwidths:
            output("# NOTE: default bandwidth === last limit: {0}".format(s))
            output("instance {0} unset-node-bandwidth {1}".format(expID, s))
        output("")

        if "stage" in parts and parts["stage"] >= 0:
            unsetBandwidth(expID, bandwidths)

        # enable client y/n?
        expID, firstDetail = generateScenarioCommands(
            expID, slaves, numNodes,
            parseNodes(parts.get("client-enabled", "0")),
            conns, "set-client-enabled")

        output("*** Killing and stopping {0} ***".format(expType))

        output("kill {0}".format(expID))

        # experiment finished
        if "keep-slaves" in parts and parts["keep-slaves"] == "yes":
            output("stop {0} stop slaves {1}".format(expID, STOP_SLAVES_DELAY))
        else:
            output("stop {0}".format(expID))

        output("sleep 5s")
        output("")

        updateStatus(expID)
        submitLogs(expID, nodes+clientNodes)

    return expID, slaves, numNodes, parts


def setBandwidth(expID, bandwidths):
    output("# Set bandwidth limits.")
    for s, bandwidth in bandwidths.items():
        if bandwidth != "0" and bandwidth != "unlimited":
            output(
                "exec-start {0} /dev/null bash -lc \""
                "tc qdisc del dev eth0 root tbf rate 1gbit burst 320kbit latency 400ms 2>/dev/null || true "
                "&& tc qdisc add dev eth0 root tbf rate {1}kbit burst 320kbit latency 400ms"
                "\"".format(s, bandwidth)
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
                "exec-start {0} /dev/null bash -lc \""
                "tc qdisc del dev eth0 root tbf rate 1gbit burst 320kbit latency 400ms 2>/dev/null || true"
                "\"".format(s)
            )
            output(
                "exec-wait {0} 2000 "
                "exec-start {0} experiment-output/{1}/slave-__id__/FAILED echo Could not unset bandwidth; "
                "exec-wait {0} 2000".format(s, expID)
            )
    for s in bandwidths:
        output("sync {0}".format(s))
    output("")


def submitLogs(expID, slaves):
    """
    Logs são comprimidos em experiment-output-<exp>-slave-__id__.tar.gz
    dentro de $HOME/MASTER_EXP_DIR (tipicamente ~/iss/current-deployment-data)
    e enviados para o diretório do master definido em MASTER_EXP_DIR/raw-results/.
    Também criamos um log dedicado por slave (submit-logs.log) para depuração.
    """
    output("# Submit logs to master node")
    for s in slaves:
        # 1) Compactar logs localmente no slave, com CWD explícito no diretório de experimento.
        # Usamos bash -lc para poder encadear 'cd' + 'tar' numa única linha.
        output(
            'exec-start {0} experiment-output/{1}/slave-__id__/submit-logs.log '
            'bash -lc "set -e; cd $HOME/{2}; '
            'tar czf experiment-output-{1}-slave-__id__.tar.gz '
            'experiment-output/{1}/slave-__id__"'.format(
                s, expID, MASTER_EXP_DIR
            )
        )
        # Se a compactação falhar, marcamos o diretório do slave com FAILED.
        output(
            "exec-wait {0} 60000 "
            "exec-start {0} experiment-output/{1}/slave-__id__/FAILED echo Could not compress logs; "
            "exec-wait {0} 2000".format(s, expID)
        )

    for s in slaves:
        # 2) Enviar o .tar.gz gerado para o master (raw-results/).
        output(
            "exec-start {0} scp-output-{1}-logs.log stubborn-scp.sh {2} "
            "experiment-output-{1}-slave-__id__.tar.gz {3}/raw-results/".format(
                s, expID, SCP_RETRY_COUNT, "$own_public_ip:" + MASTER_EXP_DIR
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
    output("exec-start __all__ /dev/null bash -lc \""
           " if [ -e status ]; then "
           "   cat status; "
           "   echo finished: {0}; "
           " else "
           "   echo finished: {0}; "
           " fi "
           " > status.new"
           "\"".format(finishedExpID))
    output("exec-wait __all__ 2000")
    output("sync __all__")
    output("exec-start __all__ /dev/null mv status.new status")
    output("exec-wait __all__ 2000")
    output("sync __all__")
    output("")


def parseScenarioDesc(expID, line):
    parts = parseParts(line.strip().split())
    desc = {"name": line.strip(), "stage": -1, "time": "30s",
            "timeout": CLIENT_TIMEOUT, "id": expID}

    # if "id" in parts: desc["id"] = idFromTokens([parts["id"]])
    if "stage" in parts:
        desc["stage"] = int(parts["stage"])

    if "time" in parts:
        desc["time"] = parts["time"]

    if "timeout" in parts:
        desc["timeout"] = parseTimeout(parts["timeout"], CLIENT_TIMEOUT)

    return desc


def parseFailureInj(expID, line, maxTime):
    parts = parseParts(line.strip().split())

    desc = {"name": " ".join(["failure-injection:", line.strip()]),
            "time": maxTime,
            "timeout": 0,
            "id": expID}

    if "time" in parts:
        desc["time"] = parts["time"]

    if "timeout" in parts:
        desc["timeout"] = parseTimeout(parts["timeout"], 0)

    if "fail" in parts:
        desc["fail"] = parseNodes(parts["fail"])

    if "fail-each" in parts:
        desc["fail"] = parseNodes(parts["fail-each"])

    if "inject" in parts:
        desc["inject"] = parseNodes(parts["inject"])

    if "record" in parts:
        desc["record"] = parseNodes(parts["record"])

    if "export" in parts:
        desc["export"] = parseNodes(parts["export"])

    return desc


def generateFailureCommands(desc, slaves, numNodes, sortedHosts):
    global deploymentSchedule

    failNodes = desc.get("fail", [])
    injectNodes = desc.get("inject", [])

    if not failNodes:
        return

    output("")
    output("#===============================================")
    output("# {0}".format(desc["name"]))
    output("#===============================================")
    output("")

    # This is somewhat ugly, but using hosts here *internally*, we need
    # to be sure that injecting on node 0 is always the master for example
    failNodesIP = [sortedHosts[i][0] for i in failNodes]
    failNodesIDX = [sortedHosts[i][1] for i in failNodes]

    injectNodesIP = [sortedHosts[i][0] for i in injectNodes]
    injectNodesIDX = [sortedHosts[i][1] for i in injectNodes]

    # Start failure injector
    output("# Start failure injector")
    output("instance {0} set-fault-master {1}".format(
        desc["id"], failNodesIP[0]))
    output("instance {0} set-fault-nodes {1}".format(
        desc["id"],
        " ".join(str(n) for n in failNodesIDX)))
    output("instance {0} set-fault-export {1}".format(
        desc["id"],
        " ".join(str(sortedHosts[i][1]) for i in desc.get("export", []))))
    output("instance {0} set-fault-record {1}".format(
        desc["id"],
        " ".join(str(sortedHosts[i][1]) for i in desc.get("record", []))))
    output("instance {0} set-fault-inject {1}".format(
        desc["id"],
        " ".join(str(n) for n in injectNodesIDX)))
    output("instance {0} set-fault-time {1}".format(
        desc["id"],
        desc["time"]))
    output("instance {0} set-fault-timeout {1}".format(
        desc["id"],
        desc["timeout"]))
    output("start {0}".format(desc["id"]))
    output("sleep {0}".format(desc["time"]))
    output("stop {0}".format(desc["id"]))
    output("sleep 10s")
    output("")

    deploymentSchedule.append({
        "id": "faults_{0}".format(desc["id"]),
        "name": desc["name"],
        "priority": 0,
        "repeat": 1,
        "deadline": "????-??-?? ??"
    })


def generateScenarioCommands(expID, slaves, numNodes, stages, conns,
                             command):
    global deploymentSchedule

    host_ip = []
    for line in sortedHosts:
        host_ip.append(line[0])

    firstDetail = False

    for stage in stages:
        desc = parseScenarioDesc(expID, scenario[stage])

        # if desc["id"] >= lastFinished:
        if desc["id"] == expID:
            # scnID = desc["id"]
            scnID = expID

            if not firstDetail:
                # should name also experiment here?
                # Check if at least one exp has finished
                if deploymentSchedule:
                    output("*** Scn details for experiment {0} ***".format(
                        deploymentSchedule[-1]["id"]))
                    firstDetail = True

            # When doing a "switch to stage", there is no need to "start"
            if desc["stage"] >= 0:
                output("instance {0} {1} stage {2}".format(
                    scnID, command, desc["stage"]))
                output("instance {0} print stage".format(scnID))
            # otherwise assume we are setting the connection
            else:
                output("instance {0} {1} {2}".format(
                    scnID, command, desc["time"]))

            output("sleep {0}".format(desc["time"]))
            output("stop {0} stop scenario stage {1}".format(scnID, STOP_SLAVES_DELAY))
            # output("sleep 5s")
            output("sleep 1s")
            output("")

            maxTime = desc["timeout"] + 20000

            if "fail" in scenario[stage]:
                desc = parseFailureInj(expID, scenario[stage], maxTime)
                generateFailureCommands(desc, slaves, numNodes, sortedHosts)
                # take some extra time before actually doing the next experiments
                # in case there are issues at the network
                output("sleep 10s")

            output("print {0} time scenario {0} stage {1} end ".format(
                scnID, stage))
            output("sleep 1s")

    return expID, firstDetail


def printDeploymentSchedule():
    if not deploymentSchedule:
        return

    print("#" * 89, file=sys.stderr)
    print("#" * 3 + " SCHEDULE " + "#" * 74, file=sys.stderr)
    print("#" * 89, file=sys.stderr)
    print("# Exp.ID* Name*Priority Repeats Deadline*",
          file=sys.stderr)

    # best effort attempt to order exp by deadline
    byDeadline = defaultdict(list)
    deadlineOrder = []

    for e in deploymentSchedule:
        byDeadline[e["deadline"]].append(e)

    deadlines = sorted(byDeadline.keys())
    for d in deadlines:
        if d[0] == "?":
            continue

        byDeadline[d] = sorted(byDeadline[d],
                               key=lambda e: (e["priority"], e["id"]))
        deadlineOrder.extend(byDeadline[d])

    deadlineOrder.extend(sorted(byDeadline["????-??-?? ??"],
                                key=lambda e: (e["priority"], e["id"])))

    for e in deadlineOrder:
        print("  {0}\t{1}\t{2:>8}\t{3:>3}\t{4}".format(
            e["id"], e["name"], e["priority"], e["repeat"], e["deadline"]
        ), file=sys.stderr)

    print("  Start numbering at 0; previous experiment results refer to last experiment ID used.",
          file=sys.stderr)


##########
# globals.
scenario = []
expID = 0
slaves = {}
numNodes = {"peers": 0}
sortedHosts = []


def parseHosts(filename):
    hosts = {}
    countHost = 0

    with open(filename, "r") as desc:
        tokens = desc.readline().split()
        while len(tokens) >= 5:
            # expected fields:
            # priority, ID, #instances, tag, host-template
            host_spec = tokens[:4]
            host_spec[1] = int(host_spec[1])
            # tag or slave types (peer, client...)
            if host_spec[2] != "0":
                for i in range(host_spec[2]):
                    hosts[countHost] = host_spec[3]
                    # expected names: node-<n>.scalable-systems
                    countHost = countHost + 1

            tokens = desc.readline().split()

    return hosts


def parseScenario(filename):
    scenario = []

    with open(filename, "r") as desc:
        # ignore tags
        tags = desc.readline()

        line = desc.readline()
        while line:
            # ignore comments and empty configs
            scenario.append(
                line if not line.startswith("#") and len(line.strip())
                else ""
            )

            line = desc.readline()

    return scenario


# parse inputs
stdin = fileinput.input()
# expDescFilename = sys.argv[1]
expDescFilename = None
hostsDescFilename = sys.argv[1]
scenarioDescFilename = sys.argv[2]
outputFilename = sys.argv[3]

# parse hosts
slaves = parseHosts(hostsDescFilename)
for s in slaves:
    numSlaves[slaves[s]] += 1

print(
    "# {0} slaves in experiment: {1}".format(len(slaves), slaves),
    file=sys.stderr)

# parse experiments
scenario = parseScenario(scenarioDescFilename)

expID = 0
expNames = []

outFile = open(outputFilename, "w")

# master node id
output("master {0}".format(LOCAL_MASTER_PORT))
output("sleep {0}".format("5s"))


# commands involving all experiments
for line in scenario:
    try:
        if not line or line.startswith("#"):
            continue
        # FIXME:
        # Command lines (starting with "cmd:") appear directly in the
        # experiments description script and are directly translated
        # to the master commands without any processing.
        #
        # The master then runs the command for every instance
        # involved in the execution.
        #
        # Perhaps it should be moved to the end of script, instead
        # of being mixed with the rest of the commands.
        tokens = line.split()
        if tokens[0] == "exp:":
            expID, slaves, numNodes, parts = generateExpCommands(
                line.split("exp:", 1)[1], slaves, numNodes)
        elif tokens[0] == "cmd:":
            if line.startswith("cmd: instance"):
                # FIXME: for now we just let user handle experiment IDs
                output(line.split("cmd: ", 1)[1])
                output("")
            elif "flood-nodes" in line:
                # FIXME: currently flooding applies to all nodes
                parts = parseParts(tokens)
                output("instance {0} set-flooding-targets {1}".format(
                    parts.get("exp", expID),
                    " ".join(
                        str(i) for i in parseNodes(
                            parts.get("flood-nodes", "0")))))
                output("instance {0} set-flooding-rate {1}".format(
                    parts.get("exp", expID), parts["rate"]))
                output("instance {0} set-flooding-client-rate {1}".format(
                    parts.get("exp", expID), parts["client-rate"]))
                output("instance {0} start-flooding".format(
                    parts.get("exp", expID)))
                output("sleep {0}".format(parts["time"]))
                output("instance {0} stop-flooding".format(
                    parts.get("exp", expID)))
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
                raise ValueError(
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

