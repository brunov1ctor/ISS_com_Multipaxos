#!/bin/bash

# start-remote-slaves.sh
#
# Sobe os slaves (peers / 1client) em modo remoto.
# Versão com logs de debug mais detalhados e correções:
#   - Garante cópia dos binários para TODOS os nós da tag (sem depender só de "já existe").
#   - Usa caminho absoluto para discoveryslave: $remote_gopath/bin/discoveryslave
#   - Loga verificação pós-cópia no nó remoto.

set -euo pipefail

source scripts/global-vars.sh

# Kill all children of this script when exiting
trap "$trap_exit_command" EXIT

exp_data_dir=$1   # ex.: deployment-data/remote-0000
tag=$2            # grupo-alvo (ex.: "peers", "1client")
n=$3              # quantos nós desse grupo iniciar
master_ip=$4      # IP público do master (ex.: 172.20.3.2)
shift 4

echo "====================================================================="
echo "=== [start-remote-slaves] INÍCIO ===================================="
echo "  exp_data_dir = $exp_data_dir"
echo "  tag          = $tag"
echo "  n            = $n"
echo "  master_ip    = $master_ip"
echo "  args rest    = $*"
echo "====================================================================="
echo ""

###############################################################################
# 1) Garantir que os binários existem LOCALMENTE
###############################################################################

binaries="discoverymaster discoveryslave orderingpeer orderingclient"
local_bin_dir="$remote_gopath/bin"

echo "==== [start-remote-slaves] (LOCAL) Verificando binários em $local_bin_dir ===="
echo "  remote_gopath = $remote_gopath"
echo "  local_bin_dir = $local_bin_dir"

missing_local=0
for b in $binaries; do
  if [ ! -x "$local_bin_dir/$b" ]; then
    echo "  [LOCAL] AUSENTE: $local_bin_dir/$b"
    missing_local=1
  else
    echo "  [LOCAL] OK     : $local_bin_dir/$b"
  fi
done

if [ $missing_local -ne 0 ]; then
  echo "  -> Alguns binários locais estão faltando. Tentando compilar com 'go install ./cmd/...'."

  # Diretório do script e raiz do repo
  script_dir="$(cd "$(dirname "$0")" && pwd)"
  repo_root="$(cd "$script_dir/../.." && pwd)"

  echo "  -> Diretório do script : $script_dir"
  echo "  -> Diretório do repo   : $repo_root"

  (
    cd "$repo_root"
    export GOPATH="$remote_gopath"
    export GOBIN="$remote_gopath/bin"
    export PATH="$GOBIN:/usr/local/go/bin:$PATH"

    echo "  -> [LOCAL] Executando: go install ./cmd/..."
    go install ./cmd/...
  ) || {
    echo "ERRO: falha ao compilar os binários com 'go install ./cmd/...'. Verifique Go/código fonte." >&2
    exit 1
  }

  echo "  -> [LOCAL] Rechecando binários após compilação..."
  for b in $binaries; do
    if [ ! -x "$local_bin_dir/$b" ]; then
      echo "    - AINDA AUSENTE: $local_bin_dir/$b (algo deu errado)" >&2
      exit 1
    else
      echo "    - OK           : $local_bin_dir/$b"
    fi
  done
fi

echo "==== [start-remote-slaves] Binários locais OK. ===="
echo ""

###############################################################################
# 2) Garantir que os binários existem nos NÓS REMOTOS daquela TAG
###############################################################################

instance_info_file="scripts/instance-info"

if [ ! -f "$instance_info_file" ]; then
  echo "WARNING: $instance_info_file não encontrado. Não será possível checar/copiar binários remotos." >&2
