# RUNBOOK — ko 生产部署手册

本手册面向已经把 ko 用在生产环境（或准备用）的运维同学。覆盖 HA 部署、离线环境、节点扩缩容、Dashboard 加固，以及常见故障的处理。

## 0. 前置检查

任何 `ko init` 之前，跑一遍 doctor：

```bash
ko doctor --config cluster.hcl
```

正常情况下应看到：
- kernel ≥ 5.4（推荐 5.15+）
- swap 已关闭（k8s 要求）
- 端口 6443 / 10250 / 10251 / 10252 在 master 上未被占用
- containerd 或 docker 已选其一安装
- 所有 master/worker 主机 SSH 可达

如果 doctor 报红，按它的提示修。**不要**绕开 doctor 直接 init。

## 1. HA 多 master 部署

### 1.1 拓扑

最少 3 master（容忍 1 故障），推荐 5 master（容忍 2 故障）。worker 数量按业务规模定。

```
            ┌────────────────────────────────────┐
            │   kube-vip (DaemonSet, hostPort)   │
            │       VIP: 10.0.0.10:6443         │
            └──────┬─────────┬──────────┬────────┘
                   │         │          │
              ┌────▼──┐ ┌───▼───┐ ┌───▼───┐
              │ m1    │ │ m2    │ │ m3    │
              │ etcd  │ │ etcd  │ │ etcd  │   stacked etcd
              └───────┘ └───────┘ └───────┘
                   │         │          │
              ┌────▼─────────▼──────────▼────┐
              │           Cilium             │   kubeProxyReplacement=strict
              └────┬─────────┬──────────┬────┘
                   │         │          │
              ┌────▼──┐ ┌───▼───┐ ┌───▼───┐
              │ w1    │ │ w2    │ │ w3    │
              └───────┘ └───────┘ └───────┘
```

### 1.2 集群配置

`cluster.hcl` 关键字段：

```hcl
cluster {
  name    = "prod"
  version = "1.30.0"
  cidr    = "10.244.0.0/16"   # pod CIDR
  svc_cidr = "10.96.0.0/12"   # service CIDR
}

ha {
  vip = "10.0.0.10"           # VIP 必须在所有 master 所在二层网络内
}

nodes {
  masters = ["10.0.0.11", "10.0.0.12", "10.0.0.13"]
  workers = ["10.0.0.21", "10.0.0.22", "10.0.0.23"]
  ssh {
    user    = "root"
    port    = 22
    key_file = "/root/.ssh/id_rsa"
    # password = "..."  # 不推荐，用 key
  }
}

runtime { default = "containerd" }
cni     { plugin  = "cilium" }
```

### 1.3 init 流程

```bash
ko init --config cluster.hcl
```

ko 会按顺序：
1. 在所有节点装 containerd（或 docker）
2. 装 kubeadm / kubelet / kubectl（apt 源或 yum 源）
3. 在 m1 上 `kubeadm init` 带 `--certificate-validity=876000h`（100 年）
4. 在所有节点部署 kube-vip（绑定 VIP）
5. 在 m2、m3 上 `kubeadm join --control-plane`（自动拷贝 CA）
6. 在所有 worker 上 `kubeadm join`
7. 装 Cilium（kubeProxyReplacement=strict）

kubeconfig 落在 `~/.ko/kube/admin.conf`。后续 kubectl 这么用：

```bash
export KUBECONFIG=~/.ko/kube/admin.conf
kubectl get nodes
```

### 1.4 100 年证书

ko 默认 `--certificate-validity=876000h`（100 年）。`ko cluster certs` 能看每个 master 上的证书剩余有效期：

```bash
ko cluster certs --config cluster.hcl
```

如果看到某些证书是 1 年有效期，说明那台 master 是 `kubeadm join` 后才签的（默认 join 不覆盖 cert 有效期，需要显式传 `--certificate-validity`）。重签：

