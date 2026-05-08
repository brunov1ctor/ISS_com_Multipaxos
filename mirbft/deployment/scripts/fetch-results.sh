#!/bin/bash
set -euo pipefail
shopt -s nullglob

# shellcheck source=/dev/null
source scripts/global-vars.sh

ts() { date +"%Y-%m-%d %H:%M:%S"; }
info(){ echo "[INFO  ][$(ts)] $*"; }
warn(){ echo "[WARN  ][$(ts)] $*"; }
err(){  echo "[ERRO  ][$(ts)] $*" >&2; }

master_ip="${1:-${MASTER_IP:-}}"
exp_dir="${2:-${exp_data_dir:-}}"

# Tenta inferir master_ip pelo instance-info, se necessário.
if [[ -z "${master_ip}" && -n "${instance_info_file:-}" && -f "${instance_info_file}" ]]; then
  master_ip="$(awk 'NF>=4 && $4=="master" {print $2; exit}' "${instance_info_file}" 2>/dev/null || true)"
fi

if [[ -z "${master_ip}" || -z "${exp_dir}" ]]; then
  err "fetch-results.sh precisa de master_ip e exp_dir."
  err "  master_ip='${master_ip:-}' exp_dir='${exp_dir:-}' instance_info_file='${instance_info_file:-}'"
  exit 1
fi

mkdir -p "${exp_dir}/experiment-output" "${exp_dir}/_fetched_tars" "${exp_dir}/_debug"

# Layout canônico (permite override via env exportado pelo deploy-remote.sh)
remote_work_dir="${remote_work_dir:-/tmp/iss-${remote_user}}"
# no layout atual NÃO existe current-deployment-data; tudo vive direto em ${remote_work_dir}
remote_exp_dir="${REMOTE_EXP_DIR:-${remote_exp_dir:-${remote_work_dir}}}"
remote_experiment_output_dir="${REMOTE_EXPERIMENT_OUTPUT_DIR:-${remote_experiment_output_dir:-${remote_work_dir}/experiment-output}}"

info "Iniciando fetch de resultados do master ${master_ip} para ${exp_dir}"
info "remote_user=${remote_user}"
info "remote_work_dir=${remote_work_dir}"
info "remote_exp_dir=${remote_exp_dir}"
info "remote_experiment_output_dir=${remote_experiment_output_dir}"
info "ssh_options=${ssh_options}"
echo

# --------------------------------------------------------------------
# Helpers: checar existência remota sem poluir log
# --------------------------------------------------------------------
remote_has_glob() {
  local ip="$1"
  local pat="$2"
  ssh $ssh_options "${remote_user}@${ip}" "ls -1 ${pat} >/dev/null 2>&1" </dev/null
}

remote_has_dir() {
  local ip="$1"
  local dir="$2"
  ssh $ssh_options "${remote_user}@${ip}" "test -d '${dir}'" </dev/null >/dev/null 2>&1
}

rsync_glob_if_exists() {
  local ip="$1"
  local pat="$2"
  local dst="$3"

  if remote_has_glob "$ip" "$pat"; then
    info "Baixando: ${ip}:${pat}"
    rsync -rtz --progress -e "ssh $ssh_options" \
      "${remote_user}@${ip}:${pat}" \
      "${dst}/"
    return 0
  else
    warn "Não existe no remoto: ${ip}:${pat}"
    return 1
  fi
}

rsync_dir_if_exists() {
  local ip="$1"
  local dir="$2"
  local dst="$3"

  if remote_has_dir "$ip" "$dir"; then
    info "Baixando dir: ${ip}:${dir}"
    rsync -rtz --progress -e "ssh $ssh_options" \
      "${remote_user}@${ip}:${dir%/}/" \
      "${dst}/"
    return 0
  else
    warn "Dir não existe no remoto: ${ip}:${dir}"
    return 1
  fi
}

# --------------------------------------------------------------------
# 0) Diagnóstico no master (não falha o script)
# --------------------------------------------------------------------
info "Diagnóstico no master: listando dirs e procurando outputs..."
ssh $ssh_options "${remote_user}@${master_ip}" "
  set -e;
  echo '--- work dir ---';
  ls -la '${remote_work_dir}' || true;
  echo '--- logs ---';
  ls -la '${remote_work_dir}/logs' 2>/dev/null || true;
  echo '--- raw-results ---';
  ls -la '${remote_work_dir}/raw-results' 2>/dev/null || true;
  echo '--- experiment-output (canônico) ---';
  ls -la '${remote_experiment_output_dir}' 2>/dev/null || true;
  echo '--- find experiment-output* (maxdepth 6) ---';
  find '${remote_work_dir}' -maxdepth 6 \
      \( -type d -name 'experiment-output' -o -type f -name 'experiment-output-*.tar.gz' \) \
      2>/dev/null | head -n 200 || true;
" </dev/null >"${exp_dir}/_debug/master-diag.txt" 2>&1 || true

info "Salvei diagnóstico do master em: ${exp_dir}/_debug/master-diag.txt"
echo

# --------------------------------------------------------------------
# 1) Busca .tar.gz em múltiplos paths
# --------------------------------------------------------------------
tar_paths=(
  "${remote_work_dir}/raw-results/experiment-output-*.tar.gz"
  "${remote_work_dir}/experiment-output-*.tar.gz"
)