else
  echo "==== [start-remote-slaves] (REMOTO) Verificando/copindo binários para tag '$tag' usando $instance_info_file ===="

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

    echo "---------------------------------------------------------------------"
    echo "  [REMOTO] Nó $instance_id ($public_ip / $private_ip)"
    echo "           role=$role tag=$slave_tag (tag alvo=$tag)"
    echo "---------------------------------------------------------------------"

    echo "    [REMOTO:$public_ip] mkdir -p \"$remote_gopath/bin\""
    ssh $ssh_options "$public_ip" "mkdir -p \"$remote_gopath/bin\"" >/dev/null 2>&1 || {
      echo "     [ERRO] Não foi possível criar $remote_gopath/bin em $public_ip" >&2
      continue
    }

    # Copia TODOS os binários sempre (para garantir consistência)
    for b in $binaries; do
      echo "    [REMOTO:$public_ip] Copiando binário '$b' para $remote_gopath/bin/..."
      if scp "$local_bin_dir/$b" "$public_ip:$remote_gopath/bin/" >/dev/null 2>&1; then
        # Verifica no nó remoto se o binário ficou executável
        ssh $ssh_options "$public_ip" "
          if [ -x \"$remote_gopath/bin/$b\" ]; then
            echo \"      [REMOTO-$b] OK: $remote_gopath/bin/$b (executável)\"
          else
            echo \"      [REMOTO-$b] ERRO: $remote_gopath/bin/$b NÃO é executável ou não existe.\"
            ls -l \"$remote_gopath/bin\" || true
          fi
        " 2>/dev/null || {
          echo "      [REMOTO-$b] ERRO ao verificar $remote_gopath/bin/$b em $public_ip" >&2
        }
      else
        echo "      [ERRO] Falha ao copiar $b para $public_ip:$remote_gopath/bin/" >&2
      fi
    done

  done < "$instance_info_file"

  echo "==== [start-remote-slaves] (REMOTO) Distribuição de binários concluída para tag '$tag'. ===="
  echo ""
fi

###############################################################################
# 3) Processar argumentos extras (skip / lista de nós) e iniciar discoveryslave
###############################################################################

echo "==== [start-remote-slaves] Processando 'skip' nos argumentos extras ===="
echo "  args atuais: $*"
echo ""

# Count how many slaves need to be skipped in the input
skip=0
while [ -n "${1-}" ] && [ "$1" = "skip" ] && [ $n -gt 0 ]; do
  # Formato:  skip <QTD> <TAG>
  # Exemplo:  skip 4 peers
  echo "  [SKIP] Encontrado padrão: skip $2 $3"
  if [ "$3" = "$tag" ]; then
    s=$2
    skip=$((skip + s))
    echo "  [SKIP] Somando $s ao contador de skip (total agora: $skip) para tag '$tag'"
  else
    echo "  [SKIP] Tag '$3' difere da tag alvo '$tag'; ignorando esse skip."
  fi
  shift 3
done

echo "  [SKIP] Valor final de 'skip' para tag '$tag': $skip"
echo "==== [start-remote-slaves] Iniciando loop pelos nós (n = $n) ===="
echo ""

