# Mlog Bench Infrastructure Reference

## GCP

- Project: `gcp-tikv-transaction-dev`
- Zone: `us-east1-b`
- Deployment: `bench-mlog`

### VM Instances

| Role | Instance | Internal IP | Machine Type |
|------|----------|-------------|-------------|
| PD | bench-mlog-pd-0 | 10.142.0.11 | c4-standard-8 |
| TiKV x3 | bench-mlog-tikv-{0,1,2} | 10.142.0.{10,12,8} | c4-highmem-16 |
| TiDB x3 | bench-mlog-tidb-{0,1,2} | 10.142.0.{7,6,5} | c4-standard-16 |
| TiFlash | bench-mlog-tiflash-0 | 10.142.0.9 | c4-standard-32 |
| Load Gen | bench-mlog-load | 10.142.0.13 | c4-standard-8 |

### Start/Stop VMs

```bash
# Start
gcloud compute instances start \
  bench-mlog-pd-0 bench-mlog-tikv-{0,1,2} bench-mlog-tidb-{0,1,2} \
  bench-mlog-tiflash-0 bench-mlog-load \
  --zone us-east1-b --project gcp-tikv-transaction-dev

# Stop
gcloud compute instances stop \
  bench-mlog-pd-0 bench-mlog-tikv-{0,1,2} bench-mlog-tidb-{0,1,2} \
  bench-mlog-tiflash-0 bench-mlog-load \
  --zone us-east1-b --project gcp-tikv-transaction-dev
```

## SSH / SCP

Helper scripts in `bench/mlog-perf/gcloud/`:

```bash
# SSH
./gcloud/ssh.sh bench-mlog-load [command]

# SCP (local -> remote)
./gcloud/scp.sh bench-mlog-load <local-path> <remote-path>
```

Connection details: user=`transaction`, key=`~/.ssh/gcp-tikv-transaction-dev.pem`.

## TiUP Cluster

From the load machine:

```bash
~/.tiup/bin/tiup cluster start bench-mlog
~/.tiup/bin/tiup cluster stop bench-mlog -y
~/.tiup/bin/tiup cluster display bench-mlog
```

## Bench Scripts on Load Machine

Scripts are synced to `~/bench/mlog-perf/` on the load machine.

## Key Gotchas

1. **Must pass `--host`**: sysbench default is `127.0.0.1`, but load machine is a separate VM. Use `--host 10.142.0.7` (or a TiDB node IP).
2. **SCP trailing slash**: `./gcloud/scp.sh bench-mlog-load ./bench '~/'` syncs the bench dir to remote home.
3. **pessimistic-auto-commit**: Required for P1 cases. Enable via `gcloud/edit_cfg.sh` + `tiup cluster reload -R tidb`.
4. **Download results**: Use scp in reverse direction (direct scp command, not the helper which is upload-only).
