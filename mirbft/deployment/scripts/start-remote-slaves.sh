#!/bin/bash

source scripts/global-vars.sh

# Kill all children of this script when exiting
trap "$trap_exit_command" EXIT

exp_data_dir=$1
tag=$2          # grupo-alvo (ex.: "peers", "1client")
n=$3            # quantos nós desse grupo iniciar
master_ip=$4
shift 4

###############################################################################
# 1) Garantir que os binários existem LOCALMENTE
#    - Usa remote_gopath como GOPATH/GOBIN
#    - Se algum binário estiver faltando, roda "go install ./cmd/..."
###############################################################################

binaries="discoverymaster discoveryslave orderingpeer orderingclient"
local_bin_dir="$remote_gopath/bin"

echo "==== [start-remote-slaves] Verificando binários locais em $local_bin_dir ===="

missing_local=0
for b in $binaries; do
  if [ ! -x "$local_bin_dir/$b" ]; then
    echo "  - Binário local AUSENTE: $local_bin_dir/$b"
    missing_local=1
  else
    echo "  - Binário local OK:      $local_bin_dir/$b"
  fi
done

if [ $missing_local -ne 0 ]; then
  echo "  -> Alguns binários estão faltando. Tentando compilar com 'go install ./cmd/...'"

  # Detecta diretório do script e raiz do repositório (mirbft)
  script_dir="$(cd "$(dirname "$0")" && pwd)"
  # deployment/scripts -> mirbft
  repo_root="$(cd "$script_dir/../.." && pwd)"

  echo "  -> Diretório raiz detectado: $repo_root"

  (
    cd "$repo_root"
    export GOPATH="$remote_gopath"
    export GOBIN="$remote_gopath/bin"
    export PATH="$GOBIN:/usr/local/go/bin:$PATH"

    echo "  -> Executando: go install ./cmd/..."
    go install ./cmd/... 
  ) || {
    echo "ERRO: falha ao compilar os binários com 'go install ./cmd/...'. Verifique Go/código fonte." >&2
    exit 1
  }

  echo "  -> Rechecando binários após compilação..."
  for b in $binaries; do
    if [ ! -x "$local_bin_dir/$b" ]; then
      echo "    - AINDA AUSENTE: $local_bin_dir/$b (algo deu errado)" >&2
      exit 1
    else
      echo "    - OK: $local_bin_dir/$b"
    fi
  done
fi

echo "==== [start-remote-slaves] Binários locais OK. ===="
echo ""

###############################################################################
# 2) Garantir que os binários existem nos NÓS REMOTOS daquela TAG
#    - Usa scripts/instance-info para descobrir os IPs públicos
#    - Se binário estiver faltando no nó, copia do local via scp
###############################################################################

instance_info_file="scripts/instance-info"

if [ ! -f "$instance_info_file" ]; then
  echo "WARNING: $instance_info_file não encontrado. Não será possível checar/copiar binários remotos." >&2
else
  echo "==== [start-remote-slaves] Verificando binários remotos para tag '$tag' usando $instance_info_file ===="

  # Percorre o instance-info filtrando pela tag alvo
  # Formato: node-X  <public_ip>  <private_ip>  <role>  <tag>
  while read -r instance_id public_ip private_ip role slave_tag; do
    # Ignora linhas em branco/comentários
    [ -z "$instance_id" ] && continue
    case "$instance_id" in
      \#*) continue ;;
    esac

    # Só interessa quem tem a tag que estamos subindo nessa chamada (peers ou 1client)
    if [ "$slave_tag" != "$tag" ]; then
      continue
    fi

    echo "  -> Nó $instance_id ($public_ip) [tag=$slave_tag]: checando binários..."

    # Garante diretório remoto
    ssh $ssh_options "$public_ip" "mkdir -p \"$remote_gopath/bin\"" >/dev/null 2>&1 || {
      echo "     [ERRO] Não foi possível criar $remote_gopath/bin em $public_ip" >&2
      continue
    }

    # Para cada binário, verifica se existe; se não, copia
    for b in $binaries; do
      ssh $ssh_options "$public_ip" "[ -x \"$remote_gopath/bin/$b\" ]" >/dev/null 2>&1
      if [ $? -eq 0 ]; then
        echo "     - $b já existe em $public_ip:$remote_gopath/bin/$b"
      else
        echo "     - $b AUSENTE em $public_ip. Copiando de $local_bin_dir/$b..."
        scp "$local_bin_dir/$b" "$public_ip:$remote_gopath/bin/" >/dev/null 2>&1 || {
          echo "       [ERRO] Falha ao copiar $b para $public_ip:$remote_gopath/bin/" >&2
        }
      fi
    done

  done < "$instance_info_file"

  echo "==== [start-remote-slaves] Verificação/cópia de binários remotos concluída para tag '$tag'. ===="
  echo ""