```bash
# 在目标 master 上
sudo kubeadm init phase certs all --certificate-validity=876000h
```

## 2. 离线部署

### 2.1 在能上网的机器上打包

```bash
# 一次同时构建 amd64 + arm64，输出 -multi.oci.tar.gz
ko pack build --arch all --output ./dist --version v0.0.1

# 产物：
#   dist/ko-v0.0.1-multi.oci.tar.gz  ← 多架构 bundle
```

### 2.2 拷贝到目标机器

```bash
# 二进制 + bundle + cluster.hcl
scp bin/ko-linux-amd64 dist/ko-v0.0.1-multi.oci.tar.gz cluster.hcl root@10.0.0.11:
```

### 2.3 离线 init

```bash
ssh root@10.0.0.11
./ko init --config cluster.hcl --offline --bundle ./ko-v0.0.1-multi.oci.tar.gz
```

`--offline` 让 ko：
- 不再访问外网（github release / apt 源 / helm repo）
- 直接从 bundle 导入 containerd / docker / helm chart / k8s 镜像
- `--bundle` 指定 bundle 路径，不指定则默认 `~/.ko/bundles/`

### 2.4 增量更新 bundle

bundle 是不可变的（每个 bundle 一个 OCI digest）。新版本重打：

```bash
ko pack build --arch all --output ./dist --version v0.0.2
```

## 3. 节点扩缩容

### 3.1 加 worker

新机器先确保 SSH 可达、kernel ≥ 5.4、swap 关闭。然后：

```bash
ko node add 10.0.0.24 --role worker --config cluster.hcl
```

ko 会：
1. 在新机器装 runtime
2. 在新机器装 kubeadm/kubelet
3. `kubeadm join` 到集群
4. 等 Node Ready 后退出

### 3.2 加 master

```bash
ko node add 10.0.0.14 --role master --config cluster.hcl
```

ko 会：
1. 装 runtime + kubeadm
2. `kubeadm join --control-plane`（自动拷贝 CA + 签 100 年证书）
3. 自动更新 VIP 的 endpoints（如果 VIP 是 LB 后端模式）

**注意**：加 master 必须满足奇数台（etcd 多数派），别加第 4 台 master 后再加第 6 台。

### 3.3 删节点

```bash
# 默认 drain + delete + reset
ko node remove 10.0.0.24 --config cluster.hcl

# 强制（跳过 drain，用于节点已宕机的情况）
ko node remove 10.0.0.24 --force --config cluster.hcl
```

删 master 会触发 etcd 集群成员变更，**必须**保证剩余 master ≥ 3 且多数派健康。

### 3.4 节点打 label

```bash
ko node label 10.0.0.21 role=gpu --config cluster.hcl
ko node label 10.0.0.21 role- --config cluster.hcl  # 删 label
```

## 4. 主机调优

### 4.1 用 profile

```bash
ko tune apply production --config cluster.hcl   # 默认高规格
ko tune apply dev        --config cluster.hcl   # 开发机
ko tune apply minimal    --config cluster.hcl   # 兜底

# 看当前实际生效配置
ko tune show --config cluster.hcl

# 恢复（删 sysctl 配置 + 重新加载内核默认）
ko tune reset --config cluster.hcl
```

### 4.2 profile 内容

| profile     | sysctl 重点                                 | swap  | modules           |
|-------------|---------------------------------------------|-------|-------------------|
| production  | net.core.somaxconn=65535, fs.file-max 高    | off   | br_netfilter, overlay |
| dev         | 通用，宽松                                  | off   | br_netfilter      |
| minimal     | 不动 / 关闭 swap                            | off   | 不动              |

### 4.3 自定义

`cluster.hcl` 里 override：

```hcl
tune {
  profile = "production"
  sysctl  = {
    "net.ipv4.tcp_keepalive_time" = "60"
    "vm.swappiness"               = "10"
  }
  swap_off = true
}
```

