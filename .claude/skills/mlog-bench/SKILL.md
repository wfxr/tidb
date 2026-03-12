---
name: mlog-bench
description: "Run mlog write overhead benchmarks on the GCP bench-mlog cluster and generate reports. Use when asked to run benchmarks, re-run tests, generate benchmark reports, start/stop the bench cluster, or sync bench scripts. Covers the full workflow: GCP VM lifecycle, TiUP cluster management, script sync, sysbench execution, result download, analysis, and report generation."
---

# Mlog Write Overhead Benchmark

Run INSERT overhead benchmarks comparing baseline vs mlog on the `bench-mlog` GCP cluster.

Infrastructure details: see [references/infra.md](references/infra.md).

Bench scripts live in `bench/mlog-perf/` relative to the repo root.

## Full Benchmark Workflow

### 1. Start Infrastructure

```bash
# Start GCP VMs
gcloud compute instances start \
  bench-mlog-pd-0 bench-mlog-tikv-{0,1,2} bench-mlog-tidb-{0,1,2} \
  bench-mlog-tiflash-0 bench-mlog-load \
  --zone us-east1-b --project gcp-tikv-transaction-dev

# Wait ~60s for VMs to boot, then start TiUP cluster from load machine
./bench/mlog-perf/gcloud/ssh.sh bench-mlog-load "~/.tiup/bin/tiup cluster start bench-mlog"

# Verify cluster health
./bench/mlog-perf/gcloud/ssh.sh bench-mlog-load "mysql -h 10.142.0.7 -P 4000 -u root -sN -e 'SELECT version();'"
```

### 2. Sync Scripts

```bash
./bench/mlog-perf/gcloud/scp.sh bench-mlog-load ./bench '~/'
```

### 3. Run Benchmark

```bash
./bench/mlog-perf/gcloud/ssh.sh bench-mlog-load \
  "cd ~/bench/mlog-perf && nohup bash run_bench.sh \
    --host 10.142.0.7 \
    --priority ALL \
    --target-row-rate 4000 \
    --threads 128 \
    > /tmp/bench.log 2>&1 & echo PID=\$!"
```

**CRITICAL**: Always pass `--host <TiDB-IP>`. Default `127.0.0.1` won't work from the load machine.

Key parameters:
- `--priority P0|P1|P2|ALL` — which test cases to run
- `--target-row-rate N` — required for P2 rate-limited cases
- `--threads N` — sysbench concurrency (default 128)
- `--time N` — seconds per case (default 660 = 60s warmup + 600s measurement)

9 cases x 660s each ≈ 100 minutes total.

### 4. Monitor Progress

```bash
# Summary view (cases completed + validation status)
./bench/mlog-perf/gcloud/ssh.sh bench-mlog-load \
  "grep -E '(^Case|^=|VALIDATE|METRICS|SKIP|DONE)' /tmp/bench.log"

# Live TPS
./bench/mlog-perf/gcloud/ssh.sh bench-mlog-load "tail -5 /tmp/bench.log"
```

### 5. Download Results

The results directory name is printed in the DONE line (e.g., `results/20260312T140018Z`).

```bash
# Find the results directory name
RESULTS_DIR=$(./bench/mlog-perf/gcloud/ssh.sh bench-mlog-load \
  "grep 'Results saved' /tmp/bench.log" | grep -oP 'results/\S+')

# Get just the directory name
DIR_NAME=$(basename "$RESULTS_DIR")

# Download via direct scp (resolve IP first)
IP=$(gcloud compute instances describe bench-mlog-load \
  --zone us-east1-b --project gcp-tikv-transaction-dev \
  --format json | jq -r '.networkInterfaces[0].accessConfigs[0].natIP')

mkdir -p bench/mlog-perf/results/$DIR_NAME
scp -r -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
  -i ~/.ssh/gcp-tikv-transaction-dev.pem \
  "transaction@${IP}:~/bench/mlog-perf/${RESULTS_DIR}/*" \
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
./bench/mlog-perf/gcloud/ssh.sh bench-mlog-load \
  "~/.tiup/bin/tiup cluster stop bench-mlog -y"

# Then stop GCP VMs
gcloud compute instances stop \
  bench-mlog-pd-0 bench-mlog-tikv-{0,1,2} bench-mlog-tidb-{0,1,2} \
  bench-mlog-tiflash-0 bench-mlog-load \
  --zone us-east1-b --project gcp-tikv-transaction-dev
```

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
