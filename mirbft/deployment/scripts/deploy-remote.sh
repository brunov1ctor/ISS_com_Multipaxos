#!/bin/bash

# scripts/deploy-remote.sh
#
# Remote deployment using instance-info (e.g., Emulab/cluster).
#
# Este script é *sourced* por deploy.sh, que já:
#   - sourced scripts/global-vars.sh
#   - run scripts/initialize-deployment.sh
#   - fez preflight de build (binários locais)
#
# Objetivos:
#   - gerar master-commands.cmd (template -> envsubst)
#   - reset remoto (com logs reais)
#   - start master em MASTER mode (file-based commands) => não morre no EOF
#   - start slaves
#   - esperar status final e baixar resultados
#   - falhar se não houver outputs reais

set -euo pipefail
shopt -s nullglob

this_dir="$(cd "$(dirname "${BASH_SOURCE[0]}" )" && pwd)"
deployment_dir="$(cd "$this_dir/.." && pwd)"

ts() { date +"%Y-%m-%d %H:%M:%S"; }
info(){ echo "[INFO  ][$(ts)] $*"; }
warn(){ echo "[WARN  ][$(ts)] $*"; }
err(){  echo "[ERRO  ][$(ts)] $*" >&2; }

die(){ err "$*"; exit 1; }

# --------- defaults essenciais p/ não explodir com set -u ----------
remote_user="${remote_user:-${REMOTE_USER:-${USER}}}"
export remote_user

ssh_options="${ssh_options:--o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null}"
export ssh_options

# Port do discovery master (master_port é usado no envsubst e nos slaves)
master_port="${master_port:-${DISCOVERY_PORT:-9999}}"
export master_port
export DISCOVERY_PORT="${DISCOVERY_PORT:-${master_port}}"

# Onde o master escreve status/ready no remoto (usados pelo deploy + master-commands)
remote_work_dir="${remote_work_dir:-/users/${remote_user}/iss}"
remote_status_file="${remote_status_file:-${remote_work_dir}/status}"
remote_ready_file="${remote_ready_file:-${remote_work_dir}/master-ready}"
export remote_status_file
export remote_ready_file

