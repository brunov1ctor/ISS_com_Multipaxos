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
publish_root="${2:-}"

# instance-info (para varrer slaves no fallback)
instance_info_file="${instance_info_file:-${INSTANCE_INFO_FILE:-}}"

if [[ -z "${master_ip}" || -z "${publish_root}" ]]; then
  err "Uso: fetch-results.sh <master_ip> <publish_root>"
  err "Ex.: fetch-results.sh 172.20.5.5 /users/Bruno/iss/experiment-output"
  exit 1
fi

mkdir -p "${publish_root}" "${publish_root}/_fetched_tars" "${publish_root}/_debug"

# Defaults canônicos
remote_user="${remote_user:-${REMOTE_USER:-${USER}}}"
ssh_options="${ssh_options:--o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -T -o BatchMode=yes -o ConnectTimeout=8 -o ConnectionAttempts=1 -o ServerAliveInterval=5 -o ServerAliveCountMax=2 -o LogLevel=ERROR -o ControlMaster=no -o ControlPath=none -o ControlPersist=no}"

remote_work_dir="${remote_work_dir:-${REMOTE_WORK_DIR:-/users/${remote_user}/iss}}"

# Onde esperamos achar tarballs no MASTER (canônico + legados)
tar_paths=(
  "${remote_work_dir}/raw-results/experiment-output-*.tar.gz"
  "${remote_work_dir}/current-deployment-data/raw-results/experiment-output-*.tar.gz"
  "${remote_work_dir}/experiment-output-*.tar.gz"
)

# Onde os slaves guardam “ao vivo” (antes do master conseguir coletar)
# (isso bate com teu caso: /users/Bruno/iss/experiment-output/0000/slave-*/peer.trc existe nos slaves)
slave_live_root="${remote_work_dir}/experiment-output"

info "Fetch resultados: master=${master_ip} -> publish_root=${publish_root}"
info "remote_user=${remote_user}"
info "remote_work_dir=${remote_work_dir}"
info "ssh_options=${ssh_options}"
info "instance_info_file=${instance_info_file:-<vazio>}"
echo

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
    rsync -rtz --ignore-missing-args --progress -e "ssh $ssh_options" \
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
    rsync -rtz --ignore-missing-args --progress -e "ssh $ssh_options" \
      "${remote_user}@${ip}:${dir%/}/" \
      "${dst}/"
    return 0
  else
    warn "Dir não existe no remoto: ${ip}:${dir}"
    return 1
  fi
}

# --------------------------------------------------------------------
# 0) Diagnóstico do master (não falha)
# --------------------------------------------------------------------
info "Diagnóstico no master (salvando em _debug)..."
ssh $ssh_options "${remote_user}@${master_ip}" "
  set +e;
  echo '--- work dir ---';
  ls -la '${remote_work_dir}' || true;
  echo '--- raw-results (canônico) ---';
  ls -la '${remote_work_dir}/raw-results' 2>/dev/null || true;
  echo '--- current-deployment-data/raw-results (legado) ---';
  ls -la '${remote_work_dir}/current-deployment-data/raw-results' 2>/dev/null || true;
  echo '--- find experiment-output tarballs (maxdepth 6) ---';
  find '${remote_work_dir}' -maxdepth 6 -type f -name 'experiment-output-*.tar.gz' 2>/dev/null | head -n 200 || true;
" </dev/null >"${publish_root}/_debug/master-diag.txt" 2>&1 || true

info "OK: ${publish_root}/_debug/master-diag.txt"
echo

# --------------------------------------------------------------------
# 1) Baixar tarballs do MASTER (se existirem)
# --------------------------------------------------------------------
found_tar=false
for pat in "${tar_paths[@]}"; do
  info "Tentando baixar tar(s) do master: ${pat}"
  if rsync_glob_if_exists "${master_ip}" "${pat}" "${publish_root}/_fetched_tars"; then
    found_tar=true
  fi
  echo
done

