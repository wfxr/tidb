#!/usr/bin/env python3
"""Analyze mlog write overhead benchmark results.

Usage: python3 analyze_results.py <results_dir>

Reads case_*.log, case_*.meta, and case_*.metrics.summary files produced
by run_bench.sh, computes throughput/latency overhead and cluster resource
comparisons, and writes summary tables to stdout plus CSV files to the
results directory.
"""

import csv
import os
import re
import statistics
import sys

WARMUP_SECS = 60


# ---------- metadata ----------

def load_case_meta(results_dir, case_id):
    """Read key=value metadata for a case."""
    meta = {}
    path = os.path.join(results_dir, f"case_{case_id}.meta")
    with open(path) as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            k, v = line.split("=", 1)
            meta[k] = v
    meta["batch_size"] = int(meta["batch_size"])
    meta["rate"] = int(meta["rate"]) if meta["rate"] != "0" else 0
    return meta


def load_metrics_summary(results_dir, case_id):
    """Read metrics summary (key=value) for a case, or None if missing."""
    path = os.path.join(results_dir, f"case_{case_id}.metrics.summary")
    if not os.path.exists(path):
        return None
    metrics = {}
    with open(path) as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            k, v = line.split("=", 1)
            metrics[k] = float(v)
    return metrics


# ---------- sysbench log parsing ----------

def parse_summary(log_text):
    """Extract final summary metrics from sysbench output."""
    result = {}

    m = re.search(r"transactions:\s+\d+\s+\(([\d.]+) per sec\.\)", log_text)
    if m:
        result["tps"] = float(m.group(1))

    m = re.search(r"avg:\s+([\d.]+)", log_text)
    if m:
        result["avg_lat"] = float(m.group(1))
    m = re.search(r"99th percentile:\s+([\d.]+)", log_text)
    if m:
        result["p99_lat"] = float(m.group(1))
    m = re.search(r"max:\s+([\d.]+)", log_text)
    if m:
        result["max_lat"] = float(m.group(1))

    m = re.search(r"total number of events:\s+(\d+)", log_text)
    if m:
        result["total_events"] = int(m.group(1))

    return result


def parse_timeseries(log_text, warmup_secs=WARMUP_SECS):
    """Extract report-interval time series, discarding warmup period."""
    pattern = r"\[\s*(\d+)s\s*\].*tps:\s*([\d.]+).*lat \(ms,99%\):\s*([\d.]+)"
    points = []
    for m in re.finditer(pattern, log_text):
        ts = int(m.group(1))
        tps = float(m.group(2))
        lat99 = float(m.group(3))
        if ts > warmup_secs:
            points.append((ts, tps, lat99))
    return points


# ---------- grouping & overhead ----------

def group_key(meta):
    """Group key for matching baseline to mlog cases."""
    rate_cat = "unlim" if meta["rate"] == 0 else f"rate-{meta['rate']}"
    return (meta["batch_size"], meta["txn_mode"], rate_cat)


def label(meta):
    """Human-readable scenario label."""
    if meta["scenario"] == "baseline":
        return "baseline"
    return f"mlog-{meta['mlog_shard']}"


def fmt_oh(mlog_val, bl_val):
    """Format overhead percentage, return '-' if either value is missing/zero."""
    if bl_val is None or mlog_val is None or bl_val == 0:
        return "-"
    return f"{(mlog_val - bl_val) / bl_val * 100:+.1f}%"


# ---------- main ----------