## 5. Dashboard 加固

### 5.1 不要默认 user/password

```bash
# 错误：默认 user=admin，密码靠环境变量
KO_DASHBOARD_PASSWORD=hunter2 ko dashboard --config cluster.hcl

# 正确：显式指定 user，并改默认监听地址
KO_DASHBOARD_PASSWORD=$(openssl rand -hex 32) \
  ko dashboard --config cluster.hcl --user ops --listen 0.0.0.0:8080
```

### 5.2 加 HTTPS（反代）

ko dashboard 是 HTTP，不是 HTTPS。**生产前**用 nginx / caddy 加 TLS：

```nginx
server {
    listen 443 ssl;
    server_name ko.internal.example.com;

    ssl_certificate     /etc/ssl/ko.crt;
    ssl_certificate_key /etc/ssl/ko.key;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Authorization $http_authorization;
        proxy_set_header Host $host;
    }
}
```

### 5.3 速率限制（token bucket）

Dashboard 在 auth 之前挡一层 token bucket，默认 **1 req/s，burst 20**（全局共享，非 per-IP）。防扫描器 / 错误客户端。429 响应带 `Retry-After: 1`。

```bash
# 调紧（生产、放在 nginx 后）
ko dashboard --config cluster.hcl --rate-limit 5 --rate-burst 50

# 关闭（仅本机 dev）
ko dashboard --config cluster.hcl --rate-limit 0
```

### 5.4 审计日志

每次请求写一行到审计文件（默认 `/var/log/ko/dashboard-audit.log`，mode 0600，append-only）。记录 **所有**响应：200 / 401（未认证，user=`-`）/ 429（限流）/ 500（panic）。失败时降级到 `io.Discard` + log，不阻塞请求。

格式（RFC3339Nano 起头）：

```
2026-07-02T10:23:45.123456Z remote=10.0.0.5:54321 user=admin method=POST path=/api/nodes/add status=200 bytes=42 dur=12.3ms
2026-07-02T10:23:46.001000Z remote=10.0.0.5:54322 user=-      method=GET  path=/api/healthz     status=401 bytes=12 dur=200µs
```

```bash
# 改路径
ko dashboard --config cluster.hcl --audit-log /var/log/ko/audit.log

# 关闭（不推荐生产开）
ko dashboard --config cluster.hcl --audit-log ""
```

### 5.5 不要暴露到公网

加完 rate limit + audit 后，basic auth 仍**不是**强认证。**只**监听 127.0.0.1，或放在内网 nginx + TLS 后。生产推荐：

1. `ko dashboard --listen 127.0.0.1:8080`（默认）
2. nginx 监听 443，转发到 8080，TLS 终止
3. audit log 用 `logrotate` 滚动（`/etc/logrotate.d/ko-dashboard`）
4. rate limit 留默认或调高（nginx 自身能挡 L7 DoS）

## 6. 备份与恢复

### 6.1 etcd 备份

```bash
ko cluster backup --config cluster.hcl --output ./etcd-snap-$(date +%Y%m%d).db
```

定时任务建议：

```cron
0 2 * * * /usr/local/bin/ko cluster backup --config /etc/ko/cluster.hcl --output /var/backups/ko/etcd-$(date +\%Y\%m\%d).db
```

### 6.2 恢复

```bash
# v0.0.1+：ko cluster restore 自动跑 stacked restore 全流程
ko cluster restore --snapshot ./ko-etcd-20260701.db --config cluster.hcl

# 流程（用户看到）：
# 1. 停所有 master 的 kubelet
# 2. 把每台 master 的 /var/lib/etcd 移到 .broken-<ts> 旁路保留
# 3. scp snapshot 到每台 master 的 /tmp
# 4. 在每台 master 上 etcdctl snapshot restore（每台用各自的 --name + 完整的 --initial-cluster）
# 5. 起所有 master 的 kubelet
# 6. 等 apiserver /healthz 返回 ok（最多 90s）
```

