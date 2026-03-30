---
name: mlog-bench
description: "Run mlog write overhead benchmarks on the GCP bench-mlog cluster and generate reports. Use when asked to run benchmarks, re-run tests, generate benchmark reports, start/stop the bench cluster, or sync bench scripts. Covers the full workflow: GCP VM lifecycle, TiUP cluster management, script sync, sysbench execution, result download, analysis, and report generation."
---

# Mlog Write Overhead Benchmark

Run INSERT overhead benchmarks comparing baseline vs mlog on the `bench-mlog` GCP cluster.

Infrastructure details: see [references/infra.md](references/infra.md).

Bench scripts live in `bench/mlog-perf/` relative to the repo root.

## Prerequisites

First-time setup (one-off, requires `gcloud` CLI and `nc`):

```bash
cd bench/mlog-perf && ./gcloud/setup-local.sh
```

This fetches the SSH key from Secret Manager and configures `~/.ssh/config` so that `ssh bench-mlog-*` works directly.

## Full Benchmark Workflow

### 1. Start Infrastructure

First check whether the GCP VMs and TiUP cluster exist:

```bash
gcloud compute instances list --filter="name~'^bench-mlog-'" \
  --project gcp-tikv-transaction-dev --format="table(name,status)"
```

**If VMs exist** — start them and the TiUP cluster:

```bash
gcloud compute instances start \
  bench-mlog-pd-0 bench-mlog-tikv-{0,1,2} bench-mlog-tidb-{0,1,2} \
  bench-mlog-tiflash-0 bench-mlog-load \
  --zone us-east1-b --project gcp-tikv-transaction-dev

# Wait ~60s for VMs to boot, then start TiUP cluster
ssh bench-mlog-load "~/.tiup/bin/tiup cluster start bench-mlog"
ssh bench-mlog-load "~/.tiup/bin/tiup cluster display bench-mlog | head -20"
```

**If VMs don't exist** — recreate the full cluster (see [Recreate Cluster](#recreate-cluster) below).

Verify cluster health (TiDB hosts are auto-discovered by run_bench.sh):
```bash
ssh bench-mlog-load "~/.tiup/bin/tiup cluster display bench-mlog | head -20"
```

### 2. Sync Scripts

```bash
scp -r ./bench bench-mlog-load:~/
```

### 3. Run Benchmark

```bash
ssh bench-mlog-load \
  "cd ~/bench/mlog-perf && nohup bash run_bench.sh \
    --target-row-rate 6500 \
    --threads 128 \
    > /tmp/bench.log 2>&1 & echo PID=\$!"
```

TiDB hosts are auto-discovered from `tiup cluster display bench-mlog`. To override, pass `--host IP[,IP,...]`.

Key parameters:
- `--dry-run` — verify config, show discovered hosts and case plan without executing
- `--host IP[,IP,...]` — override TiDB node(s); omit to auto-discover from TiUP cluster
- `--cases N[,N,...]` — run only specific case IDs (e.g. `--cases 8,9`); omit to run all
- `--target-row-rate N` — required for P2 rate-limited cases (cases 8,9)
- `--threads N` — sysbench concurrency (default 128)
- `--time N` — seconds per case (default 660 = 60s warmup + 600s measurement)

9 cases x 660s each ≈ 100 minutes total.

### 4. Monitor Progress

```bash
# Summary view (cases completed + validation status)
ssh bench-mlog-load \
  "grep -E '(^Case|^=|VALIDATE|METRICS|SKIP|DONE)' /tmp/bench.log"

# Live TPS
ssh bench-mlog-load "tail -5 /tmp/bench.log"
```

### 5. Download Results

The results directory name is printed in the DONE line (e.g., `results/20260312T140018Z`).

```bash
# Find the results directory name
RESULTS_DIR=$(ssh bench-mlog-load \
  "grep 'Results saved' /tmp/bench.log" | grep -oP 'results/\S+')

DIR_NAME=$(basename "$RESULTS_DIR")

mkdir -p bench/mlog-perf/results/$DIR_NAME
scp -r "bench-mlog-load:~/bench/mlog-perf/${RESULTS_DIR}/*" \
  bench/mlog-perf/results/$DIR_NAME/
```

### 6. Analyze Results

```bash
cd bench/mlog-perf
python3 analyze_results.py results/$DIR_NAME
```

Outputs:
- Throughput summary table (rows/s, overhead %)
- Latency comparison table
- Cluster metrics table (CPU, prewrite RPCs, disk I/O)
- Metrics overhead comparison (baseline vs mlog)
- `summary.csv` and per-case `case_*_timeseries.csv`

