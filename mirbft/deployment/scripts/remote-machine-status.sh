#!/bin/bash

source scripts/global-vars.sh

ssh $ssh_options -q -o "ConnectTimeout=10" "$1" '
  status_file="'"$remote_status_file"'"

  if [ -f "$status_file" ]; then
    s=$(cat "$status_file")

    # Se for número puro (progresso da análise), devolve direto
    if echo "$s" | grep -Eq "^[0-9]+$"; then
      echo "$s"
    else
      # Preserva DONE / ANALYZED para o fetch-results.sh
      case "$s" in
        DONE|ANALYZED)
          echo "$s"
          ;;
        *)
          # RUNNING, READY, vazio, etc. → 0
          echo 0
          ;;
      esac
    fi
  else
    # Sem arquivo de status → 0
    echo 0
  fi
' 2>/dev/null || echo 0

