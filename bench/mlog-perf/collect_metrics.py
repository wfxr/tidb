#!/usr/bin/env python3
"""Collect cluster metrics snapshots for mlog benchmark.

Subcommands:
  discover  - discover cluster nodes via tiup
  snapshot  - take a metrics snapshot (before/after)
  compute   - compute deltas between before/after snapshots
"""

import argparse
import json
import os
import re
import subprocess
import sys
import time
import urllib.request


CURL_TIMEOUT = 10
FETCH_RETRIES = 3
FETCH_BACKOFF = 2  # seconds


def fetch_metrics(url):
    """Fetch Prometheus metrics text from a URL, retrying on failure."""
    last_err = None
    for attempt in range(1, FETCH_RETRIES + 1):
        try:
            req = urllib.request.Request(url)
            with urllib.request.urlopen(req, timeout=CURL_TIMEOUT) as resp:
                return resp.read().decode("utf-8", errors="replace")
        except Exception as e:
            last_err = e
            if attempt < FETCH_RETRIES:
                print(f"[WARN] fetch {url} failed (attempt {attempt}/{FETCH_RETRIES}): {e}, retrying...",
                      file=sys.stderr)
                time.sleep(FETCH_BACKOFF)
    raise RuntimeError(f"Failed to fetch {url} after {FETCH_RETRIES} attempts: {last_err}")


def extract_metric(text, name, labels=None):
    """Extract metric value(s) from Prometheus text format.

    Returns the sum of all matching lines' values, or None if no match.
    If *labels* is a dict, only lines whose labels are a superset of it match.
    """
    total = 0.0
    found = False
    for line in text.splitlines():
        if line.startswith("#") or not line.startswith(name):
            continue
        rest = line[len(name):]
        if not rest or rest[0] not in ("{", " "):
            continue

        if labels is not None:
            if rest[0] != "{":
                continue
            close = rest.index("}")
            label_str = rest[1:close]
            line_labels = dict(re.findall(r'(\w+)="([^"]*)"', label_str))
            if not all(line_labels.get(k) == v for k, v in labels.items()):
                continue
            value_str = rest[close + 1:].strip()
        else:
            if rest[0] == "{":
                value_str = rest[rest.index("}") + 1:].strip()
            else:
                value_str = rest.strip()

        try:
            total += float(value_str)
            found = True
        except ValueError:
            continue

    return total if found else None


def count_cpu_cores(metrics_text):
    """Count CPU cores from node_exporter metrics."""
    cpus = set()
    for line in metrics_text.splitlines():
        if line.startswith("node_cpu_seconds_total{"):
            m = re.search(r'cpu="(\d+)"', line)
            if m:
                cpus.add(m.group(1))
    return len(cpus) if cpus else None


def partition_to_disk(part_device):
    """Map a partition device name to its parent disk.

    e.g. 'nvme1n1p1' -> 'nvme1n1', 'sda1' -> 'sda'
    """
    # NVMe: nvme0n1p1 -> nvme0n1
    m = re.match(r"(nvme\d+n\d+)p\d+$", part_device)
    if m:
        return m.group(1)
    # SCSI/SATA/virtio: sda1 -> sda, vda1 -> vda
    m = re.match(r"([a-z]+)\d+$", part_device)
    if m:
        return m.group(1)
    return part_device


def detect_disk_device(metrics_text, data_dir):
    """Detect the disk device for a data directory using node_exporter filesystem metrics.

    Finds the longest mountpoint prefix of data_dir, then maps
    the filesystem's partition device to the parent disk name.
    """
    # Parse node_filesystem_size_bytes{device="/dev/...", mountpoint="..."}
    mounts = {}  # mountpoint -> device
    for line in metrics_text.splitlines():
        if not line.startswith("node_filesystem_size_bytes{"):
            continue
        dev_m = re.search(r'device="([^"]*)"', line)
        mnt_m = re.search(r'mountpoint="([^"]*)"', line)
        if dev_m and mnt_m:
            mounts[mnt_m.group(1)] = dev_m.group(1)

    if not mounts:
        return None

    # Longest-prefix match (boundary-safe: mountpoint must be "/" or data_dir starts with mount + "/")
    best_mount = None
    best_len = 0
    for mnt in mounts:
        if mnt == "/":
            is_prefix = True
        else:
            is_prefix = data_dir == mnt or data_dir.startswith(mnt + "/")
        if is_prefix and len(mnt) > best_len:
            best_mount = mnt
            best_len = len(mnt)

    if best_mount is None:
        return None

    device_path = mounts[best_mount]  # e.g. "/dev/nvme1n1p1"
    device_name = device_path.split("/")[-1]
    return partition_to_disk(device_name)


# ---------- discover ----------

