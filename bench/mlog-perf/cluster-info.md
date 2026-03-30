# bench-mlog 测试集群信息

## GCP 基础信息

- Project: `gcp-tikv-transaction-dev`
- Zone: `us-east1-b`
- GCS Bucket: `gs://oltp-bench-us-east1`
- Deployment name: `bench-mlog`

## 节点列表

| 角色 | 实例名 | 机型 | 数据盘 |
|---|---|---|---|
| PD | bench-mlog-pd-0 | c4-standard-8 | 64GB hyperdisk-balanced → /data |
| TiKV | bench-mlog-tikv-0 | c4-highmem-16 | 1TB hyperdisk-balanced → /data |
| TiKV | bench-mlog-tikv-1 | c4-highmem-16 | 1TB hyperdisk-balanced → /data |
| TiKV | bench-mlog-tikv-2 | c4-highmem-16 | 1TB hyperdisk-balanced → /data |
| TiDB | bench-mlog-tidb-0 | c4-standard-16 | 无（系统盘） |
| TiDB | bench-mlog-tidb-1 | c4-standard-16 | 无（系统盘） |
| TiDB | bench-mlog-tidb-2 | c4-standard-16 | 无（系统盘） |
| TiFlash | bench-mlog-tiflash-0 | c4-standard-32 | 1TB hyperdisk-balanced → /data |
| 压测机 | bench-mlog-load | c4-standard-8 | 64GB hyperdisk-balanced → /data |

查看实际 IP：

```bash
# 查看所有实例内网 IP
./gcloud/resolve-hosts.sh

# 查看 TiUP 集群状态（含 IP）
ssh bench-mlog-load "tiup cluster display bench-mlog"
```

## 部署目录

所有节点统一：

- deploy_dir: `/data/tidb-deploy`
- data_dir: `/data/tidb-data`

## SSH 配置

首次使用运行一次 setup 脚本（拉取 SSH 密钥 + 配置 ~/.ssh/config）：

```bash
./gcloud/setup-local.sh
```

配好后即可直接使用标准 SSH/SCP 命令：

```bash
# 登录压测机（主要操作入口）
ssh bench-mlog-load

# 登录其他节点
ssh bench-mlog-pd-0
ssh bench-mlog-tikv-0
ssh bench-mlog-tidb-0
ssh bench-mlog-tiflash-0

# 直接执行远程命令
ssh bench-mlog-load "tiup cluster display bench-mlog"

# 上传文件
scp -r ./bench bench-mlog-load:~/
```

### MySQL

从压测机上连接 TiDB（IP 从 tiup cluster display 获取）：

```bash
# 查看 TiDB 节点 IP
tiup cluster display bench-mlog | grep tidb

# 连接任一 TiDB 节点
mysql -h <tidb-ip> -P 4000 -u root
```

### PD

```bash
# 查看 PD 节点 IP
tiup cluster display bench-mlog | grep pd

# Dashboard: http://<pd-ip>:2379/dashboard
# pd-ctl
tiup ctl:v8.5.4 pd -u http://<pd-ip>:2379
```

## 集群管理

```bash
tiup cluster display bench-mlog
tiup cluster start bench-mlog
tiup cluster stop bench-mlog
tiup cluster restart bench-mlog
```

## 版本验证

```bash
tiup cluster exec bench-mlog -R tidb --command "/data/tidb-deploy/tidb-4000/bin/tidb-server -V"
tiup cluster exec bench-mlog -R tikv --command "/data/tidb-deploy/tikv-20160/bin/tikv-server --version"
tiup cluster exec bench-mlog -R tiflash --command "/data/tidb-deploy/tiflash-9000/bin/tiflash/tiflash version"
```

## Patched 二进制

存放于 `gs://oltp-bench-us-east1/tmp/zwx/`：

```bash
# 下载
gcloud storage cp gs://oltp-bench-us-east1/tmp/zwx/*.tar.gz .

# 重新 patch（集群停止状态下）
tiup cluster patch bench-mlog tidb-linux-amd64.tar.gz -R tidb --overwrite --offline
tiup cluster patch bench-mlog tikv-linux-amd64.tar.gz -R tikv --overwrite --offline
tiup cluster patch bench-mlog tiflash-linux-amd64.tar.gz -R tiflash --overwrite --offline
```

## 修改集群配置

`tiup cluster edit-config` 是交互式的，可通过设置 `EDITOR` 环境变量实现非交互式修改。

### pessimistic-auto-commit（悲观事务压测必需）

```bash
# 1. 修改配置（在压测机上执行）
#    gcloud/edit_cfg.sh 会在拓扑 YAML 末尾追加 server_configs
echo y | EDITOR=~/bench/mlog-perf/gcloud/edit_cfg.sh tiup cluster edit-config bench-mlog

# 2. 重载 TiDB 节点使配置生效
tiup cluster reload bench-mlog -R tidb -y

# 3. 验证所有 TiDB 节点已生效
mysql -h <tidb-ip> -P 4000 -u root -sN \
  -e "SHOW CONFIG WHERE name='pessimistic-txn.pessimistic-auto-commit';"
# 预期输出：3 行均为 true
```

## 暂停 / 恢复 GCP 实例

不做测试时 stop VM 可节省开销，stop 后 CPU/内存不计费，仅磁盘继续计费。

```bash
# 先在压测机上停集群
tiup cluster stop bench-mlog

# 停止所有 VM
gcloud compute instances stop \
  bench-mlog-pd-0 bench-mlog-tikv-{0,1,2} bench-mlog-tidb-{0,1,2} \
  bench-mlog-tiflash-0 bench-mlog-load \
  --zone us-east1-b --project gcp-tikv-transaction-dev

# 恢复所有 VM
gcloud compute instances start \
  bench-mlog-pd-0 bench-mlog-tikv-{0,1,2} bench-mlog-tidb-{0,1,2} \
  bench-mlog-tiflash-0 bench-mlog-load \
  --zone us-east1-b --project gcp-tikv-transaction-dev

# 再在压测机上启集群
tiup cluster start bench-mlog
```

## 创建 / 销毁集群

### 创建

```bash
# 1. 创建 GCP 资源
gcloud deployment-manager deployments create bench-mlog \
  --config gcloud/bench-mlog.yaml --project gcp-tikv-transaction-dev

# 2. 等待 VM 启动完成（约 2-3 分钟），然后生成 topology.yaml
./gcloud/gen-topology.sh

# 3. 上传 bench 目录到压测机
scp -r ./bench bench-mlog-load:~/

# 4. 在压测机上部署 TiUP 集群
ssh bench-mlog-load "~/.tiup/bin/tiup cluster deploy bench-mlog v8.5.4 ~/bench/mlog-perf/topology.yaml -y"
ssh bench-mlog-load "~/.tiup/bin/tiup cluster start bench-mlog"
```

### 销毁

```bash
# 1. 销毁 TiUP 集群（在压测机上）
tiup cluster destroy bench-mlog

# 2. 删除 GCP 资源（本地执行）
gcloud deployment-manager deployments delete bench-mlog --project gcp-tikv-transaction-dev
```
