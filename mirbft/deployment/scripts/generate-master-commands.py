#!/usr/bin/env python3
"""
generate-master-commands.py

Gera o script master-commands.cmd que:
- inicializa o experimento
- executa cada experimento sequencialmente
- gera experiment-output-*.tar.gz
- ATUALIZA O STATUS FILE CORRETAMENTE (BUG FIXADO)
"""

import sys
import os
import stat
import csv
from pathlib import Path

# ------------------------------------------------------------
# Helpers
# ------------------------------------------------------------

def die(msg):
    print(f"[generate-master-commands][ERRO] {msg}", file=sys.stderr)
    sys.exit(1)

def info(msg):
    print(f"[generate-master-commands] {msg}")

def sh(cmd):
    return cmd.strip()

# ------------------------------------------------------------
# Args
# ------------------------------------------------------------

if len(sys.argv) < 4:
    die("Uso: generate-master-commands.py <local|remote> <deployment.dpl> <output-template> [exp-data-dir]")

depl_type = sys.argv[1]
deployment_file = sys.argv[2]
output_template = sys.argv[3]
exp_data_dir = sys.argv[4] if len(sys.argv) >= 5 else None

if depl_type not in ("local", "remote"):
    die(f"Tipo de deploy inválido: {depl_type}")

deployment_file = Path(deployment_file)
output_template = Path(output_template)

if not deployment_file.exists():
    die(f"Deployment file não existe: {deployment_file}")

# ------------------------------------------------------------
# Ler deployment.csv
# ------------------------------------------------------------

deployment_dir = deployment_file.parent
deployment_csv = deployment_dir / "deployment.csv"

if not deployment_csv.exists():
    die(f"deployment.csv não encontrado em {deployment_csv}")

experiments = []
with open(deployment_csv, newline="") as f:
    reader = csv.DictReader(f)
    for row in reader:
        experiments.append(row)

if not experiments:
    die("deployment.csv não contém experimentos")

last_exp_id = experiments[-1]["exp"]

info(f"{len(experiments)} experimentos detectados. Último exp = {last_exp_id}")

# ------------------------------------------------------------
# Variáveis esperadas no runtime (envsubst resolve)
# ------------------------------------------------------------

STATUS_FILE = "${status_file}"
REMOTE_WORK_DIR = "${remote_work_dir:-/users/Bruno/iss}"
REMOTE_EXP_DIR = "${remote_exp_dir:-${remote_work_dir}/current-deployment-data}"
BIN_DIR = "${remote_bin_dir}"
SSH_OPTS = "${ssh_options}"

# ------------------------------------------------------------
# Gerar script
# ------------------------------------------------------------

cmds = []

cmds.append("#!/usr/bin/env bash")
cmds.append("set -euo pipefail")
cmds.append("")
cmds.append("ts(){ date '+%Y-%m-%d %H:%M:%S'; }")
cmds.append("log(){ echo \"[MASTER][$(ts)] $*\"; }")
cmds.append("")
cmds.append(f"STATUS_FILE={STATUS_FILE}")
cmds.append(f"REMOTE_WORK_DIR={REMOTE_WORK_DIR}")
cmds.append(f"REMOTE_EXP_DIR={REMOTE_EXP_DIR}")
cmds.append(f"BIN_DIR={BIN_DIR}")
cmds.append("")
cmds.append("mkdir -p \"$REMOTE_WORK_DIR/logs\" \"$REMOTE_EXP_DIR/raw-results\"")
cmds.append("")

# ------------------------------------------------------------
# Inicialização
# ------------------------------------------------------------

cmds.append("log 'Inicializando execução de experimentos'")
cmds.append("echo RUNNING > \"$STATUS_FILE\"")
cmds.append("")

# ------------------------------------------------------------
# Loop de experimentos
# ------------------------------------------------------------

for exp in experiments:
    exp_id = exp["exp"]
    cfg = exp["config"]

    cmds.append(f"log '==== Iniciando experimento {exp_id} ===='")
    cmds.append(f"echo RUNNING-{exp_id} > \"$STATUS_FILE\"")
    cmds.append("")

    # Executa o experimento via orderingclient
    cmds.append(sh(f"""
    log "Executando orderingclient para experimento {exp_id}"
    "$BIN_DIR/orderingclient" \
        -config "{cfg}" \
        >> "$REMOTE_WORK_DIR/logs/orderingclient-{exp_id}.log" 2>&1
    """))

    # Compacta resultados
    cmds.append(f"log 'Coletando resultados do experimento {exp_id}'")
    cmds.append(f"echo COLLECTING-{exp_id} > \"$STATUS_FILE\"")

    cmds.append(sh(f"""
    tar -czf "$REMOTE_EXP_DIR/raw-results/experiment-output-{exp_id}-$(hostname).tar.gz" \
        -C "$REMOTE_WORK_DIR" \
        experiment-output \
        logs \
        || true
    """))

    cmds.append("")

# ------------------------------------------------------------
# WRAP-UP FINAL (BUG FIX CRÍTICO)
# ------------------------------------------------------------

cmds.append("log 'Todos os experimentos executados. Finalizando.'")
cmds.append(f"echo {last_exp_id} > \"$STATUS_FILE\"")
cmds.append(f"log 'STATUS FINAL = {last_exp_id}'")
cmds.append("")

# ------------------------------------------------------------
# Escrever arquivo
# ------------------------------------------------------------

output_template.parent.mkdir(parents=True, exist_ok=True)

with open(output_template, "w") as f:
    f.write("\n".join(cmds))
    f.write("\n")

# Tornar executável
st = os.stat(output_template)
os.chmod(output_template, st.st_mode | stat.S_IEXEC)

info(f"Arquivo gerado com sucesso: {output_template}")

