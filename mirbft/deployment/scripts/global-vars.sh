#!/usr/bin/env bash

#
# Variáveis globais para os scripts de deployment/execução remota.
# Este arquivo é "sourceado" por praticamente todos os scripts do diretório.
#

set -o nounset
set -o pipefail

# Diretório base deste script.
this_dir="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
deployment_dir="$( cd "${this_dir}/.." && pwd )"
repo_dir="$( cd "${deployment_dir}/.." && pwd )"

# ---------------------------------------------------------------------------
# Configuração de logging
# ---------------------------------------------------------------------------

log_ts() {
  date +"%Y-%m-%d %H:%M:%S"
}

log_info() {
  echo "[INFO  ][$(log_ts)] $*"
}

log_warn() {
  echo "[WARN  ][$(log_ts)] $*" >&2
}

log_error() {
  echo "[ERROR ][$(log_ts)] $*" >&2
}

# ---------------------------------------------------------------------------
# Configuração SSH padrão
# ---------------------------------------------------------------------------

# Opções SSH “robustas” usadas em todos os scripts remotos.
# - sem interação
# - ignora verificação de host key (importante em ambientes de teste como Emulab)
# - não grava known_hosts (evita conflito de chaves entre re-execuções)
ssh_options="-o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null"

# Opções de SCP (usa as mesmas opções de SSH).
scp_opts="-o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null"

# ---------------------------------------------------------------------------
# Parâmetros globais do experimento
# (a maioria vem de variáveis de ambiente ou de defaults razoáveis)
# ---------------------------------------------------------------------------

# Porta do master (ordering/discovery)
master_port="${MASTER_PORT:-9999}"

# Tempo padrão de espera (ms) em várias operações
default_wait_ms="${DEFAULT_WAIT_MS:-2000}"

# ---------------------------------------------------------------------------
# Caminhos *remotos*
#
# IMPORTANTE:
#  - NÃO deixe usuário fixo (tipo /users/bruno).
#  - Usamos, por padrão:
#       remote_user  = DEPL_REMOTE_USER ou $USER
#       remote_home  = DEPL_REMOTE_HOME ou $HOME ou /users/$remote_user
# ---------------------------------------------------------------------------

# Usuário remoto (pode ser sobrescrito na linha de comando ou via env)
remote_user="${DEPL_REMOTE_USER:-$USER}"

# Diretório home remoto:
if [[ -n "${DEPL_REMOTE_HOME:-}" ]]; then
  remote_home="${DEPL_REMOTE_HOME}"
elif [[ -n "${HOME:-}" ]]; then
  remote_home="${HOME}"
else
  # Fallback razoável para ambientes tipo Emulab.
  remote_home="/users/${remote_user}"
fi

remote_work_dir="${remote_home}/iss"
remote_status_file="${remote_work_dir}/status"
remote_gopath="${remote_home}/go"
remote_bin_dir="${remote_gopath}/bin"
remote_exp_dir="${remote_work_dir}/current-deployment-data"
remote_main_log="${remote_work_dir}/main.log"
remote_ready_file="${remote_work_dir}/master-ready"
remote_tls_directory="${remote_work_dir}/tls-data"

# Arquivo usado por alguns scripts para registrar o mapeamento de instâncias
remote_instance_info_file="${remote_exp_dir}/instance-info"
remote_instance_detail_file="${remote_exp_dir}/instance-detail"

# ---------------------------------------------------------------------------
# Funções auxiliares
# ---------------------------------------------------------------------------

# Espera em milissegundos (wrapper de sleep)
wait_ms() {
  local ms="$1"
  python3 - <<EOF
import time
time.sleep(${ms} / 1000.0)
EOF
}

# Pequeno helper para converter "ms" em "s" inteiro quando necessário.
ms_to_s() {
  local ms="$1"
  python3 - <<EOF
import math
ms = ${ms}
print(int(math.ceil(ms / 1000.0)))
EOF
}

