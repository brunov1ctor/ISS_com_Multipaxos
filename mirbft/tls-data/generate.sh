#!/usr/bin/env bash

# Generate TLS material used by MirBFT experiments.
#
# This script creates:
#   - CA key/cert         : ca.key, ca.pem
#   - Cluster auth key/cert: auth.key, auth.pem (SAN contains provided IPs)
#   - Client signing keys : client-ecdsa-256.key, client-ecdsa-256.pem
#
# Why this exists:
#   orderingpeer/orderingclient expect the client keypair to exist under
#   tls-data/ (paths are configured via YAML). If it's missing, peers fail to
#   register with discovery.

set -euo pipefail

usage() {
  cat >&2 <<'EOF'
Usage:
  generate.sh [-f] <ip1> [ip2 ...]

Options:
  -f   Force regeneration (overwrite existing keys/certs).

Example:
  ./generate.sh -f 10.10.1.1 10.10.1.2 172.21.17.1
EOF
}

force=0
if [[ "${1:-}" == "-f" ]]; then
  force=1
  shift
fi

if [[ $# -lt 1 ]]; then
  usage
  exit 2
fi

ips=("$@")

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "${script_dir}"

cacert="ca.pem"
cakey="ca.key"
authcert="auth.pem"
authkey="auth.key"

client_priv="client-ecdsa-256.key"
client_pub="client-ecdsa-256.pem"

need_ca=0
need_auth=0
need_client=0

if [[ $force -ne 0 || ! -s "${cakey}" || ! -s "${cacert}" ]]; then
  need_ca=1
fi
if [[ $force -ne 0 || ! -s "${authkey}" || ! -s "${authcert}" ]]; then
  need_auth=1
fi
if [[ $force -ne 0 || ! -s "${client_priv}" || ! -s "${client_pub}" ]]; then
  need_client=1
fi

if [[ $need_ca -eq 1 || $need_auth -eq 1 ]]; then
  echo "Generating key and certificate: ${authkey}, ${authcert}"
  echo "(CA: ${cakey}, ${cacert})"

  # generate-node.sh expects: [-f] <authkey> <authcert> <cakey> <cacert> <ip...>
  args=()
  if [[ $force -ne 0 ]]; then
    args+=("-f")
  fi
  args+=("${authkey}" "${authcert}" "${cakey}" "${cacert}")
  args+=("${ips[@]}")

  ./generate-node.sh "${args[@]}"
else
  echo "Auth/CA already present; skipping (use -f to regenerate)."
fi

# Always ensure client keypair exists (orderingpeer/orderingclient require it).
if [[ $need_client -eq 1 ]]; then
  echo "Generating client ECDSA keypair: ${client_priv}, ${client_pub}"
  args=()
  if [[ $force -ne 0 ]]; then
    args+=("-f")
  fi
  ./generate-client.sh "${args[@]}"
else
  echo "Client keypair already present; skipping (use -f to regenerate)."
fi

echo "TLS generation completed."

