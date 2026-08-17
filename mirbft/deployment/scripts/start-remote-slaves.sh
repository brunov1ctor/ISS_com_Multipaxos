#!/usr/bin/env bash

#
# start-remote-slaves.sh
#
# Propósito:
#   Inicia slaves remotos em instâncias EC2/Emulab para experimentos distribuídos.
#   Copia assets necessários (scripts, binários, TLS, configs) e dispara start-slave.sh.
#
# Uso:
#   start-remote-slaves.sh <exp_data_dir> <max_slaves> <wanted_tag> <instance_info_file>
#
# Parâmetros:
#   exp_data_dir        - Diretório local com dados do experimento (contém config/)
#   max_slaves          - Número máximo de slaves a iniciar (0 = todos)
#   wanted_tag          - Tag das instâncias a iniciar (ex: "peer", "client")
#   instance_info_file  - Arquivo com info das instâncias (id ctrl_ip data_ip role tag)
#
# Fluxo:
#   1. Lê instance-info e filtra por tag e role=slave
#   2. Para cada slave, dispara em PARALELO (background) uma sequência de:
#      a. Cria diretórios remotos
#      b. Copia scripts de inicialização
#      c. Mata processos antigos
#      d. Copia certificados TLS (transferência em bloco via scp -r)
#      e. Copia configs do experimento (transferência em bloco via scp -r)
#      f. Verifica integridade dos assets
#      g. Dispara start-slave.sh via nohup
#   3. Espera todos os jobs (wait por PID) e reporta sucesso/falha por nó
#
# Variáveis de ambiente importantes:
#   REMOTE_USER         - Usuário SSH remoto (default: $USER)
#   SSH_START_TIMEOUT   - Timeout para SSH (default: 12s)
#   FORCE_COPY_BINS     - Força cópia de binários mesmo em NFS (default: false)
#   remote_work_dir     - Diretório de trabalho remoto (default: /tmp/iss-$USER)
#   remote_base_dir     - Diretório base remoto (default: /users/$USER/iss)
#
# Notas:
#   - Binários já foram copiados por deploy-remote.sh para /tmp/iss-user/bin
#   - Usa cópia atômica (via .tmp) para evitar corrupção durante execução
#   - Logs remotos ficam em ${remote_work_dir}/logs/start-slave-${instance_id}.log
#

set -euo pipefail

# Funções auxiliares de logging com timestamp
ts() { date +"%Y-%m-%d %H:%M:%S%z"; }
info(){ echo "[INFO  ][$(ts)] $*"; }
warn(){ echo "[WARN  ][$(ts)] $*"; }
err(){  echo "[ERRO  ][$(ts)] $*" >&2; }