fi

###############################################################################
# 3) Lógica original: pular nós (skip) e iniciar discoveryslave nos nós remotos
###############################################################################

# Count how many slaves need to be skipped in the input
skip=0
while [ -n "$1" ] && [ "$1" = "skip" ] && [ $n -gt 0 ]; do
  # Formato:  skip <QTD> <TAG>
  # Exemplo:  skip 4 peers
  if [ "$3" = "$tag" ]; then
    s=$2
    skip=$((skip + s))
  fi
  shift 3
done

# Para cada linha de scripts/instance-info passada como argumentos:
# instance_id  public_ip  private_ip  role  slave_tag
while [ -n "$1" ] && [ $n -gt 0 ]; do
  instance_id=$1
  public_slave_ip=$2
  private_slave_ip=$3
  slave_role=$4      # master / slave
  slave_tag=$5       # peers / 1client / master
  shift 5

  if [ "$slave_tag" = "$tag" ] && [ $skip -gt 0 ]; then
    # Pular esse nó porque já foi contado em algum "skip <qtd> <tag>"
    skip=$((skip - 1))

  elif [ "$slave_tag" = "$tag" ]; then
    echo "Deploying slave at public IP $public_slave_ip ($instance_id) tagged $slave_tag"

    ssh $ssh_options "$public_slave_ip" "
      set -e

      export GOPATH=\"$remote_gopath\"
      export GOROOT=\"/usr/local/go\"
      export PATH=\"\$GOPATH/bin:\$GOROOT/bin:\$PATH\"

      LOG_DIR=\"\$HOME/iss-logs\"
      mkdir -p \"\$LOG_DIR\"
      LOG_FILE=\"\$LOG_DIR/start-slave-$slave_tag.log\"

      {
        echo \"[\\\$(date '+%Y-%m-%d %H:%M:%S')] Starting discoveryslave on \$HOSTNAME\"
        echo \"TAG=$slave_tag MASTER_IP=$master_ip PUBLIC_IP=$public_slave_ip PRIVATE_IP=$private_slave_ip\"
        echo \"PATH=\$PATH\"

        discoveryslave \"$slave_tag\" ${master_ip}:9999 \"$public_slave_ip\" \"$private_slave_ip\" &
        DISCOVERY_PID=\$!
        echo \"discoveryslave PID=\$DISCOVERY_PID\"

        sleep 5
        if kill -0 \"\$DISCOVERY_PID\" 2>/dev/null; then
          echo \"[\\\$(date '+%Y-%m-%d %H:%M:%S')] discoveryslave still running\"
        else
          echo \"[\\\$(date '+%Y-%m-%d %H:%M:%S')] discoveryslave exited\"
        fi
      } >>\"\$LOG_FILE\" 2>&1 &
    " > "$exp_data_dir/ssh-$slave_tag-$public_slave_ip.log" 2>&1 &

    # Evitar abrir conexões SSH demais ao mesmo tempo
    sleep 0.1

    # Decrementa contador de quantos desse grupo ainda faltam
    n=$((n - 1))
  fi
done

# Esperar todos os SSHs/background terminarem
wait