# --------------------------------------------------------------------
# 2) Se temos tar(s), extrair SEM NINHO em publish_root/<RUN>/slave-*/...
#    Os tarballs tipicamente contém: experiment-output/0000/slave-000/...
#    Então strip-components=2 => <RUN>/slave-000/...
# --------------------------------------------------------------------
extract_tar_no_nest() {
  local tarfile="$1"
  local bn exp
  bn="$(basename "$tarfile")"
  exp="$(echo "$bn" | sed -n 's/^experiment-output-\([0-9][0-9][0-9][0-9]\)-.*$/\1/p')"

  if [[ -z "${exp}" ]]; then
    warn "Não consegui inferir RUN do tar '${bn}'. Vou tentar extrair em ${publish_root}/_unknown"
    exp="_unknown"
  fi

  local dst="${publish_root}/${exp}"
  mkdir -p "${dst}"

  info "[untar] ${bn} -> ${dst} (strip-components=2)"
  # strip 2: remove "experiment-output/<RUN>/" do começo
  tar -xzf "${tarfile}" -C "${dst}" --strip-components=2
}

local_tars=( "${publish_root}/_fetched_tars"/experiment-output-*.tar.gz )
if [[ ${#local_tars[@]} -gt 0 ]]; then
  info "Descompactando ${#local_tars[@]} tar(s)..."
  for t in "${local_tars[@]}"; do
    extract_tar_no_nest "$t"
  done
else
  warn "Nenhum tar obtido do master."
fi
echo

# --------------------------------------------------------------------
# 3) Fallback: puxar direto dos SLAVES (quando o master não recebeu os tar)
#    Copia experiment-output/<RUN>/slave-* (do slave) para publish_root/<RUN>/slave-*
# --------------------------------------------------------------------
fallback_from_slaves=false

if [[ "${found_tar}" != "true" ]]; then
  warn "Sem tar do master; habilitando fallback: rsync direto dos slaves."
  fallback_from_slaves=true
fi

if [[ "${fallback_from_slaves}" == "true" ]]; then
  if [[ -z "${instance_info_file:-}" || ! -f "${instance_info_file:-}" ]]; then
    err "instance_info_file não definido/encontrado; não dá para varrer slaves."
    exit 2
  fi

  # Descobre quais RUNs existem (percorre /experiment-output/<RUN> no PRIMEIRO slave que responder)
  # Se falhar, faz fallback para 0000..0003 (comum no teu deployment.dpl)
  runs=()
  first_slave_ip=""
  while read -r instance_id ctrl_ip data_ip role tag rest; do
    [[ -z "${instance_id:-}" ]] && continue
    [[ "${instance_id:-}" =~ ^# ]] && continue
    [[ "${role:-}" != "slave" ]] && continue
    first_slave_ip="${ctrl_ip}"
    break
  done < "${instance_info_file}"

  if [[ -n "${first_slave_ip}" ]] && remote_has_dir "${first_slave_ip}" "${slave_live_root}"; then
    mapfile -t runs < <(ssh $ssh_options "${remote_user}@${first_slave_ip}" "ls -1 '${slave_live_root}' 2>/dev/null | grep -E '^[0-9]{4}$' | sort" </dev/null || true)
  fi

  if [[ ${#runs[@]} -eq 0 ]]; then
    runs=(0000 0001 0002 0003)
  fi

  info "RUNs para coletar via fallback: ${runs[*]}"
  echo

  while read -r instance_id ctrl_ip data_ip role tag rest; do
    [[ -z "${instance_id:-}" ]] && continue
    [[ "${instance_id:-}" =~ ^# ]] && continue
    [[ "${role:-}" != "slave" ]] && continue

    info "Fallback: slave ${instance_id} @ ${ctrl_ip}"
    for run in "${runs[@]}"; do
      src_dir="${slave_live_root}/${run}/slave-*"
      dst_dir="${publish_root}/${run}"
      mkdir -p "${dst_dir}"

      if remote_has_glob "${ctrl_ip}" "${src_dir}"; then
        info "  rsync ${ctrl_ip}:${src_dir} -> ${dst_dir}/"
        rsync -rtz --ignore-missing-args --progress -e "ssh $ssh_options" \
          "${remote_user}@${ctrl_ip}:${src_dir}" \
          "${dst_dir}/"
      else
        warn "  não existe: ${ctrl_ip}:${src_dir}"
      fi
    done
    echo
  done < "${instance_info_file}"
fi

# --------------------------------------------------------------------
# 4) Verificação: garantir que publish_root/<RUN>/slave-*/peer.trc existe
# --------------------------------------------------------------------
info "Verificando traces no publish_root..."
find "${publish_root}" -maxdepth 4 -type f \( -name 'peer.trc' -o -name '*.trc' \) | head -n 50 || true
echo

info "fetch-results finalizado. Published em: ${publish_root}/<RUN>/slave-*/"
exit 0

