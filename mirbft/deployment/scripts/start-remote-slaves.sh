#!/usr/bin/env bash

#
# start-remote-slaves.sh
#
# Propósito:
#   Inicia slaves remotos em instâncias EC2/Emulab para experimentos distribuídos.
#   Copia assets necessários (scripts, binários, TLS, configs) e dispara start-slave.sh.
#
# Uso:
#   start-remote-slaves.sh <exp_data_dir> <desired_count> <wanted_tag> <instance_info_file>
#
# Parâmetros:
#   exp_data_dir        - Diretório local com dados do experimento (contém config/)
#   desired_count       - Número de slaves a iniciar (0 = ilimitado)
#   wanted_tag          - Tag das instâncias a iniciar (ex: "peer", "client")
#   instance_info_file  - Arquivo com info das instâncias (id ctrl_ip data_ip role tag)
#
# Fluxo:
#   1. Lê instance-info e filtra por tag e role=slave
#   2. Para cada slave:
#      a. Cria diretórios remotos
#      b. Copia scripts de inicialização
#      c. Mata processos antigos
#      d. Copia binários (atomic, evita SIGBUS em NFS)
#      e. Copia certificados TLS
#      f. Copia configs do experimento
#      g. Verifica integridade dos assets
#      h. Dispara start-slave.sh via nohup
#
# Variáveis de ambiente importantes:
#   REMOTE_USER         - Usuário SSH remoto (default: $USER)
#   SSH_START_TIMEOUT   - Timeout para SSH (default: 12s)
#   FORCE_COPY_BINS     - Força cópia de binários mesmo em NFS (default: false)
#   remote_work_dir     - Diretório de trabalho remoto (default: /tmp/iss-$USER)
#   remote_base_dir     - Diretório base remoto (default: /users/$USER/iss)
#   remote_bin_dir      - Diretório de binários remoto (default: /users/$USER/go/bin)
#
# Notas:
#   - NÃO copia discoverymaster para slaves (evita overwrite no master)
#   - Detecta NFS compartilhado e pula cópia de binários para evitar SIGBUS
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
desired_count="$2"       # Número de slaves a iniciar (0 = todos)
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
# - remote_bin_dir: diretório de binários (/users/$USER/go/bin)
# - remote_exp_dir: diretório do experimento (usa remote_work_dir como root)
remote_work_dir="${remote_work_dir:-/tmp/iss-${remote_user}}"
remote_base_dir="${remote_base_dir:-/users/${remote_user}/iss}"
remote_bin_dir="${remote_bin_dir:-/users/${remote_user}/go/bin}"
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

# ------------------------------------------------------
# Funções principais
# ------------------------------------------------------

info "==== [start-remote-slaves] Iniciando ====="
info "  exp_data_dir       = ${exp_data_dir}"
info "  instance_info_file = ${instance_info_file}"
info "  wanted_tag         = ${wanted_tag}"
info "  desired_count      = ${desired_count}"
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
  info "[mkdir] ${ip}: criando estrutura de diretórios..."
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
  info "[mkdir] ${ip}: diretórios criados"
}

# Mata processos antigos (discoverymaster, discoveryslave, orderingpeer, orderingclient)
remote_kill_bins() {
  local ip="$1"
  info "[kill] ${ip}: matando processos antigos..."
  ssh ${ssh_options} "${remote_user}@${ip}" "\
    for p in discoverymaster discoveryslave orderingpeer orderingclient; do
      pkill -9 -f '${remote_bin_dir}/'\"\$p\" 2>/dev/null || true
      pkill -9 -f '\\b'\"\$p\"'\\b' 2>/dev/null || true
    done
  " </dev/null || true
  info "[kill] ${ip}: processos antigos eliminados"
}

# Copia binário de forma atômica (via .tmp) para evitar corrupção durante execução
copy_bin_atomic() {
  local ip="$1"
  local bin="$2"

  local local_path="${local_bin_dir}/${bin}"
  local remote_path="${remote_bin_dir}/${bin}"
  local remote_tmp="${remote_bin_dir}/.${bin}.tmp"

  if [[ ! -x "${local_path}" ]]; then
    err "[bin] binário obrigatório NÃO encontrado: ${local_path}"
    err "[bin] Execute: go install ./cmd/discoverymaster ./cmd/discoveryslave ./cmd/orderingpeer ./cmd/orderingclient"
    return 1
  fi

  info "[bin] ${ip}: copiando ${bin}..."
  ssh ${ssh_options} "${remote_user}@${ip}" "rm -f '${remote_tmp}'" </dev/null || true

  scp_with_retry "${scp_retries}" \
    "${local_path}" \
    "${remote_user}@${ip}:${remote_tmp}"

  ssh ${ssh_options} "${remote_user}@${ip}" "\
    mv -f '${remote_tmp}' '${remote_path}' && chmod +x '${remote_path}'
  " </dev/null
  
  info "[bin] ${ip}: ${bin} copiado com sucesso"
}