# Para cada linha de scripts/instance-info passada como argumentos:
# instance_id  public_ip  private_ip  role  slave_tag
while [ -n "${1-}" ] && [ $n -gt 0 ]; do
  instance_id=$1
  public_slave_ip=$2
  private_slave_ip=$3
  slave_role=$4      # master / slave
  slave_tag=$5       # peers / 1client / master
  shift 5

  echo "---------------------------------------------------------------------"
  echo "  [LOOP] instance_id=$instance_id"
  echo "         public_slave_ip=$public_slave_ip"
  echo "         private_slave_ip=$private_slave_ip"
  echo "         slave_role=$slave_role"
  echo "         slave_tag=$slave_tag"
  echo "         tag alvo=$tag, skip atual=$skip, n restante=$n"
  echo "---------------------------------------------------------------------"

  if [ "$slave_tag" = "$tag" ] && [ $skip -gt 0 ]; then
    echo "  [LOOP] Esse nó tem a tag '$tag', mas ainda há 'skip' pendente ($skip). Pulando esse nó."
    skip=$((skip - 1))
    echo "  [LOOP] Novo valor de skip: $skip"

  elif [ "$slave_tag" = "$tag" ]; then
    echo "  [DEPLOY] Vai iniciar slave em $public_slave_ip ($instance_id), tag=$slave_tag"

    ssh_log="$exp_data_dir/ssh-$slave_tag-$public_slave_ip.log"
    echo "  [DEPLOY] Log remoto será gravado em: $ssh_log"

    ssh $ssh_options "$public_slave_ip" "
      set -e

      export GOPATH=\"$remote_gopath\"
      export GOROOT=\"/usr/local/go\"
      export PATH=\"\$GOPATH/bin:\$GOROOT/bin:\$PATH\"

      DISCOVERY_BIN=\"$remote_gopath/bin/discoveryslave\"

      LOG_DIR=\"\$HOME/iss-logs\"
      mkdir -p \"\$LOG_DIR\"
      LOG_FILE=\"\$LOG_DIR/start-slave-$slave_tag.log\"

      {
        echo \"=================================================================\"
        echo \"[\$(date '+%Y-%m-%d %H:%M:%S')] [REMOTE-$slave_tag] Início do script remoto no host \$HOSTNAME\"
        echo \"  TAG        = $slave_tag\"
        echo \"  MASTER_IP  = $master_ip\"
        echo \"  PUBLIC_IP  = $public_slave_ip\"
        echo \"  PRIVATE_IP = $private_slave_ip\"
        echo \"  GOPATH     = \$GOPATH\"
        echo \"  GOROOT     = \$GOROOT\"
        echo \"  PATH       = \$PATH\"
        echo \"  DISCOVERY_BIN = \$DISCOVERY_BIN\"
        echo \"-----------------------------------------------------------------\"

        if [ ! -x \"\$DISCOVERY_BIN\" ]; then
          echo \"[\$(date '+%Y-%m-%d %H:%M:%S')] [REMOTE-$slave_tag] ERRO: \$DISCOVERY_BIN não existe ou não é executável.\"
          echo \"Conteúdo de $remote_gopath/bin:\"
          ls -l \"$remote_gopath/bin\" || true
        else
          echo \"[\$(date '+%Y-%m-%d %H:%M:%S')] [REMOTE-$slave_tag] Executando discoveryslave via caminho absoluto...\"
          echo \"  Comando: \$DISCOVERY_BIN $slave_tag ${master_ip}:9999 $public_slave_ip $private_slave_ip\"

          \"\$DISCOVERY_BIN\" \"$slave_tag\" ${master_ip}:9999 \"$public_slave_ip\" \"$private_slave_ip\" &
          DISCOVERY_PID=\$!
          echo \"[\$(date '+%Y-%m-%d %H:%M:%S')] [REMOTE-$slave_tag] discoveryslave iniciado com PID=\$DISCOVERY_PID\"

          sleep 5
          if kill -0 \"\$DISCOVERY_PID\" 2>/dev/null; then
            echo \"[\$(date '+%Y-%m-%d %H:%M:%S')] [REMOTE-$slave_tag] discoveryslave ainda está rodando (PID=\$DISCOVERY_PID).\"
          else
            echo \"[\$(date '+%Y-%m-%d %H:%M:%S')] [REMOTE-$slave_tag] discoveryslave JÁ MORREU (PID=\$DISCOVERY_PID).\"
          fi
        fi

        echo \"[\$(date '+%Y-%m-%d %H:%M:%S')] [REMOTE-$slave_tag] Fim do bloco remoto.\"
        echo \"=================================================================\"
      } >>\"\$LOG_FILE\" 2>&1 &
    " > "$ssh_log" 2>&1 &

    echo "  [DEPLOY] SSH disparado para $public_slave_ip em background (PID=$!)."
    echo "  [DEPLOY] Aguardando pequena pausa para não sobrecarregar o SSH."

    # Evitar abrir conexões SSH demais ao mesmo tempo
    sleep 0.1

    # Decrementa contador de quantos desse grupo ainda faltam
    n=$((n - 1))
    echo "  [DEPLOY] n restante agora = $n"
  else
    echo "  [LOOP] slave_tag='$slave_tag' não bate com a tag alvo='$tag'; ignorando esse nó."
  fi
done

echo ""
echo "==== [start-remote-slaves] Todos os SSHs disparados. Chamando 'wait' para aguardar término dos comandos locais. ===="
wait
echo "==== [start-remote-slaves] FIM ==========================================="
echo "====================================================================="

