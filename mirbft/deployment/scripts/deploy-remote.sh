#!/usr/bin/env bash
set -euo pipefail

ts() { date +"%Y-%m-%d %H:%M:%S%z"; }
log_i() { echo "[INFO  ][$(ts)] $*"; }
log_w() { echo "[WARN  ][$(ts)] $*" >&2; }
log_e() { echo "[ERRO  ][$(ts)] $*" >&2; }

cancel_instances="${cancel_instances:-false}"
to_bool() { case "${1:-false}" in 1|true|TRUE|yes|YES|y|Y) echo "true" ;; *) echo "false" ;; esac; }
cancel_instances="$(to_bool "$cancel_instances")"

if [[ -z "${exp_data_dir:-}" || -z "${instance_info_file:-}" ]]; then
  log_e "exp_data_dir ou instance_info_file não definidos."
  log_e "exp_data_dir='${exp_data_dir:-}' instance_info_file='${instance_info_file:-}'"
  exit 1
fi

if command -v realpath >/dev/null 2>&1; then
  instance_info_file="$(realpath "${instance_info_file}")"
  exp_data_dir="$(realpath "${exp_data_dir}")"
fi

instance_info_file_name="$(basename "$instance_info_file")"

local_master_command_template_file="${local_master_command_template_file:-master-commands-template.cmd}"
local_master_command_file="${local_master_command_file:-master-commands.cmd}"
local_result_fetching_log="${local_result_fetching_log:-result-fetching.log}"

remote_user="${remote_user:-${USER}}"
remote_work_dir="${remote_work_dir:-/users/${remote_user}/iss}"
remote_bin_dir="${remote_bin_dir:-/users/${remote_user}/go/bin}"

ssh_options="${ssh_options:--o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -T -o BatchMode=yes -o ConnectTimeout=8 -o ConnectionAttempts=1 -o ServerAliveInterval=5 -o ServerAliveCountMax=2 -o LogLevel=ERROR -o ControlMaster=no -o ControlPath=none -o ControlPersist=no}"

remote_status_file="${remote_status_file:-${remote_work_dir}/status}"
DISC_PORT="${master_port:-${MASTER_PORT:-9999}}"

remote_exp_dir="${remote_exp_dir:-${remote_work_dir}}"
remote_experiment_output_dir="${remote_experiment_output_dir:-${remote_work_dir}/experiment-output}"

# >>> PUBLISHED ROOT (encurtado)
published_root="${published_root:-/users/${remote_user}/iss/experiment-output}"

rsh() { ssh $ssh_options "${remote_user}@${1}" "${2}"; }

master_ip="$(awk 'NF>=4 && $4=="master"{print $2; exit}' "$instance_info_file" || true)"
if [[ -z "${master_ip}" ]]; then
  log_e "Não foi possível obter o IP do master a partir de: $instance_info_file"
  exit 1
fi

cp -f "$instance_info_file" "$exp_data_dir/$instance_info_file_name"

log_i "Master IP: $master_ip"
log_i "Remote work dir: ${remote_work_dir}"
log_i "Remote experiment-output dir: ${remote_experiment_output_dir}"
log_i "Published root: ${published_root}"
log_i "Discovery port: ${DISC_PORT}"

# TLS SAN (mantém teu bloco atual)
log_i "Gerando certificado TLS (auth.pem) com SAN dos IPs públicos+privados do cluster..."
tls_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/tls-data"
[[ -d "${tls_dir}" ]] || { log_e "Diretório tls-data não encontrado: ${tls_dir}"; exit 1; }

mapfile -t all_ips < <(
  awk '{print $2"\n"$3}' "$instance_info_file" \
  | grep -Eo '([0-9]{1,3}\.){3}[0-9]{1,3}' \
  | sort -u
)

[[ "${#all_ips[@]}" -gt 0 ]] || { log_e "Não extraí IPs do instance-info."; exit 1; }

(
  cd "${tls_dir}"
  [[ -x "./generate-auth.sh" ]] || { log_e "generate-auth.sh não executável em ${tls_dir}"; exit 1; }
  ./generate-auth.sh "${all_ips[@]}"
  san="$(openssl x509 -in auth.pem -noout -ext subjectAltName || true)"
  for ip in "${all_ips[@]}"; do
    echo "$san" | grep -q "$ip" || { log_e "SAN não contém IP esperado: $ip"; echo "$san"; exit 1; }
  done
)
log_i "TLS OK."

# Gerar template se faltar
template_path="$exp_data_dir/$local_master_command_template_file"
deployment_file="$exp_data_dir/deployment.dpl"