如果想走手动流程（老版本或 restore 命令坏了）：

```bash
# 1. 停所有 master 上的 kube-apiserver
sudo systemctl stop kubelet

# 2. 在第一个 master 上恢复
ETCDCTL_API=3 etcdctl snapshot restore /var/backups/ko/etcd-20260701.db \
  --data-dir=/var/lib/etcd-restore \
  --name=m1 \
  --initial-cluster=m1=https://10.0.0.11:2380,m2=...,m3=... \
  --initial-advertise-peer-urls=https://10.0.0.11:2380

# 3. 替换 /var/lib/etcd 内容，重启 kubelet
sudo rsync -aP /var/lib/etcd-restore/ /var/lib/etcd/
sudo systemctl start kubelet
```

### 6.3 集群重建（reset）

```bash
ko reset --config cluster.hcl
# 默认行为：在每台节点上：
#   - systemctl stop kubelet / containerd / docker / etcd
#   - kubeadm reset --force（同时清 ctr 容器）
#   - rm -rf /etc/kubernetes /var/lib/{kubelet,etcd,kube-vip}
#   - rm ko-installed systemd units（etcd / ko-etcd-backup / containerd）
#   - rm -rf /etc/cni/net.d /opt/cni/bin /var/lib/cni
#   - flush iptables (filter/nat/mangle/raw) + ipvsadm --clear
#   - delete cni0 / flannel.1 / cilium-* / kube-ipvs0 / veth* interfaces
#   - lazy unmount overlay + kubelet pod volume mounts
#   - rm ko-written config: /etc/containerd/config.toml, /etc/docker/daemon.json
#   - external etcd 模式：先 uninstall 全部 member + 备份目录
# 默认会保留镜像缓存 — back-to-back init 不需要 re-pull

# 完全清干净（dev/debug 反复 init/reset 用）：
ko reset --config cluster.hcl --purge
# --purge 在默认基础上额外清：
#   - ctr -n k8s.io containers delete --force --all
#   - rm -rf /var/lib/containerd /var/run/containerd
#   - rm -rf /var/lib/docker /var/run/docker
#   - rm -rf /var/lib/ko /root/.ko
# 留下 /usr/local/bin/{etcd,etcdctl,containerd,kubelet,kubeadm,kubectl} 等已安装二进制
# （这些不是 ko 唯一的所有物，可能跟其他 workload 共享）
```

reset 是不可逆的——执行前确认 etcd 已经备份。

**幂等性**：reset 脚本里所有清理命令都带 `|| true` 或 `2>/dev/null || true`，在从未 init 过的节点上跑也是 no-op。在调试循环里反复 init/reset 不会留下脏数据。

## 7. 常见故障

### 7.1 init 卡在 `kubeadm init`

看 m1 的 `/var/log/kubelet.log` 和 `journalctl -u kubelet`。

常见原因：
- 端口被占：`netstat -tlnp | grep 6443`
- containerd 没启：`systemctl status containerd`
- kernel 太旧：`uname -r`（要 ≥ 5.4）

### 7.2 join master 失败

`kubeadm join --control-plane` 需要从 m1 拷贝 CA。如果 SSH 不通：

```bash
# 手工从 m1 拷
scp /etc/kubernetes/pki/ca.* root@m2:/etc/kubernetes/pki/
scp /etc/kubernetes/pki/sa.* root@m2:/etc/kubernetes/pki/
scp /etc/kubernetes/pki/front-proxy-ca.* root@m2:/etc/kubernetes/pki/
```

### 7.3 Cilium 起不来

看 cilium pod 日志：

```bash
kubectl -n kube-system logs -l k8s-app=cilium
```

如果是 eBPF 模式要求 kernel ≥ 5.4 但机器只有 4.x，切到 Flannel：

```hcl
cni { plugin = "flannel" }
```