def main():
    if len(sys.argv) < 2:
        print(f"Usage: {sys.argv[0]} <results_dir>", file=sys.stderr)
        sys.exit(1)

    results_dir = sys.argv[1]
    if not os.path.isdir(results_dir):
        print(f"Error: {results_dir} is not a directory", file=sys.stderr)
        sys.exit(1)

    # Discover cases
    case_ids = sorted(
        int(f.replace("case_", "").replace(".meta", ""))
        for f in os.listdir(results_dir)
        if f.startswith("case_") and f.endswith(".meta")
    )

    if not case_ids:
        print("No case metadata found.", file=sys.stderr)
        sys.exit(1)

    # Load all case data
    cases = {}
    has_metrics = False
    for cid in case_ids:
        meta = load_case_meta(results_dir, cid)
        log_path = os.path.join(results_dir, f"case_{cid}.log")
        if not os.path.exists(log_path):
            print(f"[WARN] Missing log for case #{cid}, skipping")
            continue
        with open(log_path) as f:
            log_text = f.read()
        summary = parse_summary(log_text)
        if "tps" not in summary:
            print(f"[WARN] Could not parse TPS for case #{cid}, skipping")
            continue
        ts = parse_timeseries(log_text)
        rows_s = summary["tps"] * meta["batch_size"]
        cv = 0.0
        if len(ts) > 1:
            tps_values = [p[1] for p in ts]
            cv = statistics.stdev(tps_values) / statistics.mean(tps_values)

        metrics = load_metrics_summary(results_dir, cid)
        if metrics is not None:
            has_metrics = True

        total_rows = summary.get("total_events", 0) * meta["batch_size"]

        cases[cid] = {
            "meta": meta,
            "summary": summary,
            "timeseries": ts,
            "rows_s": rows_s,
            "cv": cv,
            "metrics": metrics,
            "total_rows": total_rows,
        }

    # Group by (batch_size, txn_mode, rate_category)
    groups = {}
    for cid, data in cases.items():
        gk = group_key(data["meta"])
        groups.setdefault(gk, {"baseline": None, "mlog": []})
        if data["meta"]["scenario"] == "baseline":
            groups[gk]["baseline"] = cid
        else:
            groups[gk]["mlog"].append(cid)

    # ---------- throughput summary table ----------
    print("=" * 110)
    print("THROUGHPUT SUMMARY")
    print("=" * 110)
    header = (f"{'Batch':>5} | {'TxnMode':<12} | {'RowRate':<10} | {'Baseline rows/s':>16} | "
              f"{'Mlog-NoShard rows/s':>20} | {'OH%':>7} | {'Mlog-Shard rows/s':>18} | {'OH%':>7} | {'CV':>5}")
    print(header)
    print("-" * 110)

    for gk in sorted(groups.keys()):
        g = groups[gk]
        batch, txn, rate_cat = gk
        bl_rows = "-"
        if g["baseline"] and g["baseline"] in cases:
            bl_rows = f"{cases[g['baseline']]['rows_s']:.0f}"

        noshard_rows = "-"
        noshard_oh = "-"
        shard_rows = "-"
        shard_oh = "-"
        cv_str = "-"

        for mcid in g["mlog"]:
            if mcid not in cases:
                continue
            mdata = cases[mcid]
            mr = mdata["rows_s"]
            cv_str = f"{mdata['cv']*100:.1f}%"
            oh = ""
            if g["baseline"] and g["baseline"] in cases:
                bl_r = cases[g["baseline"]]["rows_s"]
                oh_pct = (mr - bl_r) / bl_r * 100
                oh = f"{oh_pct:+.1f}%"
            if mdata["meta"]["mlog_shard"] == "noshard":
                noshard_rows = f"{mr:.0f}"
                noshard_oh = oh
            else:
                shard_rows = f"{mr:.0f}"
                shard_oh = oh

        print(
            f"{batch:>5} | {txn:<12} | {rate_cat:<10} | {bl_rows:>16} | "
            f"{noshard_rows:>20} | {noshard_oh:>7} | {shard_rows:>18} | {shard_oh:>7} | {cv_str:>5}"
        )

    # ---------- latency comparison table ----------
    print()
    print("=" * 90)
    print("LATENCY COMPARISON")
    print("=" * 90)
    print(f"{'Case#':>5} | {'Scenario':<14} | {'Batch':>5} | {'TxnMode':<12} | "
          f"{'Avg(ms)':>8} | {'P99(ms)':>8} | {'Max(ms)':>9} | {'Status':>7}")
    print("-" * 90)

    for cid in sorted(cases.keys()):
        d = cases[cid]
        s = d["summary"]
        m = d["meta"]
        status = "PASS"
        if d["cv"] > 0.10:
            status = "WARN"
        avg_l = f"{s.get('avg_lat', 0):.2f}"
        p99_l = f"{s.get('p99_lat', 0):.2f}"
        max_l = f"{s.get('max_lat', 0):.2f}"
        print(
            f"{cid:>5} | {label(m):<14} | {m['batch_size']:>5} | {m['txn_mode']:<12} | "
            f"{avg_l:>8} | {p99_l:>8} | {max_l:>9} | {status:>7}"
        )

    # ---------- cluster metrics table ----------
    if has_metrics:
        print()
        print("=" * 130)
        print("CLUSTER METRICS (per case)")
        print("=" * 130)
        print(f"{'Case#':>5} | {'Scenario':<14} | {'Batch':>5} | "
              f"{'TiDB CPU%':>9} | {'TiKV CPU%':>9} | "
              f"{'Prewrite/s':>10} | {'Commit/s':>10} | "
              f"{'Disk MB/s':>9} | {'Disk KB/row':>11}")
        print("-" * 130)

        for cid in sorted(cases.keys()):
            d = cases[cid]
            m = d["meta"]
            met = d["metrics"]
            if met is None:
                print(f"{cid:>5} | {label(m):<14} | {m['batch_size']:>5} | "
                      f"{'N/A':>9} | {'N/A':>9} | "
                      f"{'N/A':>10} | {'N/A':>10} | "
                      f"{'N/A':>9} | {'N/A':>11}")
                continue

            elapsed = met.get("elapsed_secs", 1)
            total_rows = d["total_rows"]

            tidb_cpu = met.get("tidb_cpu_avg_pct", 0)
            tikv_cpu = met.get("tikv_cpu_avg_pct", 0)
            prewrite_cnt = met.get("tikv_prewrite_count", 0)
            commit_cnt = met.get("tikv_commit_count", 0)
            disk_gb = met.get("tikv_disk_written_gb", 0)

            prewrite_s = prewrite_cnt / elapsed if elapsed > 0 else 0
            commit_s = commit_cnt / elapsed if elapsed > 0 else 0
            disk_mb_s = disk_gb * 1024 / elapsed if elapsed > 0 else 0
            disk_kb_row = (disk_gb * 1024 * 1024 / total_rows) if total_rows > 0 else 0

            print(f"{cid:>5} | {label(m):<14} | {m['batch_size']:>5} | "
                  f"{tidb_cpu:>8.1f}% | {tikv_cpu:>8.1f}% | "
                  f"{prewrite_s:>10,.0f} | {commit_s:>10,.0f} | "
                  f"{disk_mb_s:>9.1f} | {disk_kb_row:>11.2f}")

        # ---------- metrics overhead comparison ----------
        print()
        print("=" * 130)
        print("METRICS OVERHEAD (baseline vs mlog)")
        print("=" * 130)
        print(f"{'Comparison':<30} | "
              f"{'TiDB CPU OH%':>12} | {'TiKV CPU OH%':>12} | "
              f"{'Prewrite/s OH%':>14} | {'Commit/s OH%':>13} | "
              f"{'Disk MB/s OH%':>13} | {'Disk KB/row OH%':>15}")
        print("-" * 130)

        for gk in sorted(groups.keys()):
            g = groups[gk]
            if g["baseline"] is None or g["baseline"] not in cases:
                continue
            bl = cases[g["baseline"]]
            bl_met = bl["metrics"]
            if bl_met is None:
                continue

            bl_elapsed = bl_met.get("elapsed_secs", 1)
            bl_total_rows = bl["total_rows"]
            bl_prewrite_s = bl_met.get("tikv_prewrite_count", 0) / bl_elapsed if bl_elapsed > 0 else 0
            bl_commit_s = bl_met.get("tikv_commit_count", 0) / bl_elapsed if bl_elapsed > 0 else 0
            bl_disk_mb_s = bl_met.get("tikv_disk_written_gb", 0) * 1024 / bl_elapsed if bl_elapsed > 0 else 0
            bl_disk_kb_row = (bl_met.get("tikv_disk_written_gb", 0) * 1024 * 1024 / bl_total_rows) if bl_total_rows > 0 else 0

            for mcid in sorted(g["mlog"]):
                if mcid not in cases:
                    continue
                md = cases[mcid]
                mm = md["metrics"]
                if mm is None:
                    continue

                m_elapsed = mm.get("elapsed_secs", 1)
                m_total_rows = md["total_rows"]
                m_prewrite_s = mm.get("tikv_prewrite_count", 0) / m_elapsed if m_elapsed > 0 else 0
                m_commit_s = mm.get("tikv_commit_count", 0) / m_elapsed if m_elapsed > 0 else 0
                m_disk_mb_s = mm.get("tikv_disk_written_gb", 0) * 1024 / m_elapsed if m_elapsed > 0 else 0
                m_disk_kb_row = (mm.get("tikv_disk_written_gb", 0) * 1024 * 1024 / m_total_rows) if m_total_rows > 0 else 0

                comp_label = f"#{g['baseline']} vs #{mcid} ({label(md['meta'])})"

                tidb_oh = fmt_oh(mm.get("tidb_cpu_avg_pct", 0), bl_met.get("tidb_cpu_avg_pct", 0))
                tikv_oh = fmt_oh(mm.get("tikv_cpu_avg_pct", 0), bl_met.get("tikv_cpu_avg_pct", 0))
                pw_oh = fmt_oh(m_prewrite_s, bl_prewrite_s)
                cm_oh = fmt_oh(m_commit_s, bl_commit_s)
                dk_oh = fmt_oh(m_disk_mb_s, bl_disk_mb_s)
                dkr_oh = fmt_oh(m_disk_kb_row, bl_disk_kb_row)

                print(f"{comp_label:<30} | "
                      f"{tidb_oh:>12} | {tikv_oh:>12} | "
                      f"{pw_oh:>14} | {cm_oh:>13} | "
                      f"{dk_oh:>13} | {dkr_oh:>15}")

    # ---------- write per-case timeseries CSV ----------
    for cid in sorted(cases.keys()):
        d = cases[cid]
        ts_path = os.path.join(results_dir, f"case_{cid}_timeseries.csv")
        batch = d["meta"]["batch_size"]
        with open(ts_path, "w", newline="") as f:
            w = csv.writer(f)
            w.writerow(["timestamp", "tps", "rows_s", "lat_99"])
            for ts, tps, lat99 in d["timeseries"]:
                w.writerow([ts, f"{tps:.2f}", f"{tps * batch:.2f}", f"{lat99:.2f}"])

    # ---------- write summary CSV (with metrics) ----------
    summary_path = os.path.join(results_dir, "summary.csv")
    with open(summary_path, "w", newline="") as f:
        w = csv.writer(f)
        w.writerow([
            "case_id", "scenario", "mlog_shard", "batch_size", "txn_mode",
            "rate", "tps", "rows_s", "avg_lat_ms", "p99_lat_ms", "max_lat_ms",
            "cv", "overhead_pct", "status",
            "tidb_cpu_pct", "tikv_cpu_pct",
            "prewrite_per_s", "commit_per_s",
            "disk_mb_per_s", "disk_kb_per_row",
        ])
        for cid in sorted(cases.keys()):
            d = cases[cid]
            m = d["meta"]
            s = d["summary"]
            met = d["metrics"]
            gk = group_key(m)
            oh = ""
            if m["scenario"] != "baseline" and gk in groups:
                bl_cid = groups[gk]["baseline"]
                if bl_cid and bl_cid in cases:
                    bl_r = cases[bl_cid]["rows_s"]
                    oh = f"{(d['rows_s'] - bl_r) / bl_r * 100:.2f}"
            status = "PASS" if d["cv"] <= 0.10 else "WARN"

            # Compute per-second and per-row metrics
            tidb_cpu = tikv_cpu = prewrite_s = commit_s = disk_mb_s = disk_kb_row = ""
            if met is not None:
                elapsed = met.get("elapsed_secs", 1)
                total_rows = d["total_rows"]
                tidb_cpu = f"{met.get('tidb_cpu_avg_pct', 0):.1f}"
                tikv_cpu = f"{met.get('tikv_cpu_avg_pct', 0):.1f}"
                prewrite_s = f"{met.get('tikv_prewrite_count', 0) / elapsed:.0f}" if elapsed > 0 else ""
                commit_s = f"{met.get('tikv_commit_count', 0) / elapsed:.0f}" if elapsed > 0 else ""
                disk_gb = met.get("tikv_disk_written_gb", 0)
                disk_mb_s = f"{disk_gb * 1024 / elapsed:.1f}" if elapsed > 0 else ""
                disk_kb_row = f"{disk_gb * 1024 * 1024 / total_rows:.2f}" if total_rows > 0 else ""

            w.writerow([
                cid, m["scenario"], m["mlog_shard"], m["batch_size"],
                m["txn_mode"], m["rate"],
                f"{s.get('tps', 0):.2f}",
                f"{d['rows_s']:.2f}",
                f"{s.get('avg_lat', 0):.2f}",
                f"{s.get('p99_lat', 0):.2f}",
                f"{s.get('max_lat', 0):.2f}",
                f"{d['cv']:.4f}",
                oh,
                status,
                tidb_cpu, tikv_cpu,
                prewrite_s, commit_s,
                disk_mb_s, disk_kb_row,
            ])

    print(f"\nTimeseries CSVs written to {results_dir}/case_*_timeseries.csv")
    print(f"Summary CSV written to {summary_path}")


if __name__ == "__main__":
    main()
