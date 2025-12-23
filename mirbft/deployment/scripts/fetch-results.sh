#!/bin/bash

source scripts/global-vars.sh
source scripts/remote-commands.sh
source scripts/logging.sh

master_ip="${1:-${MASTER_IP:-}}"
exp_dir="${2:-${exp_data_dir:-}}"
instance_info="${3:-${instance_info:-}}"

if [[ -z "${master_ip}" || -z "${exp_dir}" ]]; then
  echo "Usage: $0 <master-ip> <exp-data-dir> [instance-info]"
  echo "Example: $0 172.20.5.5 deployment-data/remote-0000 scripts/instance-info"
  exit 1
fi

if [[ -z "${instance_info}" ]]; then
  echo "ERROR: instance-info não informado (3º argumento) e variável instance_info não setada."
  exit 1
fi

mkdir -p "${exp_dir}/experiment-output" "${exp_dir}/_fetched_tars" "${exp_dir}/_debug"

# --------------------------------------------------------------------
# Local canonical results location
#
# We keep a single canonical place for results on the controller node:
#   /users/<remote_user>/iss/experiment-output
#   /users/<remote_user>/iss/raw-results
#
# This avoids confusion with deployment-data/... and survives reboot/deploy.
# You can override with ISS_ROOT=/some/path if needed.
# --------------------------------------------------------------------
ISS_ROOT="${ISS_ROOT:-/users/${remote_user}/iss}"
LOCAL_EXPERIMENT_OUTPUT_DIR="${ISS_ROOT}/experiment-output"
LOCAL_RAW_RESULTS_DIR="${ISS_ROOT}/raw-results"

mkdir -p "${LOCAL_EXPERIMENT_OUTPUT_DIR}" "${LOCAL_RAW_RESULTS_DIR}"

info "fetch-results: master_ip=${master_ip} exp_dir=${exp_dir}"
info "Canonical local results: ${LOCAL_EXPERIMENT_OUTPUT_DIR} and ${LOCAL_RAW_RESULTS_DIR}"

# ------------------------------------------------------------
# 1) Descobre lista de máquinas do deployment (peers+clientes)
# ------------------------------------------------------------
info "Lendo IPs a partir de instance-info: ${instance_info}"

# Espera-se que o instance-info tenha linhas tipo:
# master: 172.20.x.x
# 0: 172.20.x.x
# 1: 172.20.x.x
# client: 172.20.x.x
#
# Vamos extrair todos os IPs exceto o master (vamos incluir o master também, se ele tiver resultados).
all_ips=$(
  awk '
    NF>=2 {
      # pega o último campo se parecer IP
      for (i=1;i<=NF;i++) {
        if ($i ~ /^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$/) print $i
      }
    }
  ' "${instance_info}" | sort -u
)

if [[ -z "${all_ips}" ]]; then
  echo "ERROR: não consegui extrair IPs de ${instance_info}"
  exit 1
fi

info "IPs encontrados:"
echo "${all_ips}" | sed 's/^/  - /'

# ------------------------------------------------------------
# 2) Puxa os tars de logs/resultados e extrai
# ------------------------------------------------------------
# Os slaves costumam gerar /users/<user>/iss/current-deployment-data/experiment-output-000X.tar.gz
# Vamos buscar todos os experiment-output-*.tar.gz (remote_log_archives).
info "Buscando archives remotos: ${remote_log_archives}"
info "Diretório remoto de onde vamos puxar: ${remote_exp_dir}"

