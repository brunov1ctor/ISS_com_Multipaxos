#!/bin/bash

source scripts/global-vars.sh

ssh $ssh_options -q -o "ConnectTimeout=10" "$1" '
  status_file="'"$remote_status_file"'"

  if [ -f "$status_file" ]; then
    s=$(cat "$status_file")

    # Se for número puro, devolve direto
    if echo "$s" | grep -Eq "^[0-9]+$"; then
      echo "$s"
    else
      # RUNNING, DONE, READY, vazio → devolve 0
      echo 0
    fi
  else
    echo 0
  fi
' 2>/dev/null || echo 0