def cmd_discover(args):
    cluster = args.cluster
    # Ensure ~/.tiup/bin is on PATH (non-interactive SSH may not source .bashrc)
    tiup_bin = os.path.expanduser("~/.tiup/bin")
    env = os.environ.copy()
    if tiup_bin not in env.get("PATH", ""):
        env["PATH"] = tiup_bin + ":" + env.get("PATH", "")

    try:
        result = subprocess.run(
            ["tiup", "cluster", "display", cluster, "--format", "json"],
            capture_output=True, text=True, timeout=30, env=env,
        )
        if result.returncode != 0:
            print(f"[ERROR] tiup cluster display failed: {result.stderr.strip()}", file=sys.stderr)
            sys.exit(1)
        cluster_info = json.loads(result.stdout)
    except (subprocess.TimeoutExpired, json.JSONDecodeError, FileNotFoundError) as e:
        print(f"[ERROR] Failed to get cluster info: {e}", file=sys.stderr)
        sys.exit(1)

    tidb_nodes = []
    tikv_nodes = []
    tikv_data_dir = None

    for inst in cluster_info.get("instances", []):
        role = inst.get("role", "").split()[0]  # "tidb (patched)" -> "tidb"
        host = inst.get("host", "")
        ports = inst.get("ports", "")
        parts = ports.split("/")
        if len(parts) < 2:
            continue
        status_port = parts[1]

        if role == "tidb":
            tidb_nodes.append({"host": host, "status_port": status_port})
        elif role == "tikv":
            tikv_nodes.append({"host": host, "status_port": status_port})
            if tikv_data_dir is None:
                tikv_data_dir = inst.get("data_dir", "")

    # Fetch node_exporter metrics once per unique host (reused for cores + disk detection)
    all_hosts = set(n["host"] for n in tidb_nodes + tikv_nodes)
    node_exporter_cache = {}  # host -> metrics text
    for host in sorted(all_hosts):
        node_exporter_cache[host] = fetch_metrics(f"http://{host}:9100/metrics")

    # CPU cores per host
    node_cores = {}
    for host, text in node_exporter_cache.items():
        cores = count_cpu_cores(text)
        if cores:
            node_cores[host] = cores
        else:
            print(f"[WARN] Could not determine CPU cores for {host}, defaulting to 1", file=sys.stderr)
            node_cores[host] = 1

    # Auto-detect TiKV disk device (probe first TiKV node, cluster is homogeneous)
    disk_device = None
    if tikv_nodes and tikv_data_dir:
        probe_host = tikv_nodes[0]["host"]
        text = node_exporter_cache.get(probe_host, "")
        disk_device = detect_disk_device(text, tikv_data_dir)
        if disk_device:
            print(f"[DISCOVER] Auto-detected disk device: {disk_device}  (data_dir={tikv_data_dir})")
        else:
            print("[WARN] Could not auto-detect TiKV disk device; disk metrics will be skipped",
                  file=sys.stderr)

    nodes = {
        "tidb": tidb_nodes,
        "tikv": tikv_nodes,
        "cores": node_cores,
        "disk_device": disk_device,
    }
    with open(args.output, "w") as f:
        json.dump(nodes, f, indent=2)

    print(f"[DISCOVER] Cluster: {cluster}")
    for role in ("tidb", "tikv"):
        for n in nodes[role]:
            h = n["host"]
            print(f"  {role:5s}  {h}:{n['status_port']}  ({node_cores.get(h, '?')} cores)")


# ---------- snapshot ----------

def cmd_snapshot(args):
    with open(args.nodes) as f:
        nodes = json.load(f)

    lines = []

    # TiDB: process_cpu_seconds_total
    for n in nodes["tidb"]:
        host, port = n["host"], n["status_port"]
        text = fetch_metrics(f"http://{host}:{port}/metrics")
        val = extract_metric(text, "process_cpu_seconds_total")
        if val is not None:
            lines.append(f"tidb {host} process_cpu_seconds_total {val}")

    # TiKV: cpu + gRPC counters
    for n in nodes["tikv"]:
        host, port = n["host"], n["status_port"]
        text = fetch_metrics(f"http://{host}:{port}/metrics")
        val = extract_metric(text, "process_cpu_seconds_total")
        if val is not None:
            lines.append(f"tikv {host} process_cpu_seconds_total {val}")
        val = extract_metric(text, "tikv_grpc_msg_duration_seconds_count", {"type": "kv_prewrite"})
        if val is not None:
            lines.append(f"tikv {host} grpc_prewrite_count {val}")
        val = extract_metric(text, "tikv_grpc_msg_duration_seconds_count", {"type": "kv_commit"})
        if val is not None:
            lines.append(f"tikv {host} grpc_commit_count {val}")

    # Node exporter for TiKV hosts: disk written bytes
    disk_device = nodes.get("disk_device")
    if disk_device:
        tikv_hosts = set(n["host"] for n in nodes["tikv"])
        for host in sorted(tikv_hosts):
            text = fetch_metrics(f"http://{host}:9100/metrics")
            val = extract_metric(text, "node_disk_written_bytes_total", {"device": disk_device})
            if val is not None:
                lines.append(f"tikv {host} disk_written_bytes {val}")

    outfile = os.path.join(args.output_dir, f"case_{args.case_id}.metrics.{args.label}")
    with open(outfile, "w") as f:
        f.write("\n".join(lines) + "\n")

    print(f"[SNAPSHOT] {args.label}: {len(lines)} metrics -> {outfile}")


