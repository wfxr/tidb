#!/bin/bash
# Usage: ./ssh.sh <instance-name> [extra ssh args...]
# Example:
#   ./ssh.sh bench-mlog-load
#   ./ssh.sh bench-mlog-tikv-0 "df -h"

set -euo pipefail

INSTANCE=${1:?Usage: ./ssh.sh <instance-name> [extra ssh args...]}
shift

PROJECT="gcp-tikv-transaction-dev"
ZONE="us-east1-b"
KEY="~/.ssh/gcp-tikv-transaction-dev.pem"
USER="transaction"

IP=$(gcloud compute instances describe "$INSTANCE" \
  --zone "$ZONE" --project "$PROJECT" \
  --format json | jq -r '.networkInterfaces[0].accessConfigs[0].natIP')

exec ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
  -i "$KEY" "${USER}@${IP}" "$@"