# Faz fetch de cada IP
while read -r ip; do
  [[ -z "${ip}" ]] && continue
  info "== Fetch from ${ip} =="

  # lista tars no remoto (pode ser vazio)
  ssh_cmd="ls -1 ${remote_exp_dir}/${remote_log_archives} 2>/dev/null || true"
  remote_tars=$(ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null "${remote_user}@${ip}" "${ssh_cmd}" | tr -d '\r')

  if [[ -z "${remote_tars}" ]]; then
    info "Nenhum tar encontrado em ${ip}:${remote_exp_dir}/${remote_log_archives}"
    continue
  fi

  info "Tars encontrados em ${ip}:"
  echo "${remote_tars}" | sed 's/^/  - /'

  while read -r tarpath; do
    [[ -z "${tarpath}" ]] && continue
    tarname=$(basename "${tarpath}")
    local_tar="${exp_dir}/_fetched_tars/${ip}--${tarname}"

    info "Baixando ${ip}:${tarpath} -> ${local_tar}"
    scp -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
      "${remote_user}@${ip}:${tarpath}" "${local_tar}" || {
        info "WARN: falha ao scp ${ip}:${tarpath}"
        continue
      }

    info "Extraindo ${local_tar} em ${exp_dir}"
    tar -xzf "${local_tar}" -C "${exp_dir}" || {
      info "WARN: falha ao extrair ${local_tar}"
      continue
    }
  done <<< "${remote_tars}"

done <<< "${all_ips}"

# ------------------------------------------------------------
# 3) Fallback: se não existirem tars, tenta rsync direto do experiment-output/
# ------------------------------------------------------------
count_dirs=$(find "${exp_dir}/experiment-output" -mindepth 1 -maxdepth 1 -type d 2>/dev/null | wc -l | tr -d ' ')
if [[ "${count_dirs}" -eq 0 ]]; then
  info "Nenhum subdir em ${exp_dir}/experiment-output após extrair tars. Tentando rsync direto do experiment-output/ remoto."

  while read -r ip; do
    [[ -z "${ip}" ]] && continue

    info "Rsync direto de ${ip}:${remote_exp_dir}/experiment-output/ ..."
    rsync -az --ignore-missing-args \
      -e "ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null" \
      "${remote_user}@${ip}:${remote_exp_dir}/experiment-output/" \
      "${exp_dir}/experiment-output/" || true

  done <<< "${all_ips}"
fi

# ------------------------------------------------------------
# 4) Verificação mínima
# ------------------------------------------------------------
count_dirs=$(find "${exp_dir}/experiment-output" -mindepth 1 -maxdepth 1 -type d 2>/dev/null | wc -l | tr -d ' ')
if [[ "${count_dirs}" -eq 0 ]]; then
  echo "ERROR: Ainda não há dados em ${exp_dir}/experiment-output."
  echo "Dicas:"
  echo "  - Verifique se os peers realmente geraram peer.log/peer.trc"
  echo "  - Verifique permissões em ${remote_exp_dir} no remoto"
  echo "  - Veja ${exp_dir}/_debug e logs do deploy"
  exit 2
fi

info "OK: experiment-output contém dados (${count_dirs} dirs)."
info "Exemplos de arquivos (head):"
find "${exp_dir}/experiment-output" -maxdepth 4 -type f | head -n 120 || true

# --------------------------------------------------------------------
# 5) Canonicalize results location on the local controller node
#
# Mirror what we fetched into /users/<user>/iss/{experiment-output,raw-results}
# so the user always knows where to look, regardless of deployment-data run id.
# --------------------------------------------------------------------
info "Mirroring to canonical ISS dirs..."
info "  -> ${LOCAL_EXPERIMENT_OUTPUT_DIR}"
rsync -a --delete "${exp_dir}/experiment-output/" "${LOCAL_EXPERIMENT_OUTPUT_DIR}/" || true

# raw-results is optional; if it exists, mirror it too.
if [[ -d "${exp_dir}/raw-results" ]]; then
  info "  -> ${LOCAL_RAW_RESULTS_DIR}"
  rsync -a --delete "${exp_dir}/raw-results/" "${LOCAL_RAW_RESULTS_DIR}/" || true
fi

# Helpful pointer file
{
  echo "Fetched at: $(date -Is)"
  echo "From exp_dir: ${exp_dir}"
  echo "Master IP: ${master_ip}"
  echo "Instance-info: ${instance_info}"
} > "${LOCAL_EXPERIMENT_OUTPUT_DIR}/.last_fetch_info" 2>/dev/null || true

info "Done. Canonical results are now in: ${LOCAL_EXPERIMENT_OUTPUT_DIR}"

