#!/bin/bash
#
# scripts/deploy-remote.sh
#
# Faz o deploy remoto de um experimento:
#   - prepara diretório de experimento (deployment-data/remote-XXXX)
#   - gera comandos do master
#   - reseta estado das máquinas remotas
#   - garante que os binários discovery/ordering existem LOCALMENTE
#   - dispara master (discoverymaster + orderingclient)
#   - dispara slaves (peers e clients)
#   - espera término, faz fetch dos resultados e resume.
#
# Uso (chamado por deploy.sh):
#   scripts/deploy-remote.sh <instance_info_file> <new|reuse> <config_generator_script>
#
# Exemplo:
#   scripts/deploy-remote.sh scripts/instance-info new scripts/experiment-configuration/generate-config.sh
#

set -euo pipefail

# --------------------------------------------------------------------
# 1) Diretórios básicos
# --------------------------------------------------------------------

this_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
deployment_dir="$(cd "${this_dir}/.." && pwd)"

# Variáveis globais (remote_work_dir, remote_exp_dir, ssh_options, etc.)
# shellcheck source=/dev/null
. "${deployment_dir}/scripts/global-vars.sh"

# --------------------------------------------------------------------
# 2) Parsing de argumentos
# --------------------------------------------------------------------

if [[ $# -ne 3 ]]; then
  echo "Uso: $0 <instance_info_file> <new|reuse> <config_generator_script>" >&2
  exit 1
fi

instance_info_file_arg="$1"
mode="$2"          # "new" ou "reuse"
config_generator_script="$3"

# Resolve instance-info (pode ser relativo ao deployment_dir)
if [[ "$instance_info_file_arg" = /* && -f "$instance_info_file_arg" ]]; then
  instance_info_file="$instance_info_file_arg"
elif [[ -f "${deployment_dir}/${instance_info_file_arg}" ]]; then
  instance_info_file="${deployment_dir}/${instance_info_file_arg}"
else
  instance_info_file="$instance_info_file_arg"
fi

if [[ ! -f "$instance_info_file" ]]; then
  echo "ERRO: instance-info não encontrado: $instance_info_file" >&2
  exit 1
fi

# --------------------------------------------------------------------
# 3) Escolhe / cria diretório de experimento (deployment-data/remote-XXXX)
# --------------------------------------------------------------------

exp_base_dir="${deployment_dir}/deployment-data"
mkdir -p "${exp_base_dir}"

if [[ "${mode}" == "new" ]]; then
  # Encontra o próximo índice remote-0000, remote-0001, ...
  idx=0
  while true; do
    candidate="${exp_base_dir}/remote-$(printf '%04d' "$idx")"
    if [[ ! -d "$candidate" ]]; then
      exp_data_dir="$candidate"
      mkdir -p "$exp_data_dir"
      break
    fi
    idx=$((idx + 1))
  done
else
  # reuse => pega o remote-XXXX mais recente
  last_dir="$(ls -1d "${exp_base_dir}"/remote-* 2>/dev/null | sort | tail -n 1 || true)"
  if [[ -z "$last_dir" ]]; then
    echo "ERRO: modo 'reuse' especificado, mas não há nenhum remote-XXXX em ${exp_base_dir}" >&2
    exit 1
  fi
  exp_data_dir="$last_dir"
fi

echo "Using experiment data directory: $(basename "$exp_data_dir")"
echo "Using experiment data directory (full path): $exp_data_dir"

# --------------------------------------------------------------------
# 4) Determina master_ip a partir do instance-info
# --------------------------------------------------------------------

master_ip=""

while read -r instance_id ctrl_ip data_ip role tag; do
  [[ -z "$instance_id" ]] && continue
  [[ "${instance_id}" =~ ^# ]] && continue

  if [[ "$role" == "master" || "$tag" == "master" || "$instance_id" == "master" ]]; then
    master_ip="$ctrl_ip"
    break
  fi
done < "$instance_info_file"

if [[ -z "$master_ip" ]]; then
  echo "ERRO: não foi possível determinar o master_ip a partir de $instance_info_file" >&2
  exit 1
fi

echo "Using instance info file: $instance_info_file"
echo "       Master IP address: $master_ip"
echo

# --------------------------------------------------------------------
# 4.5) Gera configs de experimento (via script passado)
# --------------------------------------------------------------------

echo "initialize-deployment.sh: about to generate configs..."
echo "  exp_data_dir        = $exp_data_dir"
echo "  config_generator    = $config_generator_script"
echo

# O script de geração de configs recebe:
#   <exp_data_dir> <instance_info_file>
#
# Ele deve:
#   - gerar deployment.dpl
#   - gerar deployment.csv
#   - gerar arquivos config-XXXX.yml em $exp_data_dir/config
"${deployment_dir}/${config_generator_script}" "$exp_data_dir" "$instance_info_file"

echo "Generated configs for experiments."
echo

# --------------------------------------------------------------------
# 5) Garante que os binários discovery/ordering existem LOCALMENTE
#     - discoverymaster, discoveryslave, orderingpeer, orderingclient
#     - tenta compilar com 'go install ./...' se faltar algo
# --------------------------------------------------------------------

ensure_local_binaries() {
  echo
  echo "Ensuring local discovery/ordering binaries exist."

  # Repo root é um nível acima de deployment_dir (mirbft/)
  local repo_dir
  repo_dir="$(cd "$deployment_dir/.." && pwd)"

  if ! command -v go >/dev/null 2>&1; then
    echo "  [WARN] 'go' não encontrado no PATH; não é possível compilar binários automaticamente."
    echo "         Instale o Go ou compile os binários manualmente (discoverymaster, discoveryslave, orderingpeer, orderingclient)."
    return
  fi

  # GOPATH/bin padrão
  local gopath_bin
  gopath_bin="$(go env GOPATH 2>/dev/null)/bin"
  if [[ -z "$gopath_bin" || "$gopath_bin" == "/bin" ]]; then
    gopath_bin="$HOME/go/bin"
  fi

  # Permite override via LOCAL_BIN_DIR
  if [[ -z "${LOCAL_BIN_DIR:-}" ]]; then
    LOCAL_BIN_DIR="$gopath_bin"
  fi
  export LOCAL_BIN_DIR

  echo "  - Usando LOCAL_BIN_DIR=$LOCAL_BIN_DIR"

  # Verifica presença dos binários
  local missing=""
  for bin in discoverymaster discoveryslave orderingpeer orderingclient; do
    if [[ ! -x "$LOCAL_BIN_DIR/$bin" ]]; then
      echo "  - Faltando binário: $LOCAL_BIN_DIR/$bin"
      missing=1
    else
      echo "  - Binário OK:       $LOCAL_BIN_DIR/$bin"
    fi
  done

  # Se faltar algo, tenta compilar tudo com 'go install ./...'
  if [[ -n "$missing" ]]; then
    echo "  - Alguns binários estão faltando. Executando 'go install ./...' em $repo_dir"
    if (cd "$repo_dir" && go install ./...); then
      echo "  - 'go install ./...' concluído."
    else
      echo "  [WARN] 'go install ./...' falhou; os binários podem continuar faltando."
    fi

    # Re-checa
    local still_missing=""
    for bin in discoverymaster discoveryslave orderingpeer orderingclient; do
      if [[ ! -x "$LOCAL_BIN_DIR/$bin" ]]; then
        still_missing=1
      fi
    done
    if [[ -n "$still_missing" ]]; then
      echo "  [ERROR] Binários necessários ainda estão faltando em $LOCAL_BIN_DIR."
      echo "          O deploy vai continuar, mas discovery/ordering provavelmente vai falhar."
    else
      echo "  - Todos os binários necessários agora existem em $LOCAL_BIN_DIR."
    fi
  else
    echo "  - Todos os binários já estavam presentes."
  fi
}

# Garante binários antes de subir master/slaves
ensure_local_binaries
echo

# --------------------------------------------------------------------
# 6) Gera master-commands.cmd a partir de deployment.dpl
# --------------------------------------------------------------------

deployment_file="${exp_data_dir}/deployment.dpl"
master_cmd_template="${deployment_dir}/master-commands-template.cmd"
master_cmd_out="${exp_data_dir}/master-commands.cmd"

echo "initialize-deployment.sh: about to generate master commands:"
echo "  depl_type                     = remote"
echo "  deployment_file (.dpl)        = $deployment_file"
echo "  local_master_command_template = $(basename "$master_cmd_template")"
echo "  output template path          = $master_cmd_out"

python3 "${deployment_dir}/scripts/generate-master-commands.py" \
  --deployment "$deployment_file" \
  --template   "$master_cmd_template" \
  --output     "$master_cmd_out"

echo "initialize-deployment.sh: generate-master-commands.py exit code = $?"
echo "Master command script written to $master_cmd_out."
echo

# --------------------------------------------------------------------
# 7) Reset de estado nas máquinas remotas (mata processos + limpa lixo)
# --------------------------------------------------------------------

echo "Limpando processos antigos e removendo possíveis limitações de banda nas máquinas remotas..."

"${deployment_dir}/scripts/reset-remote-procs.sh" "$instance_info_file" "$master_ip"

echo
echo "Estado das máquinas remotas resetado."
echo

# --------------------------------------------------------------------
# 8) Start master (discoverymaster + orderingclient) no nó master
# --------------------------------------------------------------------

echo "Starting master on $master_ip."

"${deployment_dir}/scripts/start-master.sh" \
  "$exp_data_dir" \
  "$master_ip"

echo

# --------------------------------------------------------------------
# 9) Start slaves (peers e 1client) usando start-remote-slaves.sh
# --------------------------------------------------------------------

num_peers=4      # extraído da deployment.dpl, mas aqui mantemos fixo conforme template
num_clients=1

echo "Starting peer slaves (tag=peers)."
"${deployment_dir}/scripts/start-remote-slaves.sh" \
  "$exp_data_dir" \
  "$num_peers" \
  "peers" \
  "$instance_info_file"

echo "Starting client slaves (tag=1client)."
"${deployment_dir}/scripts/start-remote-slaves.sh" \
  "$exp_data_dir" \
  "$num_clients" \
  "1client" \
  "$instance_info_file"

echo "All slaves started. waiting for them to finish."
echo "Remote slave deployment finished."
echo

# --------------------------------------------------------------------
# 10) Espera término + fetch de resultados
# --------------------------------------------------------------------

echo "Waiting for deployment process and result fetching to finish."
echo "For progress on experiment result fetching, see result-fetching.log."
echo "Do not forget to cancel the used virtual servers using"
echo
echo "    scripts/cancel-cloud-instances.sh ${exp_data_dir}/cloud-instance-info"
echo

# Aqui assumimos que o master-remote (no master) cuida de rodar os experimentos,
# gerar tar.gz e que depois vamos rodar fetch-results.sh manualmente.
# Se quiser automatizar o fetch aqui, pode-se chamar:
#   ${deployment_dir}/scripts/fetch-results.sh "$master_ip" "$exp_data_dir"

echo "Done. Experiment data directory: $exp_data_dir"

