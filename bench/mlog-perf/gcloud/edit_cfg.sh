#!/usr/bin/env bash
# Used as EDITOR for `tiup cluster edit-config` to append server_configs non-interactively.
# Usage: EDITOR=/path/to/edit_cfg.sh tiup cluster edit-config <cluster>

cat >> "$1" <<'EOF'

server_configs:
    tidb:
        pessimistic-txn.pessimistic-auto-commit: true
EOF
