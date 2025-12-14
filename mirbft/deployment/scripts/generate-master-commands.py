#!/usr/bin/env python3

import argparse
import json
import re
import sys
from datetime import datetime
from pathlib import Path


def read_dpl_lines(dpl_path: Path):
  lines = []
  for line in dpl_path.read_text().splitlines():
    l = line.strip()
    if not l or l.startswith("#"):
      continue
    lines.append(l)
  return lines


def parse_dpl_lines(lines):
  """
  Espera linhas no formato:
    <exp_id> <n_instances> <tag> <template>
  ex:
    -1 4 peers cloud-machine-templates/small-machine-fra05.cmt
  """
  out = []
  for l in lines:
    parts = l.split()
    if len(parts) < 4:
      print(f"[WARN] linha dpl ignorada (curta): {l}", file=sys.stderr)
      continue
    exp_id = parts[0]
    n = int(parts[1])
    tag = parts[2]
    templ = parts[3]
    out.append((exp_id, n, tag, templ))
  return out


def emit(line, out_lines):
  out_lines.append(line)


def comment(msg, out_lines):
  emit(f"# {msg}", out_lines)


def write_file(path, content, out_lines):
  # write-file <content> <fileName>
  emit(f"write-file {content} {path}", out_lines)


def wait_for_slaves(n, tag, out_lines):
  emit(f"wait {n} {tag}", out_lines)


def sync(tag, out_lines):
  emit(f"sync {tag}", out_lines)


def exec_start(tag, out_lines, cmd, args=None, out_file=None):
  # exec-start <tag> <outFileOrDash> <cmd> [args...]
  if args is None:
    args = []
  if out_file is None:
    out_file = "-"
  joined_args = " ".join(str(a) for a in args)
  if joined_args:
    emit(f"exec-start {tag} {out_file} {cmd} {joined_args}", out_lines)
  else:
    emit(f"exec-start {tag} {out_file} {cmd}", out_lines)


def exec_wait(tag, timeout_ms, out_lines):
  emit(f"exec-wait {tag} {timeout_ms}", out_lines)


def noop(tag, out_lines):
  emit(f"noop {tag}", out_lines)


def discover_reset(npeers, out_lines):
  emit(f"discover-reset {npeers}", out_lines)


def discover_wait(out_lines):
  emit("discover-wait", out_lines)


def mkexpdir(tag, exp_id_digits, exp, out_lines):
  # cria estrutura básica de output por slave para aquele experimento
  # (usa wildcards __id__)
  base = f"experiment-output/{exp:0{exp_id_digits}d}/slave-__id__"
  exec_start(tag, out_lines, "mkdir", ["-p", f"{base}/config"], out_file="/dev/null")
  exec_start(tag, out_lines, "mkdir", ["-p", f"{base}/prof"], out_file="/dev/null")
  exec_start(tag, out_lines, "mkdir", ["-p", f"{base}/prof-client"], out_file="/dev/null")
  exec_start(tag, out_lines, "mkdir", ["-p", f"{base}/client"], out_file="/dev/null")
  exec_wait(tag, 2000, out_lines)


def diagnostics(tag, out_lines):
  # Diagnósticos básicos (debug)
  exec_start(tag, out_lines, "whoami", out_file="experiment-output/0000/slave-__id__/whoami.log")
  exec_start(tag, out_lines, "hostname", out_file="experiment-output/0000/slave-__id__/hostname.log")
  exec_start(tag, out_lines, "pwd", out_file="experiment-output/0000/slave-__id__/pwd.log")
  exec_start(tag, out_lines, "printenv", ["GOPATH"], out_file="experiment-output/0000/slave-__id__/env-gopath.log")
  exec_start(tag, out_lines, "printenv", ["PATH"], out_file="experiment-output/0000/slave-__id__/env-path.log")
  exec_start(tag, out_lines, "printenv", ["master_port"], out_file="experiment-output/0000/slave-__id__/env-masterport.log")
  exec_start(tag, out_lines, "printenv", ["own_public_ip"], out_file="experiment-output/0000/slave-__id__/env-ownip.log")
  exec_start(tag, out_lines, "ls", ["-la", "/users/$USER/go/bin"], out_file="experiment-output/0000/slave-__id__/ls-users-go-bin.log")
  exec_start(tag, out_lines, "ls", ["-la", "$GOPATH/bin"], out_file="experiment-output/0000/slave-__id__/ls-gopath-bin.log")
  exec_start(tag, out_lines, "ls", ["-la", "config"], out_file="experiment-output/0000/slave-__id__/ls-config-dir.log")
  exec_start(tag, out_lines, "ls", ["-la", "config/config.yml"], out_file="experiment-output/0000/slave-__id__/ls-config-yml.log")
  exec_wait(tag, 3000, out_lines)