# ---------------------------------------------------------
# Helper: resolve instance-info path
# ---------------------------------------------------------
resolve_instance_info() {
  local base_dir="$1"
  local info_arg="$2"

  if [[ "$info_arg" = /* ]] && [[ -f "$info_arg" ]]; then
    echo "$info_arg"; return 0
  fi

  local repo_dir
  repo_dir="$(cd "$deployment_dir/.." && pwd)"

  local cand1="$repo_dir/$info_arg"
  local cand2="$deployment_dir/$info_arg"

  [[ -f "$cand1" ]] && { echo "$cand1"; return 0; }
  [[ -f "$cand2" ]] && { echo "$cand2"; return 0; }
  [[ -f "$info_arg" ]] && { echo "$info_arg"; return 0; }

  return 1
}

# =========================================================
# 1) Determine master IP from instance-info
# =========================================================

deployment_file="$exp_data_dir/deployment.dpl"
[[ -f "$deployment_file" ]] || die "Deployment file not found: $deployment_file"

if ! instance_info_file="$(resolve_instance_info "$exp_data_dir" "$instance_info_file")"; then
  die "Could not find instance-info file: '$instance_info_file'"
fi

master_ip=""
while read -r instance_id ctrl_ip data_ip role itag; do
  [[ -z "${instance_id:-}" ]] && continue
  [[ "${instance_id}" =~ ^[[:space:]]*# ]] && continue
  if [[ "${role}" == "master" || "${instance_id}" == "master" || "${instance_id}" == "-1" ]]; then
    master_ip="$ctrl_ip"; break
  fi
done < "$instance_info_file"

[[ -n "$master_ip" ]] || die "Could not determine master IP from instance-info '$instance_info_file'"

info "Using instance info file: $instance_info_file"
info "Master IP address      : $master_ip"
info "remote_user            : $remote_user"
info "remote_work_dir        : $remote_work_dir"
info "master_port            : $master_port"
echo

# =========================================================
# 2) Generate master commands (template -> envsubst -> cmd)
# =========================================================

local_master_command_template="master-commands-template.cmd"
local_master_command_file="master-commands.cmd"

info "Gerando master commands via generate-master-commands.py"
info "  deployment_file (.dpl) = $deployment_file"
info "  template out           = $exp_data_dir/$local_master_command_template"

python3 scripts/generate-master-commands.py \
  remote \
  "$deployment_file" \
  "$exp_data_dir/$local_master_command_template" \
  "$exp_data_dir"

export ssh_key_file="$remote_private_key_file"
export own_public_ip="$master_ip"

# IMPORTANTE: ambos precisam existir para write-file $ready_file/$status_file
export status_file="$remote_status_file"
export ready_file="$remote_ready_file"

local_template_path="$exp_data_dir/$local_master_command_template"
local_cmd_path="$exp_data_dir/$local_master_command_file"

if [[ -f "$local_template_path" ]]; then
  envsubst '$ssh_key_file $own_public_ip $master_port $status_file $ready_file' \
    < "$local_template_path" > "$local_cmd_path"
else
  die "Template não existe: $local_template_path"
fi

[[ -s "$local_cmd_path" ]] || die "master-commands.cmd foi gerado mas está vazio: $local_cmd_path"
info "Master command script escrito em: $local_cmd_path"
echo

# =========================================================
# 3) Reset remoto (tolerante a erros, mas com logs reais)
# =========================================================

debug_dir="$exp_data_dir/_debug"
mkdir -p "$debug_dir"

info "Limpando processos antigos e removendo traffic shaping nas máquinas remotas..."

while read -r instance_id ctrl_ip data_ip role itag; do
  [[ -z "${instance_id:-}" ]] && continue
  [[ "${instance_id}" =~ ^[[:space:]]*# ]] && continue

  info "[reset-proc] ${ctrl_ip}: matando processos antigos..."
  if ssh $ssh_options "${remote_user}@${ctrl_ip}" "\
    pkill -f 'tail -F' 2>/dev/null || true; \
    pkill -f 'fetch-results.sh' 2>/dev/null || true; \
    pkill -f 'start-slave.sh' 2>/dev/null || true; \
    pkill -f discoverymaster 2>/dev/null || true; \
    pkill -f discoveryslave 2>/dev/null || true; \
    pkill -f orderingpeer 2>/dev/null || true; \
    pkill -f orderingclient 2>/dev/null || true; \
    pkill -f 'scp ' 2>/dev/null || true; \
    pkill -f rsync 2>/dev/null || true; \
    tc qdisc del dev eth0 root tbf 2>/dev/null || true; \
  " 2> "$debug_dir/reset-proc-${ctrl_ip}.stderr"; then
    info "[reset-proc] ${ctrl_ip}: OK"
  else
    warn "[reset-proc] ${ctrl_ip}: ssh falhou (continuando). stderr em $debug_dir/reset-proc-${ctrl_ip}.stderr"
  fi
done < "$instance_info_file"

echo
info "Resetando state (remove arquivos antigos do experimento)..."

while read -r instance_id ctrl_ip data_ip role itag; do
  [[ -z "${instance_id:-}" ]] && continue
  [[ "${instance_id}" =~ ^[[:space:]]*# ]] && continue

  info "[reset-state] ${ctrl_ip}: limpando..."
  if ssh $ssh_options "${remote_user}@${ctrl_ip}" "\
    tc qdisc del dev eth0 root tbf 2>/dev/null || true; \
    killall -9 discoverymaster discoveryslave orderingpeer orderingclient scp rsync 2>/dev/null || true; \
    rm -rf $remote_delete_files 2>/dev/null || true; \
    mkdir -p '${remote_work_dir}' 2>/dev/null || true; \
  " 2> "$debug_dir/reset-state-${ctrl_ip}.stderr"; then
    info "[reset-state] ${ctrl_ip}: OK"
  else
    warn "[reset-state] ${ctrl_ip}: ssh falhou (continuando). stderr em $debug_dir/reset-state-${ctrl_ip}.stderr"
  fi
done < "$instance_info_file"

echo
info "Estado das máquinas remotas resetado."
echo

# =========================================================
# 4) Start master  (CORRIGIDO: ordem args)
# =========================================================

info "Starting master on $master_ip"
# start-master.sh deve ser: start-master.sh <exp_data_dir> <master_ip>
scripts/start-master.sh "$exp_data_dir" "$master_ip"
echo

# =========================================================
# 5) Start slaves
# =========================================================

count_tag_from_dpl() {
  local tag="$1"
  # deployment.dpl contém linhas do tipo:
  #   -1 <N> <tag> <machine_template>
  # Pegamos o primeiro N encontrado para o tag.
  awk -v t="${tag}" '($1==-1 && $3==t){print $2; exit}' "${deployment_file}" 2>/dev/null || echo 0
}

peers_tag="peers"
peers_n="$(count_tag_from_dpl "${peers_tag}")"
info "Starting peer slaves (tag=${peers_tag}, n=${peers_n})"
scripts/start-remote-slaves.sh "$exp_data_dir" "${peers_n:-0}" "$peers_tag" "$instance_info_file"

echo
clients_tag="1client"
clients_n="$(count_tag_from_dpl "${clients_tag}")"
info "Starting client slaves (tag=${clients_tag}, n=${clients_n})"
scripts/start-remote-slaves.sh "$exp_data_dir" "${clients_n:-0}" "$clients_tag" "$instance_info_file"

echo
info "All slaves started."

# =========================================================
# 6) Esperar término REAL (status_file) + fetch + validação
# =========================================================

csv_file="$exp_data_dir/deployment.csv"
[[ -f "$csv_file" ]] || die "deployment.csv não encontrado em $csv_file"

last_exp="$(awk -F',' 'NR>1{print $1}' "$csv_file" | tail -n 1)"
[[ -n "${last_exp}" ]] || die "não consegui detectar last_exp pelo deployment.csv"

info "[WAIT] Aguardando master marcar status final (last_exp=${last_exp})"
wait_timeout_sec="${DEPLOY_WAIT_TIMEOUT_SEC:-1800}"   # 30 min
poll_sec="${DEPLOY_WAIT_POLL_SEC:-5}"
start_ts="$(date +%s)"

diag_printed=0

while true; do
  now="$(date +%s)"
  if (( now - start_ts > wait_timeout_sec )); then
    err "Timeout aguardando status final no master."
    err "master=${master_ip} status_file=${remote_status_file} last_exp=${last_exp}"
    err "Dica: no master: ${remote_work_dir}/main_log.log ; ${remote_status_file} ; ${remote_ready_file}"
    exit 3
  fi

  status_val="$(ssh ${ssh_options} "${remote_user}@${master_ip}" "cat '${remote_status_file}' 2>/dev/null || true" </dev/null || true)"
  status_val="${status_val//$'\r'/}"
  status_val="${status_val//$'\n'/}"

  if [[ -z "${status_val}" ]]; then
    info "[WAIT] status atual='<vazio>' (aguardando '${last_exp}'), dormindo ${poll_sec}s..."

    # 1x diagnóstico rápido se estiver preso
    if (( diag_printed == 0 )); then
      diag_printed=1
      warn "[WAIT] status vazio. Imprimindo diagnóstico rápido do MASTER (1x)..."
      ssh ${ssh_options} "${remote_user}@${master_ip}" "\
        echo '--- master diag: ls workdir ---'; \
        ls -la '${remote_work_dir}' | head -n 80 || true; \
        echo '--- master diag: ls experiment-config ---'; \
        ls -la '${remote_work_dir}/experiment-config' | head -n 80 || true; \
        echo '--- master diag: tail main_log.log ---'; \
        tail -n 80 '${remote_work_dir}/main_log.log' 2>/dev/null || true; \
      " </dev/null || true
    fi

    sleep "${poll_sec}"
    continue
  fi

  info "[WAIT] status atual='${status_val}' (aguardando '${last_exp}')"

  if [[ "${status_val}" == "${last_exp}" ]]; then
    info "[WAIT] status final atingido: ${status_val}"
    break
  fi

  sleep "${poll_sec}"
done

echo
info "Buscando resultados (fetch-results.sh)..."
scripts/fetch-results.sh "$exp_data_dir" "$master_ip" || true

# Validação: tem que existir alguma evidência real no exp_data_dir
if compgen -G "${exp_data_dir}/experiment-output-*.tar.gz" > /dev/null; then
  info "OK: arquivos experiment-output-*.tar.gz encontrados."
else
  warn "Nenhum experiment-output-*.tar.gz encontrado em ${exp_data_dir}."
fi

if [[ -d "${exp_data_dir}/experiment-output" ]]; then
  info "OK: diretório experiment-output existe."
else
  warn "Diretório experiment-output NÃO existe em ${exp_data_dir}."
fi

# Falha estrutural se não houver evidência de outputs
if ! compgen -G "${exp_data_dir}/experiment-output-*.tar.gz" > /dev/null && [[ ! -d "${exp_data_dir}/experiment-output" ]]; then
  die "Sem outputs reais: nem experiment-output/ nem experiment-output-*.tar.gz. Deploy não executou end-to-end."
fi

info "Deploy remoto finalizado."

