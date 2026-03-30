#!/bin/bash
# Resolve internal IPs of all bench-mlog instances from GCP.
#
# Outputs shell variable assignments (one per line):
#   PD_0=10.x.x.x
#   TIKV_0=10.x.x.x
#   TIDB_0=10.x.x.x
#   LOAD=10.x.x.x
#   ...
#
# Usage:
#   eval "$(./gcloud/resolve-hosts.sh)"
#   echo "PD: $PD_0, TiDB: $TIDB_0,$TIDB_1,$TIDB_2"

set -euo pipefail

PROJECT="${PROJECT:-gcp-tikv-transaction-dev}"
ZONE="${ZONE:-us-east1-b}"
PREFIX="${PREFIX:-bench-mlog}"

gcloud compute instances list \
    --project "$PROJECT" \
    --zones "$ZONE" \
    --filter "name~'^${PREFIX}-'" \
    --format 'csv[no-heading](name,networkInterfaces[0].networkIP)' \
| sort \
| while IFS=, read -r name ip; do
    [[ -z "$name" || -z "$ip" ]] && continue
    # bench-mlog-tikv-0 → TIKV_0
    suffix="${name#${PREFIX}-}"
    var_name=$(echo "$suffix" | tr '[:lower:]-' '[:upper:]_')
    echo "${var_name}=${ip}"
done