def stubborn_scp_cmd(remote_work_dir: str | None):
  """
  IMPORTANTE:
    discoveryslave executa comandos via exec.LookPath usando o PATH do processo.
    Não dá pra confiar que PATH inclua /users/<user>/iss/scripts.
    Então aqui geramos o caminho absoluto quando temos remote_work_dir.
  """
  if remote_work_dir:
    return f"{remote_work_dir.rstrip('/')}/scripts/stubborn-scp.sh"
  return "stubborn-scp.sh"


def fetch_config(tag, out_lines, master_ip, remote_config_dir, exp, remote_work_dir: str | None):
  # Copia config do master -> slave via stubborn-scp.sh
  # Formato esperado pelo wrapper:
  #   stubborn-scp.sh 10 -i  <masterip>:<remote_path> <local_path>
  scp = stubborn_scp_cmd(remote_work_dir)
  src = f"{master_ip}:{remote_config_dir}/config-{exp:04d}.yml"
  dst = "config/config.yml"
  exec_start(tag, out_lines, scp, ["10", "-i", src, dst], out_file=f"scp-output-{exp:04d}-config.log")
  # Se falhar, cria um marcador de FAILED dentro do output daquele slave
  exec_wait(tag, 2000, out_lines)
  exec_start(tag, out_lines, "echo", ["Could not fetch config"], out_file=f"experiment-output/{exp:04d}/slave-__id__/FAILED")
  exec_wait(tag, 2000, out_lines)

  # pós-check
  exec_start(tag, out_lines, "ls", ["-la", dst], out_file=f"experiment-output/{exp:04d}/slave-__id__/post-config-ls.log")
  exec_start(tag, out_lines, "head", ["-n", "40", dst], out_file=f"experiment-output/{exp:04d}/slave-__id__/post-config-head.log")
  exec_wait(tag, 3000, out_lines)


def run_peer(tag, out_lines, master_ip, master_port, exp, out_root):
  base = f"{out_root}/{exp:04d}/slave-__id__"
  # orderingpeer config/config.yml <masterAddr> <publicIP> <privateIP> <trc> <profDir>
  exec_start(tag, out_lines, "orderingpeer",
             ["config/config.yml",
              f"{master_ip}:{master_port}",
              "__public_ip__",
              "__private_ip__",
              f"{base}/peer.trc",
              f"{base}/prof"],
             out_file=f"{base}/peer.log")
  # (não espera aqui; a coordenação usa discover-wait)


def run_client(tag, out_lines, master_ip, master_port, exp, out_root):
  base = f"{out_root}/{exp:04d}/slave-__id__"
  # orderingclient config/config.yml <masterAddr> <publicIP> <privateIP> <outTrc> <profDir>
  exec_start(tag, out_lines, "orderingclient",
             ["config/config.yml",
              f"{master_ip}:{master_port}",
              "__public_ip__",
              "__private_ip__",
              f"{base}/client.trc",
              f"{base}/prof-client"],
             out_file=f"{base}/client.log")


