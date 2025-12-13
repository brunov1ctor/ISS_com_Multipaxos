#!/usr/bin/env python3
"""
generate-master-commands.py

Gera o script master-commands-template.cmd (bash) para o ISS/mirbft.

Correções estruturais:
- NÃO assume coluna 'config' no deployment.csv.
- Resolve config file de forma robusta.
- Atualiza status_file durante execução.
- SEMPRE escreve o status final com o último experimento (ex: 0003).
"""

import sys
import os
import stat
import csv
from pathlib import Path
from typing import Dict, List, Optional

# ------------------------------------------------------------
# Helpers
# ------------------------------------------------------------

def die(msg: str) -> None:
    print(f"[generate-master-commands][ERRO] {msg}", file=sys.stderr)
    sys.exit(1)

def info(msg: str) -> None:
    print(f"[generate-master-commands] {msg}")

def is_probably_config_key(k: str) -> bool:
    k = k.lower().strip()
    if "config" in k and ("path" in k or "file" in k or k == "config"):
        return True
    return k in {
        "cfg",
        "configyml",
        "config_yaml",
        "config_yml",
        "configfile",
        "configpath",
    }

def pick_config_from_row(row: Dict[str, str]) -> Optional[str]:
    for k, v in row.items():
        if is_probably_config_key(k) and v:
            return v.strip()
    return None

def resolve_config_path(exp_id: str, row_cfg: Optional[str], exp_data_dir: Path) -> Path:
    # 1) veio do CSV
    if row_cfg:
        p = Path(row_cfg)
        if p.is_absolute():
            return p
        cand1 = (exp_data_dir / row_cfg).resolve()
        cand2 = (exp_data_dir.parent / row_cfg).resolve()
        if cand1.exists():
            return cand1
        if cand2.exists():
            return cand2
        return cand1

    # 2) padrão normal
    cand = exp_data_dir / "config" / f"config-{exp_id}.yml"
    if cand.exists():
        return cand

    # 3) fallback faulty
    cand_faulty = exp_data_dir / "config" / f"config-{exp_id}-faulty.yml"
    if cand_faulty.exists():
        return cand_faulty

    # 4) último recurso
    return cand

# ------------------------------------------------------------
# Args
# ------------------------------------------------------------

if len(sys.argv) < 4:
    die("Uso: generate-master-commands.py <local|remote> <deployment.dpl> <output-template> [exp-data-dir]")

depl_type = sys.argv[1]
deployment_file = Path(sys.argv[2])
output_template = Path(sys.argv[3])
exp_data_dir = Path(sys.argv[4]).resolve() if len(sys.argv) >= 5 else None

if depl_type not in ("local", "remote"):
    die(f"Tipo de deploy inválido: {depl_type}")

if not deployment_file.exists():
    die(f"Deployment file não existe: {deployment_file}")

if exp_data_dir is None:
    exp_data_dir = deployment_file.parent.resolve()

deployment_csv = deployment_file.parent / "deployment.csv"
if not deployment_csv.exists():
    die(f"deployment.csv não encontrado em {deployment_csv}")

# ------------------------------------------------------------
# Ler deployment.csv
# ------------------------------------------------------------

experiments: List[Dict[str, str]] = []
with open(deployment_csv, newline="") as f:
    reader = csv.DictReader(f)
    if not reader.fieldnames:
        die("deployment.csv não tem header")
    for row in reader:
        experiments.append({k.strip(): (v or "").strip() for k, v in row.items()})

if not experiments:
    die("deployment.csv não contém experimentos")

if "exp" not in experiments[0]:
    die(f"deployment.csv não contém coluna 'exp'. Colunas: {list(experiments[0].keys())}")

last_exp_id = experiments[-1]["exp"]

info(f"{len(experiments)} experimentos detectados")
info(f"Último experimento = {last_exp_id}")
info(f"deployment_csv = {deployment_csv}")
info(f"exp_data_dir   = {exp_data_dir}")

# ------------------------------------------------------------
# Variáveis resolvidas em runtime (envsubst)
# ------------------------------------------------------------

