#!/usr/bin/env bash
# wait-quiescent.sh <marker_path> <timeout_ms>
#
# Polls for the existence of <marker_path> every 200ms, up to <timeout_ms>. Used by
# generate-master-commands.py's stopPeers() to wait for a peer to confirm it has no
# catch-up fetch in flight (see maintainQuiescenceMarker in cmd/orderingpeer/main.go)
# before sending it SIGINT -- avoids a real, reproducible permanent hang caused by
# interrupting a peer mid-catch-up (see Limitações no artigo).
#
# Always exits 0: a timeout here means "proceed anyway", not a failure -- stopPeers
# still sends SIGINT and escalates to SIGKILL regardless, so this is a best-effort
# wait, not a new way for the sweep to hang forever.
marker="$1"
timeout_ms="${2:-15000}"
elapsed_ms=0
while [ ! -f "$marker" ] && [ "$elapsed_ms" -lt "$timeout_ms" ]; do
  sleep 0.2
  elapsed_ms=$((elapsed_ms + 200))
done
exit 0