### 7.4 离线 init 找不到镜像

```bash
ko init --config cluster.hcl --offline --bundle ./ko-v0.0.1-multi.oci.tar.gz
```

`--bundle` 必须是 multi-arch bundle 或与目标机器 arch 匹配的 single-arch bundle。`ko pack inspect <bundle>` 看 bundle 的 arches。

### 7.5 Dashboard 401

basic auth 失败。三种情况：
- 没带 `Authorization: Basic` header — 用 `curl -u user:pass`
- 密码错了 — 重启时换环境变量
- 用了 `--user ops` 但 curl 还是 `admin` — 对一下 user

## 8. 升级

v0.0.1 不支持集群内升级。等 v0.1.x 走 `ko upgrade` 子命令。过渡方案：

```bash
ko reset --config cluster.hcl
ko init --config cluster.hcl --offline --bundle ./ko-v0.0.2-multi.oci.tar.gz
```

应用层数据如果有持久卷（PV），不会被 reset 影响。

## 9. 监控

ko v0.0.1 不内置监控。建议：

- Prometheus + Grafana（用 prometheus-operator chart 装）
- node-exporter DaemonSet
- kube-state-metrics
- Alertmanager 路由告警到 Slack / 企业微信 / 钉钉

Cilium 自带 Hubble 可选开（`cilium.enable-hubble=true`），能看网络流。

## 10. 外部 etcd（S14）

外部 etcd 把控制平面的状态从 k8s 集群里挪到独立维护的 etcd 集群上。
ko 负责：二进制下载、mTLS 证书生成、systemd 部署、8 小时自动备份。

### 10.1 什么时候用

- 你有专门的 etcd 团队 / 物理机，需要独立管理 etcd 的备份与扩缩容。
- 控制平面的故障域需要和 master 解耦。
- 业务对 etcd 备份 RPO 有要求（8h 滚动 14 天，比 kubeadm stacked 默认 24h 更激进）。

### 10.2 拓扑与配置

最小配置 3 节点 etcd（容忍 1 故障）+ 3 节点 master + N 节点 worker。
master 与 etcd 物理上分开。

```hcl
cluster {
  name     = "prod"
  version  = "1.35"
  cidr     = "10.244.0.0/16"
  svc_cidr = "10.96.0.0/12"
}

etcd {
  mode      = "external"
  endpoints = ["https://10.0.0.31:2379","https://10.0.0.32:2379","https://10.0.0.33:2379"]
  pki_dir   = "/etc/etcd/pki"
}

members "etcd-1" { host = "10.0.0.31" }
members "etcd-2" { host = "10.0.0.32" }
members "etcd-3" { host = "10.0.0.33" }

ha {
  vip   = "10.0.0.10"
  iface = "eth0"
}

nodes {
  masters = ["10.0.0.11","10.0.0.12","10.0.0.13"]
  workers = ["10.0.0.21","10.0.0.22"]
  ssh {
    user     = "root"
    key_file = "/root/.ssh/id_rsa"
  }
}
```

`ko init` 启动后会：
1. 拉 etcd 3.5.21 tarball 到本地 `~/.ko/etcd/`（已 sha256 校验）。
2. 在 `~/.ko/etcd/pki/` 签发整套 mTLS：ca / server / peer / client。
3. scp tarball + 证书到每台 etcd 节点，写 systemd unit `etcd.service`，enable + start。
4. 部署 8 小时 systemd timer + 备份脚本（`ko-etcd-backup.timer` / `.service`）。
5. 等所有 member `/health` 返回 `{"health":"true"}`（最长 2 分钟超时）。
6. 把 ca + client cert 同步到每台 master 的 `/etc/etcd/pki/`。
7. `kubeadm init` 带上 `--etcd-servers=https://10.0.0.31:2379,... --etcd-cafile=... --etcd-certfile=... --etcd-keyfile=...`。

