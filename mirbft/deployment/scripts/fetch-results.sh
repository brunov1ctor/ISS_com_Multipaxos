#!/bin/bash
# fetch-results.sh
#
# Objetivo:
#  - Publicar (consolidar) resultados em: <publish_root>/<RUN>/slave-*/
#  - Preferir tarballs do MASTER quando existirem
#  - Se faltar trace (.trc) após extrair/rsync do master, FAZER fallback automático puxando dos SLAVES
#  - Opcional: rodar analyze.sh automaticamente para gerar .val (throughput/latência)
#
# Uso:
#   fetch-results.sh <master_ip> <publish_root>
# Ex.:
#   fetch-results.sh 172.20.5.5 /users/Bruno/iss/experiment-output
#
# Variáveis opcionais:
#   INSTANCE_INFO_FILE / instance_info_file : caminho para instance-info (para varrer slaves)
#   REMOTE_USER / remote_user               : usuário remoto
#   SSH_OPTIONS / ssh_options               : opções ssh
#   REMOTE_WORK_DIR / remote_work_dir       : root remoto (/users/<user>/iss)
#   RUN_ANALYZE=1|0                         : default 1 (tenta gerar .val)
#   ANALYZE_BIN=...                         : default scripts/analyze/analyze.sh
#   FORCE_SLAVE_FALLBACK=1                  : sempre puxa de slaves (mesmo se tar existir)

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

# Onde os slaves guardam “ao vivo”
slave_live_root="${remote_work_dir}/experiment-output"

RUN_ANALYZE="${RUN_ANALYZE:-1}"
ANALYZE_BIN="${ANALYZE_BIN:-scripts/analyze/analyze.sh}"
FORCE_SLAVE_FALLBACK="${FORCE_SLAVE_FALLBACK:-0}"

info "Fetch resultados: master=${master_ip} -> publish_root=${publish_root}"
info "remote_user=${remote_user}"
info "remote_work_dir=${remote_work_dir}"
info "ssh_options=${ssh_options}"
info "instance_info_file=${instance_info_file:-<vazio>}"
info "RUN_ANALYZE=${RUN_ANALYZE} ANALYZE_BIN=${ANALYZE_BIN}"
info "FORCE_SLAVE_FALLBACK=${FORCE_SLAVE_FALLBACK}"
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
  echo '--- find experiment-output tarballs (maxdepth 8) ---';
  find '${remote_work_dir}' -maxdepth 8 -type f -name 'experiment-output-*.tar.gz' 2>/dev/null | head -n 200 || true;
  echo '--- find experiment-output dirs (maxdepth 6) ---';
  find '${remote_work_dir}' -maxdepth 6 -type d -name 'experiment-output' 2>/dev/null | head -n 50 || true;
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
# 2) Extrair tars SEM NINHO em publish_root/<RUN>/slave-*/...
#    tarballs tipicamente contém: experiment-output/0000/slave-000/...
#    strip-components=2 => <RUN>/slave-000/...
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
  tar -xzf "${tarfile}" -C "${dst}" --strip-components=2 || {
    warn "[untar] Falhou extrair ${bn} (talvez layout diferente). Tentando sem strip..."
    tar -xzf "${tarfile}" -C "${dst}"
  }
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
# 3) Descobrir RUNs a considerar (do published + do slave)
# --------------------------------------------------------------------
discover_runs_from_published() {
  find "${publish_root}" -maxdepth 1 -type d -printf "%f\n" 2>/dev/null \
    | grep -E '^[0-9]{4}$' | sort || true
}