# ---------- compute ----------

def load_snapshot(path):
    """Load a metrics snapshot into {(role, ip, metric): value}."""
    data = {}
    with open(path) as f:
        for line in f:
            parts = line.split()
            if len(parts) != 4:
                continue
            role, ip, metric, val = parts
            data[(role, ip, metric)] = float(val)
    return data


def cmd_compute(args):
    with open(args.nodes) as f:
        nodes = json.load(f)

    before_path = os.path.join(args.output_dir, f"case_{args.case_id}.metrics.before")
    after_path = os.path.join(args.output_dir, f"case_{args.case_id}.metrics.after")

    if not os.path.exists(before_path) or not os.path.exists(after_path):
        print(f"[WARN] Missing metrics files for case #{args.case_id}", file=sys.stderr)
        return

    before = load_snapshot(before_path)
    after = load_snapshot(after_path)
    elapsed = args.elapsed
    cores = nodes.get("cores", {})

    def cpu_pcts(role, node_list):
        pcts = []
        for n in node_list:
            host = n["host"]
            key = (role, host, "process_cpu_seconds_total")
            if key in before and key in after:
                delta = after[key] - before[key]
                pcts.append(delta / elapsed / cores.get(host, 1) * 100)
        return pcts

    tidb_cpu = cpu_pcts("tidb", nodes["tidb"])
    tikv_cpu = cpu_pcts("tikv", nodes["tikv"])

    prewrite_total = 0.0
    commit_total = 0.0
    for n in nodes["tikv"]:
        host = n["host"]
        for metric, attr in [("grpc_prewrite_count", "prewrite"), ("grpc_commit_count", "commit")]:
            key = ("tikv", host, metric)
            if key in before and key in after:
                delta = after[key] - before[key]
                if attr == "prewrite":
                    prewrite_total += delta
                else:
                    commit_total += delta

    disk_written_bytes = 0.0
    for host in set(n["host"] for n in nodes["tikv"]):
        key = ("tikv", host, "disk_written_bytes")
        if key in before and key in after:
            disk_written_bytes += after[key] - before[key]
    disk_written_gb = disk_written_bytes / (1024 ** 3)

    tidb_cpu_avg = sum(tidb_cpu) / len(tidb_cpu) if tidb_cpu else 0
    tikv_cpu_avg = sum(tikv_cpu) / len(tikv_cpu) if tikv_cpu else 0

    def node_info(role_nodes, pcts):
        if not pcts:
            return ""
        core_vals = set(cores.get(n["host"], "?") for n in role_nodes)
        if len(core_vals) == 1:
            return f"({len(pcts)} nodes x {core_vals.pop()} cores)"
        return f"({len(pcts)} nodes)"

    tidb_info = node_info(nodes["tidb"], tidb_cpu)
    tikv_info = node_info(nodes["tikv"], tikv_cpu)

    # Write summary file
    summary_path = os.path.join(args.output_dir, f"case_{args.case_id}.metrics.summary")
    with open(summary_path, "w") as f:
        f.write(f"tidb_cpu_avg_pct={tidb_cpu_avg:.1f}\n")
        f.write(f"tikv_cpu_avg_pct={tikv_cpu_avg:.1f}\n")
        f.write(f"tikv_prewrite_count={prewrite_total:.0f}\n")
        f.write(f"tikv_commit_count={commit_total:.0f}\n")
        f.write(f"tikv_disk_written_gb={disk_written_gb:.2f}\n")
        f.write(f"elapsed_secs={elapsed}\n")

    # Print to stdout
    print(f"[METRICS] Case #{args.case_id}:")
    print(f"  TiDB CPU avg: {tidb_cpu_avg:.1f}%  {tidb_info}")
    print(f"  TiKV CPU avg: {tikv_cpu_avg:.1f}%  {tikv_info}")
    print(f"  TiKV prewrite RPCs: {prewrite_total:,.0f}")
    print(f"  TiKV commit RPCs:   {commit_total:,.0f}")
    print(f"  TiKV disk written:  {disk_written_gb:.1f} GB")


# ---------- main ----------

def main():
    parser = argparse.ArgumentParser(description="Collect cluster metrics for mlog benchmark")
    sub = parser.add_subparsers(dest="command", required=True)

    p = sub.add_parser("discover")
    p.add_argument("--cluster", default="bench-mlog")
    p.add_argument("--output", required=True)

    p = sub.add_parser("snapshot")
    p.add_argument("--nodes", required=True)
    p.add_argument("--output-dir", required=True)
    p.add_argument("--case-id", required=True)
    p.add_argument("--label", required=True, choices=["before", "after"])

    p = sub.add_parser("compute")
    p.add_argument("--nodes", required=True)
    p.add_argument("--output-dir", required=True)
    p.add_argument("--case-id", required=True)
    p.add_argument("--elapsed", required=True, type=float)

    args = parser.parse_args()
    {"discover": cmd_discover, "snapshot": cmd_snapshot, "compute": cmd_compute}[args.command](args)


if __name__ == "__main__":
    main()