### 10.3 子命令

```bash
ko etcd install              # 重新跑一次安装（idempotent）
ko etcd status               # 看每台 member 的 systemctl + endpoint 健康
ko etcd backup               # 立即手动备份（8h 定时任务也跑）
ko etcd uninstall            # 停 systemd + 清 PKI 和 data
ko etcd uninstall --purge-backups   # 顺便删 /var/backups/etcd
```

dashboard 端点：

```
GET /api/etcd/status         # 每台 member active / health
GET /api/etcd/backups        # 所有备份文件列表（按 mtime desc）
```

### 10.4 备份恢复

自动备份 8 小时一次（systemd timer + Persistent=true 补错过点），
落在每台 member 的 `/var/backups/etcd/<host>-<ts>.db`，
本地保留 14 天滚动。手动 `ko etcd backup` 会把文件 scp 回 `ko` 所在机器当前目录。

**单 member 故障**（多数派还活）：直接重启那台 member 就能重新加入 quorum，
**不需要 restore**——只有全集群故障或数据污染才需要从 snapshot restore。

**从 snapshot restore 全集群**：

```bash
# v0.0.1+：ko cluster restore 自动按 member 顺序 stop / move aside / restore / start
ko cluster restore --snapshot ./ko-etcd-etcd-1-20260101-120000.db --config cluster.hcl

# 流程：
# 1. 对每个 member（按名字排序）：
#    a. systemctl stop etcd
#    b. 把 /var/lib/etcd/<member> 移到 .broken-<ts>
#    c. scp snapshot 到 /tmp
#    d. etcdctl snapshot restore --name=<member> --initial-cluster=<全部成员>
#       --initial-advertise-peer-urls=https://<host>:2380 --data-dir=/var/lib/etcd/<member>
#    e. systemctl start etcd
#    f. 等 /health=true（最多 30s）
# 2. 整个恢复过程中不要动 kube-apiserver，让 master 自然发现 etcd 恢复
```

手动单 member restore（老版本或 `ko cluster restore` 不可用时）：

```bash
ssh etcd-1
sudo systemctl stop etcd
sudo etcdctl snapshot restore /var/backups/etcd/etcd-1-20260101-120000.db \
  --name etcd-1 \
  --initial-cluster etcd-1=https://10.0.0.31:2380,... \
  --initial-advertise-peer-urls https://10.0.0.31:2380 \
  --data-dir /var/lib/etcd/etcd-1.new
sudo mv /var/lib/etcd/etcd-1 /var/lib/etcd/etcd-1.broken
sudo mv /var/lib/etcd/etcd-1.new /var/lib/etcd/etcd-1
sudo systemctl start etcd
```

### 10.5 证书轮换

证书默认 10 年有效期（`DefaultValidity`）。到期前 30 天 dashboard `/api/certs`
会显示 `etcd-server`、`etcd-peer` 的 `not_after`。轮换：

```bash
ko etcd install --regen-pki
```

（`--regen-pki` 标志将在 v0.1.x 提供；当前 v0.0.1 重跑 `ko etcd install` 会
保留 CA 仅刷新 server/peer/client，幂等。）

### 10.6 故障排查

| 现象 | 排查 |
|---|---|
| `ko etcd status` 显示 `inactive` | `ssh etcd-1 systemctl status etcd` 看日志 |
| `endpoint_health=unhealthy` 但 `active=active` | `/etc/etcd/pki/client.crt` 过期或 SAN 不对，`journalctl -u etcd` 看 TLS 错误 |
| `waitForEtcdHealthy` 2 分钟超时 | `curl -k --cacert ... https://10.0.0.31:2379/health` 单点探测 |
| 8h timer 没跑 | `systemctl list-timers ko-etcd-backup.timer` 看上次/下次触发 |
| 备份目录满 | `find /var/backups/etcd -mtime +14 -delete`（脚本会自己滚）|