discover_runs_from_first_slave() {
  local first_slave_ip=""
  if [[ -n "${instance_info_file:-}" && -f "${instance_info_file}" ]]; then
    while read -r instance_id ctrl_ip data_ip role tag rest; do
      [[ -z "${instance_id:-}" ]] && continue
      [[ "${instance_id:-}" =~ ^# ]] && continue
      [[ "${role:-}" != "slave" ]] && continue
      first_slave_ip="${ctrl_ip}"
      break
    done < "${instance_info_file}"
  fi

  if [[ -n "${first_slave_ip}" ]] && remote_has_dir "${first_slave_ip}" "${slave_live_root}"; then
    ssh $ssh_options "${remote_user}@${first_slave_ip}" \
      "ls -1 '${slave_live_root}' 2>/dev/null | grep -E '^[0-9]{4}$' | sort" </dev/null || true
  fi
}

mapfile -t runs_published < <(discover_runs_from_published)
mapfile -t runs_slave < <(discover_runs_from_first_slave)

runs=()
if [[ ${#runs_published[@]} -gt 0 ]]; then
  runs+=("${runs_published[@]}")
fi
if [[ ${#runs_slave[@]} -gt 0 ]]; then
  runs+=("${runs_slave[@]}")
fi

# uniq
if [[ ${#runs[@]} -gt 0 ]]; then
  mapfile -t runs < <(printf "%s\n" "${runs[@]}" | sort -u)
else
  # fallback conservador
  runs=(0000 0001 0002 0003)
fi

info "RUNs detectados: ${runs[*]}"
echo

# --------------------------------------------------------------------
# 4) Verificar se cada RUN tem traces no published
# --------------------------------------------------------------------
run_has_traces_published() {
  local run="$1"
  # peer.trc pode estar dentro de slave-*/ ou em subdir, então maxdepth maior
  find "${publish_root}/${run}" -maxdepth 4 -type f \( -name 'peer.trc' -o -name '*.trc' \) -print -quit 2>/dev/null | grep -q .
}

# --------------------------------------------------------------------
# 5) Fallback: puxar traces/logs direto dos SLAVES para os RUNs que faltam
# --------------------------------------------------------------------
need_fallback_runs=()
for run in "${runs[@]}"; do
  if [[ "${FORCE_SLAVE_FALLBACK}" == "1" ]]; then
    need_fallback_runs+=("${run}")
    continue
  fi
  if ! run_has_traces_published "${run}"; then
    need_fallback_runs+=("${run}")
  fi
done

do_fallback=false
if [[ "${found_tar}" != "true" ]]; then
  do_fallback=true
fi
if [[ ${#need_fallback_runs[@]} -gt 0 ]]; then
  do_fallback=true
fi
if [[ "${FORCE_SLAVE_FALLBACK}" == "1" ]]; then
  do_fallback=true
fi

if [[ "${do_fallback}" == "true" ]]; then
  warn "Fallback habilitado: vou puxar dos SLAVES para RUN(s): ${need_fallback_runs[*]:-<tudo>}"
  if [[ -z "${instance_info_file:-}" || ! -f "${instance_info_file:-}" ]]; then
    err "instance_info_file não definido/encontrado; não dá para varrer slaves."
    err "Defina INSTANCE_INFO_FILE/instance_info_file ou rode a partir de um exp_data_dir que contenha instance-info."
    exit 2
  fi

  # Se need_fallback_runs vazio e fallback por falta de tar, usa todos runs
  if [[ ${#need_fallback_runs[@]} -eq 0 ]]; then
    need_fallback_runs=("${runs[@]}")
  fi

  while read -r instance_id ctrl_ip data_ip role tag rest; do
    [[ -z "${instance_id:-}" ]] && continue
    [[ "${instance_id:-}" =~ ^# ]] && continue
    [[ "${role:-}" != "slave" ]] && continue

    info "Fallback: slave ${instance_id} @ ${ctrl_ip}"
    for run in "${need_fallback_runs[@]}"; do
      dst_dir="${publish_root}/${run}"
      mkdir -p "${dst_dir}"

      # Copia o conteúdo de cada slave-*/ do run (inclui peer.trc, peer.log, prof, etc)
      src_glob="${slave_live_root}/${run}/slave-*"
      if remote_has_glob "${ctrl_ip}" "${src_glob}"; then
        info "  rsync ${ctrl_ip}:${src_glob} -> ${dst_dir}/"
        rsync -rtz --ignore-missing-args --progress -e "ssh $ssh_options" \
          "${remote_user}@${ctrl_ip}:${src_glob}" \
          "${dst_dir}/"
      else
        warn "  não existe: ${ctrl_ip}:${src_glob}"
      fi
    done
    echo
  done < "${instance_info_file}"
fi

# --------------------------------------------------------------------
# 6) Verificação final de traces
# --------------------------------------------------------------------
info "Verificando traces no publish_root (amostra)..."
find "${publish_root}" -maxdepth 4 -type f \( -name 'peer.trc' -o -name '*.trc' \) | head -n 50 || true
echo

missing=0
for run in "${runs[@]}"; do
  if run_has_traces_published "${run}"; then
    info "OK: RUN ${run} tem traces."
  else
    warn "FALTA: RUN ${run} não tem traces no published (${publish_root}/${run})."
    missing=$((missing+1))
  fi
done
echo

if [[ "${missing}" -gt 0 ]]; then
  warn "Ainda faltam traces em ${missing} RUN(s). Sem .trc não dá para gerar métricas (.val)."
fi

# --------------------------------------------------------------------
# 7) (Opcional) Rodar analyze.sh automaticamente para gerar .val
# --------------------------------------------------------------------
if [[ "${RUN_ANALYZE}" == "1" ]]; then
  if [[ ! -x "${ANALYZE_BIN}" ]]; then
    warn "RUN_ANALYZE=1, mas não encontrei executável: ${ANALYZE_BIN} (pulando análise)."
  else
    for run in "${runs[@]}"; do
      run_dir="${publish_root}/${run}"
      if [[ -d "${run_dir}" ]] && run_has_traces_published "${run}"; then
        info "[analyze] RUN ${run}: ${ANALYZE_BIN} ${run_dir}"
        set +e
        "${ANALYZE_BIN}" "${run_dir}" > "${run_dir}/analyze.log" 2>&1
        rc=$?
        set -e
        if [[ $rc -ne 0 ]]; then
          warn "[analyze] RUN ${run}: analyze retornou rc=${rc}. Veja ${run_dir}/analyze.log"
        else
          info "[analyze] RUN ${run}: OK. Gerados (se existirem) .val em ${run_dir}/"
        fi
      else
        warn "[analyze] RUN ${run}: sem traces ou sem diretório (${run_dir}) (pulando)."
      fi
    done
  fi
fi

# --------------------------------------------------------------------
# 8) Mostrar métricas encontradas
# --------------------------------------------------------------------
info "Listando .val (amostra)..."
find "${publish_root}" -maxdepth 2 -type f -name '*.val' -print | sort | head -n 200 || true
echo

info "fetch-results finalizado. Published em: ${publish_root}/<RUN>/slave-*/"
exit 0