# Copia certificados TLS para o slave remoto
copy_tls_assets() {
  local ip="$1"

  if [[ ! -d "${local_tls_dir}" ]]; then
    warn "[tls] Diretório TLS local NÃO encontrado: ${local_tls_dir}"
    return 0
  fi

  info "[tls] ${ip}: copiando certificados TLS..."
  ssh ${ssh_options} "${remote_user}@${ip}" "\
    mkdir -p '${remote_base_dir}/tls-data' 2>/dev/null || true
  " </dev/null || true

  local count=0
  for f in "${local_tls_dir}"/*; do
    [[ -f "${f}" ]] || continue
    local base
    base="$(basename "${f}")"

    scp_with_retry "${scp_retries}" \
      "${f}" \
      "${remote_user}@${ip}:${remote_base_dir}/tls-data/${base}"

    ((count++)) || true
  done

  info "[tls] ${ip}: ${count} arquivos TLS copiados"
}

# Copia configs do experimento (membership, 1client, etc) para o slave remoto
copy_experiment_configs() {
  local ip="$1"

  if [[ ! -d "${local_exp_config_dir}" ]]; then
    warn "[config] Diretório de configs NÃO encontrado: ${local_exp_config_dir}"
    return 0
  fi

  info "[config] ${ip}: copiando configs do experimento..."
  ssh ${ssh_options} "${remote_user}@${ip}" "\
    mkdir -p '${remote_exp_config_dir}' 2>/dev/null || true
  " </dev/null || true

  local count=0
  for f in "${local_exp_config_dir}"/*; do
    [[ -f "${f}" ]] || continue
    local base
    base="$(basename "${f}")"

    scp_with_retry "${scp_retries}" \
      "${f}" \
      "${remote_user}@${ip}:${remote_exp_config_dir}/${base}"

    ((count++)) || true
  done

  info "[config] ${ip}: ${count} arquivos de config copiados"
}

# Verifica se todos os assets necessários estão presentes no slave remoto
# Retorna 0 se OK, 1 se faltam arquivos críticos
remote_check_assets() {
  local ip="$1"
  
  info "[check] ${ip}: verificando assets remotos..."
  
  # Conta arquivos em cada diretório e verifica binários/certificados obrigatórios
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
  
  # Verifica se algum arquivo crítico está faltando
  if echo "${check_output}" | grep -q "MISSING"; then
    err "[check] ${ip}: assets críticos faltando (veja acima)"
    return 1
  fi
  
  info "[check] ${ip}: todos os assets OK"
  return 0
}

# Copia todos os assets necessários para o slave remoto:
# - Scripts de inicialização (start-slave.sh, global-vars.sh)
# - Binários (discoveryslave, orderingpeer, orderingclient)
# - Certificados TLS (ca.pem, auth.pem, auth.key, etc)
# - Configs do experimento (membership, 1client, etc)
copy_required_assets() {
  local ip="$1"
  
  info "[copy] ${ip}: iniciando cópia de assets..."

  # 1. Cria estrutura de diretórios no remoto
  info "[copy] ${ip}: criando diretórios remotos..."
  remote_mkdirs "${ip}"

  # 2. Copia scripts de inicialização
  info "[copy] ${ip}: copiando scripts de inicialização..."
  scp_with_retry "${scp_retries}" \
    "${this_dir}/start-slave.sh" \
    "${remote_user}@${ip}:${remote_work_dir}/scripts/start-slave.sh"

  scp_with_retry "${scp_retries}" \
    "${this_dir}/global-vars.sh" \
    "${remote_user}@${ip}:${remote_work_dir}/scripts/global-vars.sh"

  ssh ${ssh_options} "${remote_user}@${ip}" "\
    chmod +x '${remote_work_dir}/scripts/start-slave.sh' \
    2>/dev/null || true
  " </dev/null || true

  # 3. Mata processos antigos antes de copiar novos binários
  info "[copy] ${ip}: matando processos antigos..."
  remote_kill_bins "${ip}"

  # -------------------------------------------------------------------------
  # FIX: evitar sobrescrita concorrente em FS compartilhado (NFS)
  # -------------------------------------------------------------------------
  # Em testbeds como Emulab, /users/<user>/go/bin é NFS compartilhado.
  # Copiar binários enquanto outros nodes os executam causa SIGBUS.
  # Solução: se remote_bin_dir == /users/.../go/bin, assume que binários
  # já estão instalados e pula a cópia (a menos que FORCE_COPY_BINS=true).

  force_copy_bins="${FORCE_COPY_BINS:-false}"
  shared_go_bin=false
  if [[ "${remote_bin_dir}" == "/users/${remote_user}/go/bin" ]]; then
    shared_go_bin=true
  fi

  # 4. Copia binários (se necessário)
  if [[ "${force_copy_bins}" == "true" || "${shared_go_bin}" == "false" ]]; then
    info "[copy] ${ip}: copiando binários para ${remote_bin_dir}..."
    # NÃO copiar discoverymaster para slaves (evita overwrite no master)
    copy_bin_atomic "${ip}" discoveryslave
    copy_bin_atomic "${ip}" orderingpeer
    copy_bin_atomic "${ip}" orderingclient
  else
    info "[copy] ${ip}: PULANDO cópia de binários (FS compartilhado detectado: ${remote_bin_dir})"
    info "[copy] ${ip}: assumindo que binários já estão instalados. Use FORCE_COPY_BINS=true para forçar."
  fi

  # 5. Copia certificados TLS
  info "[copy] ${ip}: copiando certificados TLS..."
  copy_tls_assets "${ip}"
  
  # 6. Copia configs do experimento (membership, 1client, etc)
  info "[copy] ${ip}: copiando configs do experimento..."
  copy_experiment_configs "${ip}"

  # 7. Verifica se tudo foi copiado corretamente
  info "[copy] ${ip}: verificando integridade dos assets..."
  remote_check_assets "${ip}" || {
    err "[copy] ${ip}: FALHA na verificação de assets. Slave pode não iniciar corretamente."
    return 0
  }
  
  info "[copy] ${ip}: cópia de assets concluída com sucesso"
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

  info "========================================"
  info "[slave] Iniciando slave ${instance_id}"
  info "[slave]   ctrl_ip  = ${ctrl_ip}"
  info "[slave]   data_ip  = ${data_ip}"
  info "[slave]   role     = ${role}"
  info "[slave]   tag      = ${tag}"
  info "========================================"

  # Copia scripts, binários, TLS e configs para o slave
  copy_required_assets "${ctrl_ip}"

  local _master_ip="${master_ip}"

  # Comando remoto: dispara start-slave.sh em background via nohup
  # Logs vão para ${remote_work_dir}/logs/start-slave-${instance_id}.log
  local remote_cmd="
    cd '${remote_work_dir}'
    echo '[start-remote-slaves] Disparando slave ${instance_id} (tag=${tag}) em ${ctrl_ip}...' >> '${remote_work_dir}/logs/start-remote-slaves-${instance_id}.log'
    /usr/bin/nohup '${remote_work_dir}/scripts/start-slave.sh' '${tag}' '${_master_ip}' '${ctrl_ip}' '${data_ip}' '${remote_exp_dir}' \
      >> '${remote_work_dir}/logs/start-slave-${instance_id}.log' 2>&1 < /dev/null &
    echo STARTED
  "

  info "[slave] ${instance_id}: disparando start-slave.sh via SSH (timeout=${SSH_START_TIMEOUT})..."

  local out=""
  out="$( (timeout "${SSH_START_TIMEOUT}" ssh ${ssh_options} "${remote_user}@${ctrl_ip}" "${remote_cmd}" </dev/null) 2>&1 || true )"

  # Verifica se o processo foi iniciado (procura por "STARTED" na saída)
  if echo "${out}" | grep -q "STARTED"; then
    info "[slave] ${instance_id}: SUCESSO - slave iniciado em ${ctrl_ip}"
    echo "${out}"
    return 0
  fi

  err "[slave] ${instance_id}: FALHA ao disparar slave em ${ctrl_ip}"
  err "[slave] ${instance_id}: saída do SSH:"
  echo "${out}" >&2
  return 0
}

# Contadores para estatísticas finais
total=0      # Total de linhas lidas do instance-info
matched=0    # Linhas com tag correspondente
started=0    # Slaves efetivamente iniciados

# Loop principal: lê instance-info e inicia slaves
while read -r instance_id ctrl_ip data_ip role tag rest; do
  # Pula linhas vazias e comentários
  [[ -z "${instance_id:-}" ]] && continue
  [[ "${instance_id}" =~ ^# ]] && continue

  ((total++)) || true

  # Filtra por tag (ex: "peer", "client")
  if [[ "${tag}" != "${wanted_tag}" ]]; then
    continue
  fi

  ((matched++)) || true

  # Limita número de slaves iniciados (0 = ilimitado)
  if [[ "${desired_count}" -ne 0 && "${started}" -ge "${desired_count}" ]]; then
    continue
  fi

  # Só inicia se role=slave (pula master)
  if [[ "${role}" != "slave" ]]; then
    continue
  fi

  start_remote_slave "${instance_id}" "${ctrl_ip}" "${data_ip}" "${role}" "${tag}"
  ((started++)) || true
done < "${instance_info_file}"

# Exibe estatísticas finais
info "==========================================="
info "[resumo] Total de linhas lidas: ${total}"
info "[resumo] Com tag=${wanted_tag}: ${matched}"
info "[resumo] Slaves iniciados: ${started}"
info "==========================================="

exit 0