if [[ ! -f "$template_path" ]]; then
  log_i "Gerando master-commands-template.cmd..."
  [[ -f "$deployment_file" ]] || { log_e "Deployment file não encontrado: $deployment_file"; exit 1; }
  python3 scripts/generate-master-commands.py remote "$deployment_file" "$template_path" "$exp_data_dir" \
    || { log_e "Falha ao gerar master-commands-template.cmd"; exit 1; }
else
  log_i "Usando template existente: $template_path"
fi

export ssh_key_file="${remote_private_key_file:-}"
export own_public_ip="$master_ip"
export master_port="${DISC_PORT}"
export status_file="$remote_status_file"

log_i "Gerando master-commands.cmd..."
envsubst '$ssh_key_file $own_public_ip $master_port $status_file' \
  < "$template_path" > "$exp_data_dir/$local_master_command_file"
echo -e "\nwrite-file $status_file DONE" >> "$exp_data_dir/$local_master_command_file"
log_i "master-commands.cmd pronto."

# Reset remoto (IMPORTANTE: mantém raw-results e também cria published_root no MASTER)
log_i "Reset remoto..."
for ip in $(awk '{print $2}' "$instance_info_file"); do
  ssh $ssh_options "${remote_user}@${ip}" "bash -s" >/dev/null 2>&1 <<EOF_RESET || true
tc qdisc del dev eth0 root tbf rate 1gbit burst 320kbit latency 400ms 2>/dev/null || true
killall -9 discoverymaster discoveryslave orderingpeer orderingclient scp rsync 2>/dev/null || true
rm -rf '${remote_work_dir}'
mkdir -p '${remote_work_dir}' \
         '${remote_work_dir}/logs' \
         '${remote_work_dir}/scripts' \
         '${remote_work_dir}/tls-data' \
         '${remote_work_dir}/experiment-output' \
         '${remote_work_dir}/raw-results'
echo RUNNING > '${remote_status_file}'
EOF_RESET
  sleep 0.1
done
wait
log_i "Reset remoto concluído."

# Start master
log_i "Iniciando master..."
scripts/start-master.sh \
  "$remote_user" \
  "$master_ip" \
  "$remote_work_dir" \
  "$remote_bin_dir" \
  "$exp_data_dir" \
  "$exp_data_dir/$local_master_command_file"

log_i "Validando discoverymaster..."
rsh "$master_ip" "ss -lntp | grep \":${DISC_PORT} \"" >/dev/null \
  || { log_e "discoverymaster não escutando ${DISC_PORT}."; rsh "$master_ip" "tail -n 200 '${remote_work_dir}/logs/discoverymaster.log' || true" || true; exit 1; }

# Start slaves
log_i "Iniciando slaves peers..."
scripts/start-remote-slaves.sh "$exp_data_dir" 0 peers "$instance_info_file"
log_i "Iniciando slaves 1client..."
scripts/start-remote-slaves.sh "$exp_data_dir" 0 1client "$instance_info_file"

# Fetch resultados -> PUBLISHED ROOT
log_i "Coletando resultados para published_root=${published_root} ..."
export REMOTE_WORK_DIR="${remote_work_dir}"
export INSTANCE_INFO_FILE="${instance_info_file}"

set +e
scripts/fetch-results.sh "$master_ip" "${published_root}" > "$exp_data_dir/$local_result_fetching_log" 2>&1
fetch_rc=$?
set -e

if [[ $fetch_rc -ne 0 ]]; then
  log_e "fetch-results.sh falhou (rc=${fetch_rc}). Log: $exp_data_dir/$local_result_fetching_log"
  exit $fetch_rc
fi
log_i "fetch-results.sh OK. Log: $exp_data_dir/$local_result_fetching_log"

# Rodar análise automática para cada RUN existente em published_root
log_i "Rodando análise (gera .val) em ${published_root} ..."
if compgen -G "${published_root}/[0-9][0-9][0-9][0-9]" > /dev/null; then
  for run_dir in "${published_root}"/[0-9][0-9][0-9][0-9]; do
    [[ -d "${run_dir}" ]] || continue
    log_i "Analyze: ${run_dir}"
    # não falha o deploy inteiro se um run estiver incompleto; mas loga
    ./scripts/analyze/analyze.sh "${run_dir}" >> "${exp_data_dir}/analyze.log" 2>&1 || log_w "Analyze falhou em ${run_dir} (ver ${exp_data_dir}/analyze.log)"
  done
else
  log_w "Nenhum RUN encontrado em ${published_root} para analisar."
fi

# Cancelar instâncias
if [[ "$cancel_instances" == "true" ]]; then
  log_i "Encerrando instâncias..."
  scripts/cancel-cloud-instances.sh "$exp_data_dir/$instance_info_file_name"
else
  echo -e "Lembre-se de encerrar as VMs com:\n  cancel-cloud-instances.sh $exp_data_dir/$instance_info_file_name\n"
fi

log_i "deploy-remote.sh finalizado."
exit 0