# Validação de argumentos
if [[ $# -lt 4 ]]; then
  echo "Uso: $0 <exp_data_dir> <desired_count> <wanted_tag> <instance_info_file>" >&2
  exit 2
fi

# Argumentos do script
exp_data_dir="$1"        # Diretório local com dados do experimento
max_slaves="$2"          # Número máximo de slaves a iniciar (0 = todos)
wanted_tag="$3"          # Tag das instâncias a filtrar (ex: "peer", "client")
instance_info_file="$4"  # Arquivo com info das instâncias

this_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Usuário SSH remoto (default: usuário local)
remote_user="${remote_user:-${REMOTE_USER:-${USER}}}"

# Opções SSH: desabilita verificações de host, timeouts agressivos
if [[ -z "${ssh_options:-}" ]]; then
  ssh_options="-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
-T -o BatchMode=yes -o ConnectTimeout=8 -o ConnectionAttempts=1 \
-o ServerAliveInterval=5 -o ServerAliveCountMax=2 -o LogLevel=ERROR \
-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
-o ControlMaster=no -o ControlPath=none -o ControlPersist=no"
fi

# Timeout para iniciar slave via SSH
SSH_START_TIMEOUT="${SSH_START_TIMEOUT:-12s}"

# Diretórios remotos:
# - remote_work_dir: diretório temporário de trabalho (/tmp/iss-$USER)
# - remote_base_dir: diretório base persistente (/users/$USER/iss)
# - remote_bin_dir: diretório de binários LOCAL (/tmp/iss-$USER/bin) - evita SIGBUS em NFS
# - remote_exp_dir: diretório do experimento (usa remote_work_dir como root)
remote_work_dir="${remote_work_dir:-/tmp/iss-${remote_user}}"
remote_base_dir="${remote_base_dir:-/users/${remote_user}/iss}"
remote_bin_dir="${remote_work_dir}/bin"
local_bin_dir="${local_bin_dir:-${GOBIN:-${HOME}/go/bin}}"
remote_exp_dir="${remote_work_dir}"

# Número de tentativas para SCP
scp_retries="${scp_retries:-10}"

# Diretórios locais:
# - local_tls_dir: certificados TLS (em deployment/tls-data)
# - local_exp_config_dir: configs do experimento (membership, 1client, etc)
local_tls_dir="$(cd "${this_dir}/.." && pwd)/tls-data"
local_exp_config_dir="${exp_data_dir}/config"

# Diretório remoto para configs do experimento
remote_exp_config_dir="${remote_base_dir}/experiment-config"

# IP do master (detectado automaticamente do instance-info)
master_ip="${master_ip:-}"

# Detecta o IP do master a partir do instance-info (role=master)
# Necessário para que os slaves saibam onde conectar
detect_master_ip() {
  if [[ -n "${master_ip}" ]]; then
    return 0
  fi

  if [[ ! -f "${instance_info_file}" ]]; then
    err "instance_info_file ${instance_info_file} não encontrado para detectar master_ip."
    return 1
  fi

  local ip
  ip="$(grep -v '^[[:space:]]*#' "${instance_info_file}" | awk '$4 == "master" { print $2; exit }' || true)"

  if [[ -z "${ip}" ]]; then
    err "Não foi possível detectar master_ip a partir de ${instance_info_file} (role=master não encontrado)."
    return 1
  fi

  master_ip="${ip}"
  info "master_ip detectado automaticamente a partir do instance-info: ${master_ip}"
  return 0
}

# Copia arquivo via SCP com retry automático
# Útil para lidar com falhas temporárias de rede
scp_with_retry() {
  local retries="$1"
  local src="$2"
  local dst="$3"

  local attempt=1

  while (( attempt <= retries )); do
    set +e
    scp -q \
      -o StrictHostKeyChecking=no \
      -o UserKnownHostsFile=/dev/null \
      -o BatchMode=yes \
      -o ConnectTimeout=8 \
      -o ConnectionAttempts=1 \
      -o LogLevel=ERROR \
      "${src}" "${dst}" </dev/null
    local status=$?
    set -e

    if (( status == 0 )); then
      return 0
    fi

    warn "[scp] Retry ${attempt}/${retries} (status ${status}): ${src} -> ${dst}"
    attempt=$((attempt + 1))
    sleep 0.3
  done

  err "[scp] FALHA: não foi possível copiar '${src}' -> '${dst}' após ${retries} tentativas."
  return 1
}

# Copia o CONTEÚDO de um diretório local para um diretório remoto via tar + ssh (uma única
# conexão), com retry automático. Evita depender da sintaxe "origem/." do scp, que quebra em
# versões modernas do OpenSSH (scp baseado em SFTP): "scp: error: unexpected filename: .".
copy_dir_with_retry() {
  local retries="$1"
  local local_dir="$2"
  local ip="$3"
  local remote_dir="$4"

  local attempt=1

  while (( attempt <= retries )); do
    set +e
    ssh ${ssh_options} "${remote_user}@${ip}" "mkdir -p '${remote_dir}'" </dev/null \
      && tar -C "${local_dir}" -cf - . \
         | ssh ${ssh_options} "${remote_user}@${ip}" "tar -C '${remote_dir}' -xf -"
    local status=$?
    set -e

    if (( status == 0 )); then
      return 0
    fi

    warn "[copy-dir] Retry ${attempt}/${retries} (status ${status}): ${local_dir} -> ${ip}:${remote_dir}"
    attempt=$((attempt + 1))
    sleep 0.3
  done

  err "[copy-dir] FALHA: não foi possível copiar '${local_dir}' -> '${ip}:${remote_dir}' após ${retries} tentativas."
  return 1
}

# ------------------------------------------------------
# Funções principais
# ------------------------------------------------------

info "==== [start-remote-slaves] Iniciando ====="
info "  exp_data_dir       = ${exp_data_dir}"
info "  instance_info_file = ${instance_info_file}"
info "  wanted_tag         = ${wanted_tag}"
info "  max_slaves         = ${max_slaves} (0=ilimitado)"
info "  remote_user        = ${remote_user}"
info "  remote_work_dir    = ${remote_work_dir}"
info "  remote_base_dir    = ${remote_base_dir}"
info "  remote_bin_dir     = ${remote_bin_dir}"
info "  remote_exp_dir     = ${remote_exp_dir}"
info "  local_bin_dir      = ${local_bin_dir}"
info "  SSH_START_TIMEOUT  = ${SSH_START_TIMEOUT}"
info "==========================================="
info ""

# Detecta IP do master antes de iniciar slaves
detect_master_ip || exit 1

# Cria diretórios necessários no slave remoto
remote_mkdirs() {
  local ip="$1"
  ssh ${ssh_options} "${remote_user}@${ip}" "\
    mkdir -p '${remote_work_dir}' \
             '${remote_work_dir}/scripts' \
             '${remote_work_dir}/logs' \
             '${remote_base_dir}' \
             '${remote_base_dir}/config' \
             '${remote_base_dir}/tls-data' \
             '${remote_exp_config_dir}' \
             '${remote_exp_dir}' \
             '${remote_bin_dir}' \
    2>/dev/null || true
  " </dev/null
}

# Mata processos antigos (discoverymaster, discoveryslave, orderingpeer, orderingclient)
remote_kill_bins() {
  local ip="$1"
  ssh ${ssh_options} "${remote_user}@${ip}" "\
    for p in discoverymaster discoveryslave orderingpeer orderingclient; do
      pkill -9 -f '${remote_bin_dir}/'\"\$p\" 2>/dev/null || true
      pkill -9 -f '\\b'\"\$p\"'\\b' 2>/dev/null || true
    done
  " </dev/null || true
}

# Copia certificados TLS para o slave remoto
copy_tls_assets() {
  local ip="$1"

  if [[ ! -d "${local_tls_dir}" ]]; then
    warn "[tls] Diretório TLS local NÃO encontrado: ${local_tls_dir}"
    return 0
  fi

  copy_dir_with_retry "${scp_retries}" \
    "${local_tls_dir}" \
    "${ip}" \
    "${remote_base_dir}/tls-data"

  local count
  count="$(find "${local_tls_dir}" -maxdepth 1 -type f | wc -l)"
  info "[tls] ${ip}: ${count} arquivos copiados (transferência em bloco)"
}

# Copia configs do experimento (membership, 1client, etc) para o slave remoto
copy_experiment_configs() {
  local ip="$1"

  if [[ ! -d "${local_exp_config_dir}" ]]; then
    warn "[config] Diretório de configs NÃO encontrado: ${local_exp_config_dir}"
    return 0
  fi

  copy_dir_with_retry "${scp_retries}" \
    "${local_exp_config_dir}" \
    "${ip}" \
    "${remote_exp_config_dir}"

  local count
  count="$(find "${local_exp_config_dir}" -maxdepth 1 -type f | wc -l)"
  info "[config] ${ip}: ${count} arquivos copiados (transferência em bloco)"
}

# Verifica se todos os assets necessários estão presentes no slave remoto
# Retorna 0 se OK, 1 se faltam arquivos críticos
remote_check_assets() {
  local ip="$1"
  
  local check_output
  check_output="$(ssh ${ssh_options} "${remote_user}@${ip}" "\
    echo -n 'scripts: '; ls -1 '${remote_work_dir}/scripts' 2>/dev/null | wc -l; \
    echo -n 'bins: '; ls -1 '${remote_bin_dir}' 2>/dev/null | wc -l; \
    echo -n 'tls-data: '; ls -1 '${remote_base_dir}/tls-data' 2>/dev/null | wc -l; \
    echo -n 'experiment-config: '; ls -1 '${remote_exp_config_dir}' 2>/dev/null | wc -l; \
    test -x '${remote_bin_dir}/discoveryslave' && echo 'discoveryslave: OK' || echo 'discoveryslave: MISSING'; \
    test -x '${remote_bin_dir}/orderingpeer' && echo 'orderingpeer: OK' || echo 'orderingpeer: MISSING'; \
    test -x '${remote_bin_dir}/orderingclient' && echo 'orderingclient: OK' || echo 'orderingclient: MISSING'; \
    test -f '${remote_base_dir}/tls-data/ca.pem' && echo 'ca.pem: OK' || echo 'ca.pem: MISSING'; \
    test -f '${remote_base_dir}/tls-data/auth.pem' && echo 'auth.pem: OK' || echo 'auth.pem: MISSING'; \
    test -f '${remote_base_dir}/tls-data/auth.key' && echo 'auth.key: OK' || echo 'auth.key: MISSING'
  " </dev/null)"
  
  info "[check] ${ip}: ${check_output}"
  
  if echo "${check_output}" | grep -q "MISSING"; then
    err "[check] ${ip}: assets críticos faltando"
    return 1
  fi
  
  return 0
}

# Copia todos os assets necessários para o slave remoto:
# - Scripts de inicialização (start-slave.sh, global-vars.sh)
# - Certificados TLS (ca.pem, auth.pem, auth.key, etc)
# - Configs do experimento (membership, 1client, etc)
# Nota: binários já foram copiados por deploy-remote.sh
copy_required_assets() {
  local ip="$1"
  
  info "[copy] ${ip}: iniciando cópia de assets..."

  remote_mkdirs "${ip}"

  scp_with_retry "${scp_retries}" \
    "${this_dir}/start-slave.sh" \
    "${remote_user}@${ip}:${remote_work_dir}/scripts/start-slave.sh"

  scp_with_retry "${scp_retries}" \
    "${this_dir}/global-vars.sh" \
    "${remote_user}@${ip}:${remote_work_dir}/scripts/global-vars.sh"

  scp_with_retry "${scp_retries}" \
    "${this_dir}/wait-quiescent.sh" \
    "${remote_user}@${ip}:${remote_work_dir}/scripts/wait-quiescent.sh"

  ssh ${ssh_options} "${remote_user}@${ip}" "\
    chmod +x '${remote_work_dir}/scripts/start-slave.sh' \
             '${remote_work_dir}/scripts/wait-quiescent.sh' \
    2>/dev/null || true
  " </dev/null || true

  remote_kill_bins "${ip}"

  copy_tls_assets "${ip}"
  copy_experiment_configs "${ip}"

  remote_check_assets "${ip}" || {
    err "[copy] ${ip}: FALHA na verificação de assets"
    return 1
  }
  
  info "[copy] ${ip}: concluído"
}

# Inicia um slave remoto:
# 1. Copia todos os assets necessários (scripts, bins, tls, configs)
# 2. Dispara start-slave.sh via nohup (processo em background)
# 3. Verifica se o processo foi iniciado com sucesso
start_remote_slave() {
  local instance_id="$1"
  local ctrl_ip="$2"
  local data_ip="$3"
  local role="$4"
  local tag="$5"

  info "[slave] Iniciando ${instance_id} @ ${ctrl_ip} (tag=${tag})"

  copy_required_assets "${ctrl_ip}" || {
    err "[slave] ${instance_id}: FALHA na cópia de assets"
    return 1
  }

  local _master_ip="${master_ip}"

  # Comando remoto: dispara start-slave.sh em background via nohup
  # Logs vão para ${remote_work_dir}/logs/start-slave-${instance_id}.log
  local remote_cmd="
    export remote_bin_dir='${remote_bin_dir}'
    export remote_work_dir='${remote_work_dir}'
    export remote_base_dir='${remote_base_dir}'
    cd '${remote_work_dir}'
    echo '[start-remote-slaves] Disparando slave ${instance_id} (tag=${tag}) em ${ctrl_ip}...' >> '${remote_work_dir}/logs/start-remote-slaves-${instance_id}.log'
    bash '${remote_work_dir}/scripts/start-slave.sh' '${tag}' '${_master_ip}' '${ctrl_ip}' '${data_ip}' '${remote_exp_dir}' \
      >> '${remote_work_dir}/logs/start-slave-${instance_id}.log' 2>&1 < /dev/null &
    echo STARTED
  "

  local out=""
  out="$( (timeout "${SSH_START_TIMEOUT}" ssh ${ssh_options} "${remote_user}@${ctrl_ip}" "${remote_cmd}" </dev/null) 2>&1 || true )"

  # Verifica se o processo foi iniciado (procura por "STARTED" na saída)
  if echo "${out}" | grep -q "STARTED"; then
    info "[slave] ${instance_id}: SUCESSO"
    return 0
  fi

  err "[slave] ${instance_id}: FALHA"
  echo "${out}" >&2
  return 1
}

# Contadores para estatísticas finais
total=0      # Total de linhas lidas do instance-info
matched=0    # Linhas com tag correspondente
launched=0   # Slaves disparados (em background, aguardando resultado)
succeeded=0  # Slaves confirmados com sucesso após wait
failed=0     # Slaves que falharam após wait

# Jobs em background: arrays paralelos (pid / instance_id / arquivo de log)
pids=()
pid_ids=()
pid_logs=()

# Garante que jobs locais órfãos e tempfiles sejam limpos se o script for interrompido.
# Não afeta o start-slave.sh remoto, que já roda desacoplado via nohup no lado do slave.
trap 'kill "${pids[@]}" 2>/dev/null || true; rm -f "${pid_logs[@]}" 2>/dev/null || true' EXIT

# Loop principal: lê instance-info e dispara os slaves em paralelo
while read -r instance_id ctrl_ip data_ip role tag rest || [[ -n "${instance_id}" ]]; do
  # Pula linhas vazias e comentários
  [[ -z "${instance_id:-}" ]] && continue
  [[ "${instance_id}" =~ ^# ]] && continue

  ((total++)) || true

  # Filtra por tag (ex: "peer", "client")
  if [[ "${tag}" != "${wanted_tag}" ]]; then
    continue
  fi

  ((matched++)) || true

  # Limita número de slaves disparados (0 = ilimitado)
  if [[ "${max_slaves}" -ne 0 && "${launched}" -ge "${max_slaves}" ]]; then
    continue
  fi

  # Só inicia se role=slave (pula master)
  if [[ "${role}" != "slave" ]]; then
    continue
  fi

  logfile="$(mktemp "${TMPDIR:-/tmp}/start-remote-slave.${instance_id}.XXXXXX.log")"
  ( start_remote_slave "${instance_id}" "${ctrl_ip}" "${data_ip}" "${role}" "${tag}" ) > "${logfile}" 2>&1 &

  pids+=("$!")
  pid_ids+=("${instance_id}")
  pid_logs+=("${logfile}")

  ((launched++)) || true
done < "${instance_info_file}"

# Espera cada job terminar e coleta o resultado real (exit code por PID)
for i in "${!pids[@]}"; do
  pid="${pids[$i]}"
  id="${pid_ids[$i]}"
  logfile="${pid_logs[$i]}"

  if wait "${pid}"; then
    info "[slave] ${id}: job concluído com SUCESSO"
    ((succeeded++)) || true
  else
    err "[slave] ${id}: job FALHOU"
    ((failed++)) || true
  fi

  sed "s/^/[${id}] /" "${logfile}" || true
  rm -f "${logfile}"
done

# Exibe estatísticas finais
info "==========================================="
info "[resumo] Total de linhas lidas: ${total}"
info "[resumo] Com tag=${wanted_tag}: ${matched}"
info "[resumo] Slaves disparados: ${launched}"
info "[resumo] Slaves iniciados com sucesso: ${succeeded}"
info "[resumo] Falhas: ${failed}"
info "==========================================="

if (( failed > 0 )); then
  err "[main] ${failed} slave(s) falharam ao iniciar."
  exit 1
fi

exit 0
