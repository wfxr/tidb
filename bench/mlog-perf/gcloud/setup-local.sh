#!/bin/bash
# One-time local setup for bench-mlog cluster access.
# Installs SSH key and configures ~/.ssh/config.
#
# Usage: ./gcloud/setup-local.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# Pre-flight: nc is required by ssh_config ProxyCommand
if ! command -v nc &>/dev/null; then
  echo "ERROR: 'nc' (netcat) is not installed. SSH ProxyCommand requires it." >&2
  echo "  Arch: sudo pacman -S openbsd-netcat" >&2
  echo "  Debian/Ubuntu: sudo apt install netcat-openbsd" >&2
  echo "  RHEL/Fedora: sudo dnf install nmap-ncat" >&2
  exit 1
fi

KEY="$HOME/.ssh/gcp-tikv-transaction-dev.pem"
SSH_CONFIG="$HOME/.ssh/config"
INCLUDE_LINE="Include ${SCRIPT_DIR}/ssh_config"

# 1. Install SSH key from Secret Manager
echo "Fetching SSH key from Secret Manager..."
mkdir -p ~/.ssh
gcloud secrets versions access latest \
  --secret=transaction-team-auth-key > "$KEY"
chmod 600 "$KEY"
echo "  Saved to $KEY"

# 2. Add Include to ~/.ssh/config (idempotent)
if [[ -f "$SSH_CONFIG" ]] && grep -qF "$INCLUDE_LINE" "$SSH_CONFIG"; then
  echo "  ~/.ssh/config already has Include line"
else
  echo "Adding Include to ~/.ssh/config..."
  # Include must be at the top to take precedence
  if [[ -f "$SSH_CONFIG" ]]; then
    tmp=$(mktemp)
    { echo "$INCLUDE_LINE"; echo ""; cat "$SSH_CONFIG"; } > "$tmp"
    mv "$tmp" "$SSH_CONFIG"
  else
    echo "$INCLUDE_LINE" > "$SSH_CONFIG"
  fi
  chmod 600 "$SSH_CONFIG"
  echo "  Done"
fi

echo ""
echo "Setup complete. Test with:"
echo "  ssh bench-mlog-load hostname"