found_tar=false
for pat in "${tar_paths[@]}"; do
  info "Tentando baixar tar(s) do master: ${pat}"
  if rsync_glob_if_exists "${master_ip}" "${pat}" "${exp_dir}/_fetched_tars"; then
    found_tar=true
  fi
  echo
done

# --------------------------------------------------------------------
# 2) Fallback: rsync de diretórios experiment-output (master e slaves)
# --------------------------------------------------------------------
dir_paths=(
  "${remote_experiment_output_dir}"
  "${remote_work_dir}/experiment-output"
)

if [[ "${found_tar}" != "true" ]]; then
  info "Nenhum tar encontrado. Fallback: tentando rsync de diretórios experiment-output..."
  echo

  info "Tentando no master primeiro..."
  for d in "${dir_paths[@]}"; do
    rsync_dir_if_exists "${master_ip}" "${d}" "${exp_dir}/experiment-output" || true
  done
  echo

  if [[ -z "${instance_info_file:-}" || ! -f "${instance_info_file:-}" ]]; then
    err "instance_info_file não definido/encontrado; não dá para varrer slaves."
    err "(sem tar e sem dir -> abortando)"
    exit 2
  fi

  while read -r instance_id ctrl_ip data_ip role tag; do
    [[ -z "${instance_id:-}" ]] && continue
    [[ "${instance_id:-}" =~ ^# ]] && continue
    [[ "${role:-}" != "slave" ]] && continue

    info "- slave ${instance_id} (${tag}) @ ${ctrl_ip}: tentando dirs..."
    for d in "${dir_paths[@]}"; do
      rsync_dir_if_exists "${ctrl_ip}" "${d}" "${exp_dir}/experiment-output" || true
    done
    echo
  done < "${instance_info_file}"
fi

# --------------------------------------------------------------------
# 2b) Garantir que traces de clientes sejam copiados
#     (clientes rodam em nós separados e o master não tem seus .trc)
# --------------------------------------------------------------------
if [[ -n "${instance_info_file:-}" && -f "${instance_info_file:-}" ]]; then
  while read -r instance_id ctrl_ip data_ip role tag; do
    [[ -z "${instance_id:-}" ]] && continue
    [[ "${instance_id:-}" =~ ^# ]] && continue
    [[ "${role:-}" != "slave" ]] && continue
    [[ "${tag:-}" == "peers" ]] && continue

    info "Copiando traces do cliente ${instance_id} (${tag}) @ ${ctrl_ip}..."
    rsync_dir_if_exists "${ctrl_ip}" "${remote_experiment_output_dir}" "${exp_dir}/experiment-output" || true
  done < "${instance_info_file}"
fi

# --------------------------------------------------------------------
# 3) Se baixamos tars, descompactar
#     Tarballs tipicamente contêm: experiment-output/0000/slave-000/...
#     Então strip-components=2 remove "experiment-output/<RUN>/".
# --------------------------------------------------------------------
local_tars=("${exp_dir}/_fetched_tars"/experiment-output-*.tar.gz)

if [[ ${#local_tars[@]} -gt 0 ]]; then
  info "Descompactando ${#local_tars[@]} tar(s) em ${exp_dir}/experiment-output/ ..."
  for t in "${local_tars[@]}"; do
    bn="$(basename "$t")"
    exp="$(echo "$bn" | sed -n 's/^experiment-output-\([0-9][0-9][0-9][0-9]\)-.*$/\1/p')"

    if [[ -n "${exp}" ]]; then
      info "[untar] ${bn} -> ${exp_dir}/experiment-output/${exp} (strip-components=2)"
      mkdir -p "${exp_dir}/experiment-output/${exp}"
      tar -xzf "$t" -C "${exp_dir}/experiment-output/${exp}" --strip-components=2
    else
      warn "Não consegui inferir expID de '${bn}'. Extraindo no root de experiment-output."
      info "[untar] ${bn} -> ${exp_dir}/experiment-output (sem expID; strip-components=1)"
      tar -xzf "$t" -C "${exp_dir}/experiment-output" --strip-components=1
    fi
  done
else
  info "Nenhum .tar.gz obtido para extrair (talvez os logs tenham vindo só via rsync de experiment-output/)."
fi

# --------------------------------------------------------------------
# 4) Verificação final
# --------------------------------------------------------------------
if [[ ! -d "${exp_dir}/experiment-output" ]]; then
  err "experiment-output não existe após fetch."
  exit 10
fi

count_dirs="$(find "${exp_dir}/experiment-output" -mindepth 2 -maxdepth 2 -type d 2>/dev/null | wc -l | tr -d ' ')"
if [[ "${count_dirs}" == "0" ]]; then
  err "experiment-output está vazio após fetch."
  err "Veja: ${exp_dir}/_debug/master-diag.txt"
  exit 11
fi

info "OK: experiment-output contém dados (${count_dirs} dirs)."
info "Exemplos de arquivos (head):"
find "${exp_dir}/experiment-output" -maxdepth 4 -type f | head -n 120 || true

info "fetch-results finalizado."