STATUS_FILE = "${status_file}"
REMOTE_WORK_DIR = "${remote_work_dir}"
REMOTE_EXP_DIR = "${remote_exp_dir}"
BIN_DIR = "${remote_bin_dir}"

# ------------------------------------------------------------
# Gerar script bash
# ------------------------------------------------------------

cmds: List[str] = []

cmds += [
    "#!/usr/bin/env bash",
    "set -euo pipefail",
    "",
    "ts(){ date '+%Y-%m-%d %H:%M:%S'; }",
    "log(){ echo \"[MASTER][$(ts)] $*\"; }",
    "",
    f"STATUS_FILE={STATUS_FILE}",
    f"REMOTE_WORK_DIR={REMOTE_WORK_DIR}",
    f"REMOTE_EXP_DIR={REMOTE_EXP_DIR}",
    f"BIN_DIR={BIN_DIR}",
    "",
    "mkdir -p \"$REMOTE_WORK_DIR/logs\" \"$REMOTE_EXP_DIR/raw-results\"",
    "MASTER_LOG=\"$REMOTE_WORK_DIR/logs/master-exec.log\"",
    "exec > >(tee -a \"$MASTER_LOG\") 2>&1",
    "",
    "log 'Inicializando execução de experimentos'",
    "echo RUNNING > \"$STATUS_FILE\"",
    "",
]

# ------------------------------------------------------------
# Loop de experimentos
# ------------------------------------------------------------

for exp in experiments:
    exp_id = exp["exp"]
    row_cfg = pick_config_from_row(exp)
    cfg_path = resolve_config_path(exp_id, row_cfg, exp_data_dir)

    cfg_on_master = f"$REMOTE_WORK_DIR/experiment-config/config-{exp_id}.yml"

    cmds += [
        "",
        f"log '==== Iniciando experimento {exp_id} ===='",
        f"echo RUNNING-{exp_id} > \"$STATUS_FILE\"",
        f"log \"Config inferido (node-0): {cfg_path}\"",
        f"log \"Config esperado no master: {cfg_on_master}\"",
        "",
        f"log \"Executando orderingclient ({exp_id})\"",
        "set +e",
        f"\"$BIN_DIR/orderingclient\" -config {cfg_on_master} >> \"$REMOTE_WORK_DIR/logs/orderingclient-{exp_id}.log\" 2>&1",
        "rc=$?",
        "set -e",
        f"log \"orderingclient rc=$rc (exp={exp_id})\"",
        "",
        f"log 'Coletando resultados do experimento {exp_id}'",
        f"echo COLLECTING-{exp_id} > \"$STATUS_FILE\"",
        "",
        f"tarname=\"$REMOTE_EXP_DIR/raw-results/experiment-output-{exp_id}-$(hostname).tar.gz\"",
        "log \"Gerando tar: $tarname\"",
        "set +e",
        "tar -czf \"$tarname\" -C \"$REMOTE_WORK_DIR\" logs experiment-output experiment-config master-commands.cmd status 2>/dev/null",
        "tarc=$?",
        "set -e",
        "log \"tar rc=$tarc\"",
    ]

# ------------------------------------------------------------
# Finalização (BUG FIX)
# ------------------------------------------------------------

cmds += [
    "",
    "log 'Todos os experimentos processados. Finalizando.'",
    f"echo {last_exp_id} > \"$STATUS_FILE\"",
    f"log 'STATUS FINAL = {last_exp_id}'",
    "ls -la \"$REMOTE_EXP_DIR/raw-results\" || true",
    "",
]

# ------------------------------------------------------------
# Escrever arquivo
# ------------------------------------------------------------

output_template.parent.mkdir(parents=True, exist_ok=True)
with open(output_template, "w") as f:
    f.write("\n".join(cmds))
    f.write("\n")

st = os.stat(output_template)
os.chmod(output_template, st.st_mode | stat.S_IEXEC)

info(f"Arquivo gerado com sucesso: {output_template}")

