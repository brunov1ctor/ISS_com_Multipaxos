#!/usr/bin/env bash

#
# start-remote-slaves.sh
#
# Responsável por:
#   1. Copiar scripts/binários para os slaves remotos
#   2. Disparar o script start-slave.sh em cada slave
#

set -euo pipefail

this_dir="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
deployment_dir="$( cd "${this_dir}/.." && pwd )"
repo_dir="$( cd "${deployment_dir}/.." && pwd )"

source "${this_dir}/global-vars.sh"

tag="$1"

log_info "==== [start-remote-slaves] Diretórios detectados ====="
log_info "  this_dir       = ${this_dir}"
log_info "  deployment_dir = ${deployment_dir}"
log_info "  repo_dir       = ${repo_dir}"

# Usuário remoto (pode ser sobrescrito com DEPL_REMOTE_USER)
remote_user="${DEPL_REMOTE_USER:-$USER}"

log_info "  remote_user    = ${remote_user}"
log_info "  remote_gopath  = ${remote_gopath}"
log_info "  remote_bin_dir = ${remote_bin_dir}"
log_info "  remote_work_dir= ${remote_work_dir}"
log_info "  remote_exp_dir = ${remote_exp_dir}"
log_info

# Arquivo com informações das instâncias (gerado pelo deploy)
instance_info_file="${deployment_dir}/deployment-data/remote-0000/cloud-instance-info"

if [[ ! -f "${instance_info_file}" ]]; then
  log_error "Arquivo de instance info não encontrado: ${instance_info_file}"
  exit 1
fi

log_info "Usando instance info: ${instance_info_file}"

# ---------------------------------------------------------------------------
# Função auxiliar para iterar sobre as instâncias de uma tag específica
# Formato esperado (por linha, exemplo):
#   public_ip private_ip instance_id tag
# ---------------------------------------------------------------------------
iterate_instances() {
  local wanted_tag="$1"
  while read -r public_ip private_ip instance_id instance_tag; do
    [[ -z "${public_ip}" ]] && continue
    [[ "${instance_tag}" != "${wanted_tag}" ]] && continue
    echo "${public_ip} ${private_ip} ${instance_id} ${instance_tag}"
  done < <(grep -v '^\s*$' "${instance_info_file}")
}

# ---------------------------------------------------------------------------
# Copiar scripts + binários para uma instância
# ---------------------------------------------------------------------------
distribute_to_instance() {
  local public_ip="$1"
  local private_ip="$2"
  local instance_id="$3"
  local instance_tag="$4"

  log_info "---------------------------------------------------------------------"
  log_info "  [REMOTO] Garantindo ambiente em ${public_ip}"
  log_info "           instance_id = ${instance_id}"
  log_info "           tag         = ${instance_tag}"
  log_info "---------------------------------------------------------------------"

  # Garante diretórios no remoto
  ssh ${ssh_options} "${remote_user}@${public_ip}" "mkdir -p '${remote_work_dir}' '${remote_exp_dir}' '${remote_bin_dir}'"

  # Copia scripts auxiliares
  scp ${scp_opts} \
    "${this_dir}/start-slave.sh" \
    "${this_dir}/global-vars.sh" \
    "${this_dir}/remote-machine-status.sh" \
    "${this_dir}/stubborn-scp.sh" \
    "${remote_user}@${public_ip}:${remote_work_dir}/"

  # Copia binários necessários
  scp ${scp_opts} \
    "${remote_bin_dir}/discoverymaster" \
    "${remote_bin_dir}/discoveryslave" \
    "${remote_bin_dir}/orderingpeer" \
    "${remote_bin_dir}/orderingclient" \
    "${remote_user}@${public_ip}:${remote_bin_dir}/"

  log_info "  [REMOTO] OK: ambiente garantido em ${public_ip}."
}

# ---------------------------------------------------------------------------
# Dispara o start-slave.sh em uma instância
# ---------------------------------------------------------------------------
start_slave_on_instance() {
  local public_ip="$1"
  local private_ip="$2"
  local instance_id="$3"
  local instance_tag="$4"
  local master_ip="$5"

  log_info "  [DEPLOY] Iniciando slave em ${public_ip} (instance_id=${instance_id}, tag=${instance_tag})"

  ssh ${ssh_options} "${remote_user}@${public_ip}" "
    cd '${remote_work_dir}' && \
    chmod +x start-slave.sh && \
    ./start-slave.sh '${instance_tag}' '${master_ip}' '${public_ip}' '${private_ip}' > start-slave-${instance_tag}.log 2>&1 &
  "
}

# ---------------------------------------------------------------------------
# Distribuição de scripts/binários
# ---------------------------------------------------------------------------
log_info "==== [start-remote-slaves] Distribuindo scripts/binários aos slaves ===="

while read -r public_ip private_ip instance_id instance_tag; do
  distribute_to_instance "${public_ip}" "${private_ip}" "${instance_id}" "${instance_tag}"
done < <(iterate_instances "${tag}")

log_info "==== [start-remote-slaves] Distribuição concluída. ===="
log_info

# ---------------------------------------------------------------------------
# Disparo propriamente dito
# ---------------------------------------------------------------------------

log_info "==== [start-remote-slaves] Disparando slaves da tag '${tag}' ===="

master_ip="${MASTER_IP:-$(awk 'NR==1 {print $1}' \"${instance_info_file}\")}"

while read -r public_ip private_ip instance_id instance_tag; do
  start_slave_on_instance "${public_ip}" "${private_ip}" "${instance_id}" "${instance_tag}" "${master_ip}"
done < <(iterate_instances "${tag}")

log_info
log_info "==== [start-remote-slaves] Todos os slaves disparados. ===="
log_info "==== [start-remote-slaves] FIM ==========================================="