def main():
  ap = argparse.ArgumentParser()
  ap.add_argument("deployment_file", type=str, help="Arquivo .dpl")
  ap.add_argument("template_out", type=str, help="Arquivo template (opcional)")
  ap.add_argument("out_file", type=str, help="Arquivo master-commands.cmd")
  ap.add_argument("--master-port", type=int, default=9999)
  ap.add_argument("--remote-config-dir", type=str, default="/users/$USER/iss/experiment-config")
  ap.add_argument("--remote-work-dir", type=str, default=None)
  args = ap.parse_args()

  dpl_path = Path(args.deployment_file)
  lines = read_dpl_lines(dpl_path)
  entries = parse_dpl_lines(lines)

  # Calcula quantos peers e clients (por tag)
  tag_counts = {}
  for (_, n, tag, _) in entries:
    tag_counts[tag] = tag_counts.get(tag, 0) + n

  # Assume: existe exatamente um "1client" e N "peers"
  n_peers = tag_counts.get("peers", 0)
  n_clients = tag_counts.get("1client", 0)

  out_lines = []
  comment(f"Generated by generate-master-commands.py at {datetime.now().isoformat()}", out_lines)
  comment(f"deployment_file={args.deployment_file}", out_lines)
  comment(f"master_port={args.master_port}", out_lines)
  comment(f"remote_config_dir={args.remote_config_dir}", out_lines)
  comment(f"remote_work_dir={args.remote_work_dir}", out_lines)
  emit("", out_lines)

  # master-ready marker
  write_file("/users/$USER/iss/master-ready", "READY", out_lines)

  # wait for registrations
  wait_for_slaves(n_peers, "peers", out_lines)
  if n_clients > 0:
    wait_for_slaves(n_clients, "1client", out_lines)

  # Primeira sync
  sync("peers", out_lines)
  if n_clients > 0:
    sync("1client", out_lines)

  # Preparação (só para exp 0000, mas ok)
  mkexpdir("peers", 4, 0, out_lines)
  if n_clients > 0:
    mkexpdir("1client", 4, 0, out_lines)

  # Diagnóstico inicial para exp 0000
  diagnostics("peers", out_lines)
  if n_clients > 0:
    diagnostics("1client", out_lines)

  # Para cada experimento (IDs derivados de index)
  # Aqui supomos 4 experimentos: 0000..0003
  # (no deploy, o generate-config.sh já cria esses)
  master_ip = "___MASTER_IP___"  # placeholder, substituído no deploy-remote.sh
  for exp in range(0, 4):
    comment(f"=== EXP {exp:04d} ===", out_lines)

    # Busca config no slave (via master)
    fetch_config("peers", out_lines, master_ip, args.remote_config_dir, exp, args.remote_work_dir)
    if n_clients > 0:
      fetch_config("1client", out_lines, master_ip, args.remote_config_dir, exp, args.remote_work_dir)

    # sync para garantir config presente
    sync("peers", out_lines)
    if n_clients > 0:
      sync("1client", out_lines)

    # Reset discovery (no. peers fixo)
    discover_reset(n_peers, out_lines)

    # pre-check simples
    exec_start("peers", out_lines, "printenv", ["master_port"], out_file=f"experiment-output/{exp:04d}/slave-__id__/peer-pre.log")
    exec_start("peers", out_lines, "printenv", ["own_public_ip"], out_file=f"experiment-output/{exp:04d}/slave-__id__/peer-pre.log")
    exec_start("peers", out_lines, "ls", ["-la", "orderingpeer"], out_file=f"experiment-output/{exp:04d}/slave-__id__/peer-pre.log")

    # Start peers
    run_peer("peers", out_lines, master_ip, args.master_port, exp, "experiment-output")

    # Aguarda discover-wait (peers prontos)
    discover_wait(out_lines)

    # Start client
    if n_clients > 0:
      run_client("1client", out_lines, master_ip, args.master_port, exp, "experiment-output")

    # Espera um pouco e sync
    exec_wait("peers", 2000, out_lines)
    if n_clients > 0:
      exec_wait("1client", 2000, out_lines)

    sync("peers", out_lines)
    if n_clients > 0:
      sync("1client", out_lines)

  # Final: grava status "0003" (para o deploy.sh sair do loop)
  write_file("/users/$USER/iss/status", "0003", out_lines)
  emit("exit", out_lines)

  # Render template_out (só informativo)
  Path(args.template_out).write_text("\n".join(out_lines) + "\n")

  # master-commands final (substitui placeholder master_ip)
  # O deploy-remote.sh faz replace do ___MASTER_IP___.
  Path(args.out_file).write_text("\n".join(out_lines) + "\n")


if __name__ == "__main__":
  main()