### 7. Generate Report

Write `results/report-rN.md` based on the analysis output. Key sections:
- Executive Summary (headline overhead numbers)
- Test Matrix (9 cases)
- Throughput Results (raw data + overhead analysis for Q1-Q5)
- Cluster Metrics (per-case + overhead comparison)
- Key Findings (one subsection per question Q1-Q5, with root cause analysis)
- Summary table (predicted vs actual overhead)
- Validation status
- Environment info

### 8. Stop Infrastructure

```bash
# Stop TiUP cluster first
ssh bench-mlog-load "~/.tiup/bin/tiup cluster stop bench-mlog -y"

# Then stop GCP VMs
gcloud compute instances stop \
  bench-mlog-pd-0 bench-mlog-tikv-{0,1,2} bench-mlog-tidb-{0,1,2} \
  bench-mlog-tiflash-0 bench-mlog-load \
  --zone us-east1-b --project gcp-tikv-transaction-dev
```

## Recreate Cluster

When the GCP VMs have been deleted, follow this procedure to recreate from scratch. The official TiDB release does not support `CREATE MATERIALIZED VIEW LOG`, so patching with feature-branch binaries is mandatory after every `tiup cluster deploy`.

```bash
# 1. Create GCP VMs via Deployment Manager
gcloud deployment-manager deployments create bench-mlog \
  --config gcloud/bench-mlog.yaml --project gcp-tikv-transaction-dev

# 2. Setup local SSH config (fetches SSH key + configures ProxyCommand)
cd bench/mlog-perf && ./gcloud/setup-local.sh

# 3. Wait ~90s for startup scripts, then regenerate topology with new IPs
./gcloud/gen-topology.sh

# 4. Sync scripts to load machine
cd <repo-root> && rsync -avz --exclude='results/' ./bench/mlog-perf/ bench-mlog-load:~/bench/mlog-perf/

# 5. Deploy TiUP cluster (do NOT start — must patch first)
ssh bench-mlog-load "~/.tiup/bin/tiup cluster deploy bench-mlog v8.5.1 \
  ~/bench/mlog-perf/topology.yaml --user transaction -y"

# 6. Patch with feature-branch binaries (cluster is offline after deploy)
ssh bench-mlog-load "gcloud storage cp 'gs://oltp-bench-us-east1/tmp/zwx/*.tar.gz' /tmp/"
ssh bench-mlog-load "~/.tiup/bin/tiup cluster patch bench-mlog /tmp/tidb-linux-amd64.tar.gz -R tidb --overwrite --offline -y"
ssh bench-mlog-load "~/.tiup/bin/tiup cluster patch bench-mlog /tmp/tikv-linux-amd64.tar.gz -R tikv --overwrite --offline -y"
ssh bench-mlog-load "~/.tiup/bin/tiup cluster patch bench-mlog /tmp/tiflash-linux-amd64.tar.gz -R tiflash --overwrite --offline -y"

# 7. Start the patched cluster
ssh bench-mlog-load "~/.tiup/bin/tiup cluster start bench-mlog --init"
# --init generates a random root password, printed in the output

# 8. Reset root password to empty (bench scripts expect passwordless root)
ssh bench-mlog-load "mysql -h <tidb-ip> -P 4000 -u root -p'<password>' \
  -e \"ALTER USER 'root'@'%' IDENTIFIED BY '';\""

# 9. Verify
ssh bench-mlog-load "~/.tiup/bin/tiup cluster display bench-mlog | head -20"
```

Once patched, subsequent `tiup cluster start/stop` do not require re-patching.

## Test Cases Reference

| # | Scenario | Shard | Batch | TxnMode | Rate | Priority |
|---|----------|-------|-------|---------|------|----------|
| 1 | baseline | - | 1 | optimistic | unlim | P0 |
| 2 | mlog | shard | 1 | optimistic | unlim | P0 |
| 3 | mlog | noshard | 1 | optimistic | unlim | P0 |
| 4 | baseline | - | 10 | optimistic | unlim | P0 |
| 5 | mlog | shard | 10 | optimistic | unlim | P0 |
| 6 | baseline | - | 1 | pessimistic | unlim | P1 |
| 7 | mlog | shard | 1 | pessimistic | unlim | P1 |
| 8 | baseline | - | 1 | optimistic | rate-X | P2 |
| 9 | mlog | shard | 1 | optimistic | rate-X | P2 |
