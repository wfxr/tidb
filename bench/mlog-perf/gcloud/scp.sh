#!/bin/bash
# Usage: ./scp.sh <instance-name> <local-path> <remote-path>
# Example:
#   ./scp.sh bench-mlog-load ./bench ~/bench
#   ./scp.sh bench-mlog-load ~/bench/results ./results   # download from remote

set -euo pipefail

INSTANCE=${1:?Usage: ./scp.sh <instance-name> <local-path> <remote-path>}
LOCAL_PATH=${2:?Missing local path}
REMOTE_PATH=${3:?Missing remote path}

PROJECT="gcp-tikv-transaction-dev"
ZONE="us-east1-b"
KEY="~/.ssh/gcp-tikv-transaction-dev.pem"
USER="transaction"

IP=$(gcloud compute instances describe "$INSTANCE" \
  --zone "$ZONE" --project "$PROJECT" \
  --format json | jq -r '.networkInterfaces[0].accessConfigs[0].natIP')

exec scp -r -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
  -i "$KEY" "$LOCAL_PATH" "${USER}@${IP}:${REMOTE_PATH}"
