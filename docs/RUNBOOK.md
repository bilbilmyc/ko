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

## 1. 离线部署（主线 / 推荐）

> 项目主用场景：公司内部 + 真离线。bundle 含所有镜像 + 自举 in-cluster registry，**全程不访问公网**。所有 init / node add 默认走离线模式。

S17 之后，离线部署才真的"全离线"：bundle 里同时烤了 `containerd` / `kubeadm` 二进制 / `k8s 控制面` 镜像（apiserver / controller-manager / scheduler / proxy / coredns / pause / etcd）/ `cilium` 全部镜像 / `registry:2` 仓库镜像本身 / `cilium` helm chart。`ko init --offline` 会在 master-1 拉起一个 in-cluster Docker distribution registry，把这些镜像 re-tag 进去，然后通过 containerd mirror config 自动把所有 upstream 拉取重写到本地。

### 1.1 bundle 里到底有什么

```
dist/bundle-k8s1.32.0-cilium1.16.1-20260705-multi.oci.tar.gz
├─ index.json                          # 顶层：2 个 arch manifest
├─ oci-layout
└─ blobs/sha256/
   ├─ <amd64 manifest>
   │   ├─ containerd-<latest>-linux-amd64.tar.gz      ← pack 时 fetch latest stable（v0.0.5+）
   │   ├─ kubeadm-v1.32.0-linux-amd64.tar.gz          ← kubeadm 静态二进制
   │   ├─ k8s-1.32.0-amd64.tar                       ← kube-apiserver/controller-manager/
   │   │                                              scheduler/proxy/coredns/pause/etcd
   │   ├─ registry-2-linux-amd64.tar                 ← registry:2 镜像（备选，不再主用）
   │   ├─ registry-2.8.3-linux-amd64.tar.gz          ← static Go 二进制（主用）
   │   ├─ cilium-1.16.1-images.tar                   ← cilium/operator/hubble/...
   │   └─ cilium-1.16.1.tgz                          ← helm chart
   └─ <arm64 manifest>                                （chart/k8s-images/registry 跨架构去重）
```

每层都是独立 mediaType（`vnd.ko.layer.*`），operator 用 `ko pack inspect <bundle>` 看。

**v0.0.5+ containerd / docker 版本追踪**：

- `ko pack build` 不再写死 containerd 版本。默认调 GitHub API `repos/containerd/containerd/releases/latest` 拿 tag（如 `v2.0.5` / `v2.1.0`），24h cache 到 `~/.ko/cache/containerd-latest.txt`。GitHub 不可达退到 `v2.1.0` 而不是 fail
- docker CE 不在 GitHub release 走（apt 渠道），改为 `apt install docker-ce`（channel=stable 默认装最新），不再写死 `27.5.1`。HCL `docker.version` 仍可显式 pin
- bundle 一旦烤完，containerd 版本冻结（registry 二进制、kubeadm 静态二进制同理）。新版本要重烤

### 1.2 在能上网的机器上打包

```bash
ko pack build --arch all --output ./dist --version bundle-k8s1.32.0-cilium1.16.1-20260705
# 产物：dist/bundle-k8s1.32.0-cilium1.16.1-20260705-multi.oci.tar.gz  (amd64 bundle ~826M；含 containerd + kubeadm + k8s 控制面镜像 + registry 静态二进制 + cilium 全部镜像)
# 注：containerd 层文件名会反映 pack 当天的 latest tag（如 containerd-2.0.5-linux-amd64.tar.gz）
```

如果只想给一种架构烤：

```bash
ko pack build --arch amd64 --output ./dist --version bundle-k8s1.32.0-cilium1.16.1-20260705
```

烤好的 bundle 推到**公司内部存储**（HTTP / NFS / MinIO 等 — 待选型，见 PLAN §8.6），交付时与 ko 二进制一起给到目标机器。详细流程见 §2.8 交付物。

**v0.0.5+ pack 完本机镜像自动清理**：bundle 烤完后 `ImagePuller.Remove` 会把本机 docker/nerdctl 镜像存储里临时拉入的 k8s / cilium / `registry:2` 镜像 `rmi` 掉（best-effort，单张失败 `logger.Warn` 后继续），单次 pack 大约释放 5-10 GB。**`~/.ko/cache/<sha>.tar` 不动**（那是 bundle 层的真实输入，下次 pack 复用）。

### 1.3 cluster.hcl 配置

```bash
ko init --generate-config=ha -o cluster.hcl
# 可选 profile: single | ha | external-etcd
vim cluster.hcl   # 填节点 IP、SSH user、VIP、CNI 等
ko doctor --config cluster.hcl   # 红就修
```

关键字段：

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

### 1.4 拓扑（HA：3 master + N worker）

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

### 1.5 拷贝交付物到目标机器

> **版本控制说明**：ko 工具走 GitHub release（v0.0.x）；bundle 走公司内部存储（独立版本号 `bundle-k8s<X.Y.Z>-cilium<X.Y.Z>-<YYYYMMDD>-<arch>.oci.tar.gz`）。两者解耦，按需组合。

```bash
# 1. 从 NFS 拿 bundle
cp /mnt/ko-store/bundle-k8s1.32.0-cilium1.16.1-20260705-multi.oci.tar.gz .

# 2. 从 GitHub release 拿 ko 二进制（或本地 make build）
wget https://github.com/bilbilmyc/ko/releases/download/v0.0.5/ko-linux-amd64
chmod +x ko-linux-amd64

# 3. 打交付物 tar（含 ko + bundle + 三个 profile 模板；TODO: #65 完成后此命令可用）
./ko pack ship \
  --output ko-delivery-v0.0.5.tar.gz \
  --bundle ./bundle-k8s1.32.0-cilium1.16.1-20260705-multi.oci.tar.gz \
  --include-configs
# 产物：ko-delivery-v0.0.5.tar.gz
#   ├─ ko-linux-amd64              ← ko 二进制（来自 GitHub release v0.0.5）
#   ├─ bundle-k8s1.32.0-cilium1.16.1-20260705-multi.oci.tar.gz  ← 来自 NFS
#   ├─ cluster.hcl.single
#   ├─ cluster.hcl.ha
#   └─ cluster.hcl.external-etcd

# 4. 推到 master-1
scp ko-delivery-v0.0.5.tar.gz root@10.0.0.11:

# 5. 在 master-1 解 tar
ssh root@10.0.0.11 'mkdir ko-delivery && tar -xzf ko-delivery-v0.0.5.tar.gz -C ko-delivery'
```

### 1.6 离线 init

```bash
ssh root@10.0.0.11
./ko init --config cluster.hcl --offline --bundle ./bundle-k8s1.32.0-cilium1.16.1-20260705-multi.oci.tar.gz
```

`--offline` 让 ko 走 `OfflineRunner`（`internal/cluster/offline.go`），流程：

1. **scp + 解包**：bundle 传到 master-1 的 `/tmp/ko-bundle.oci.tar.gz`，解到 `/var/lib/ko/bundle/`
2. **按 mediaType 找各 layer**：从 `index.json` → manifest → `blobs/sha256/<digest>` 一路定位 `containerd` / `kubeadm` / `k8s-images` / `cilium-images` / `cilium-chart` / `registry-binary`
3. **装 runtime + kubeadm + containerd tune**：解 containerd 到 `/usr/local`，解 kubeadm 后 `install -m 0755 /usr/local/bin/kubeadm`，生成 `containerd config.toml` 并应用 tune（`max_concurrent_downloads`、`timeout`、`disable_snapshot_annotations`），`systemctl enable --now containerd`
4. **写 containerd mirror**：给 `quay.io` / `registry.k8s.io` / `docker.io` / `ghcr.io` 各加一个 `registry.mirrors."<host>"` 指向 `http://ko.local:5000`；`ko.local:5000` 自己标 `insecure_skip_verify = true`
5. **`ctr -n=k8s.io images import`** 拉 k8s-images / cilium-images 进 host containerd（registry-binary 不需要 import，是静态二进制）
6. **起 in-cluster registry**：从 bundle 解 `registry` 二进制到 `/usr/local/bin/registry`，以 `systemd` 服务运行（硬化的 `ko-registry.service`，带资源限制和安全沙盒），监听 `:5000`（host network）+ `:5001`（debug/prometheus），`curl /v2/` 轮询 30s 确认就绪
7. **retag + push**：每个镜像 `ctr -n=k8s.io images tag <upstream> ko.local:5000/<repo>` + `ctr -n=k8s.io images push --plain-http ko.local:5000 <target>`（并发 5 + 重试 3 + 间隔 2s）
8. **写 hosts**：解析 master-1 的 `ip -4 -o addr show`，把 `<master-1-IP> ko.local` 写到每个 master + worker 的 `/etc/hosts`（幂等：`grep -qF` 守卫）
9. **kubelet drop-in**：写 `/etc/systemd/system/kubelet.service.d/20-ko-offline.conf` 把 `KUBELET_KUBEADM_ARGS` 覆盖为 airgap image-pull-deadline=30m + registry QPS + eviction 阈值；`systemctl daemon-reload`
10. **清临时文件 + ctr cache**：pushImages 完成、`wait` 后给每个 push 过的 image 跑 `ctr images unset` + `ctr images rm`（source + target tag），再 `rm -f /tmp/ko-bundle.oci.tar.gz`。registry 已经是 source of truth，本地 ctr cache 是纯复制；master-1 disk 一般 <100G，必须让出来。`/var/lib/ko/bundle/` 保留（reset --purge 才删）

然后回到正常 init 流程：
- kubeadm init 时 `--image-repository=ko.local:5000`，所有控制面镜像从本地拉
- kubeadm join 时同样 `--image-repository=ko.local:5000`
- Cilium 走 helm install，image 通过 containerd mirror 自动 rewrite 到本地

整个 init 期间 master-1 不会向 `quay.io` / `registry.k8s.io` / `docker.io` / `ghcr.io` 发任何请求。

### 1.7 验证真的离线了

init 完后，到 master-1 上：

```bash
# 1. registry 起来了（HTTP，in-cluster only）
curl -sS http://ko.local:5000/v2/_catalog
# {"repositories":["cilium/cilium","cilium/certgen",...,"coredns/coredns",...]}

# 2. registry 运行在 systemd（查看资源限制 / 安全沙盒配置）
systemctl status ko-registry.service
# 可以看到 MemoryLimit=2G、CPUQuota=200%、User=65534（nobody）等

# 3. kubeadm 拉镜像从本地走（不应有公网 IP 出现）
crictl images | grep ko.local:5000
# ko.local:5000/coredns/coredns       v1.11.3
# ko.local:5000/etcd                  3.5.16-0
# ...

# 4. containerd 镜像配置确认（registry mirror 已生效）
containerd config dump | grep -A 2 "mirrors.*registry.k8s.io"
# endpoint = ["http://ko.local:5000"]

# 5. 每个节点的 /etc/hosts 都有 ko.local
for h in m1 m2 m3 w1 w2; do ssh $h "grep ko.local /etc/hosts"; done
# 10.0.0.11 ko.local

# 6. kubelet 配置 tune（可选验证：如果 init 前写了 tune，这里能看到）
cat /var/lib/kubelet/kubeadm-flags.env
# --container-runtime-endpoint=unix:///run/containerd/containerd.sock
# （其他 tune 项如 --eviction-hard 会体现在 kubelet 配置里）
```

### 1.8 HA 集群的 ko.local 解析（可选）

master-1 IP 写入 hosts 是早期 init 用的；HA 集群 kube-vip 绑定 VIP 后，可选地把所有节点的 `ko.local` 解析从 master-1 IP 改成 VIP，让 master-1 故障时其他 master 也能拉镜像：

```bash
# 把 m1 上的 10.0.0.11 ko.local 改成 <VIP> ko.local
ssh <VIP-node> 'sed -i "s/^.* ko.local$/<VIP> ko.local/" /etc/hosts'
for h in m2 m3 w1 w2; do ssh $h 'sed -i "s/^.* ko.local$/<VIP> ko.local/" /etc/hosts'; done
```

这一步是可选的——`ko.local:5000` 通过 master-1 IP 在 init 期间已经 work，VIP 切换只是给后续 `ko node add` / `ko cluster backup` 兜底。

### 1.9 100 年证书

ko 默认 `--certificate-validity=876000h`（100 年）。`ko cluster certs` 能看每个 master 上的证书剩余有效期：

```bash
ko cluster certs --config cluster.hcl
```

如果看到某些证书是 1 年有效期，说明那台 master 是 `kubeadm join` 后才签的（默认 join 不覆盖 cert 有效期，需要显式传 `--certificate-validity`）。重签：

```bash
# 在目标 master 上
sudo kubeadm init phase certs all --certificate-validity=876000h
```

### 1.10 增量更新 bundle

bundle 是不可变的（每个 bundle 一个 OCI digest）。新版本重打：

```bash
ko pack build --arch all --output ./dist --version bundle-k8s1.32.0-cilium1.16.1-20260705
```

bundle 跨架构会做内容可寻址去重（cilium chart / k8s-images / registry 二进制三个 layer 在 amd64+arm64 之间 sha256 完全相同），所以只升一个架构的 patch 时实际增量大都来自架构特定的 binary。

## 2. 在线部署（备选 / 不推荐）

> **本节仅作历史参考和测试用**。项目主用离线模式（§1），ko 团队内部不再优化此路径——所有镜像走公网 registry，无 SLA 保证，无版本锁定。

### 2.1 cluster.hcl 配置

同 §1.3。

### 2.2 init 流程

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

### 2.3 100 年证书

同 §1.9。

### 2.4 离线模式怎么回退到在线

如果某次离线 init 失败（bundle 损坏 / OCI 解包异常），可以直接重跑但**不**带 `--offline`：

```bash
# 在所有节点上 systemctl stop kubelet containerd
ssh m1 'kubeadm reset --force'
ssh m2 'kubeadm reset --force'
ssh m3 'kubeadm reset --force'

# 在线 init 一次（拉公网镜像）
ko init --config cluster.hcl
```

回退后集群里没 in-cluster registry，kubeadm 拉镜像直接走公网。**生产环境不建议走这条路径**。

## 3. 节点扩缩容

### 3.1 加 worker

新机器先确保 SSH 可达、kernel ≥ 5.4、swap 关闭。然后：

```bash
# 离线模式（项目主用）
ko node add 10.0.0.24 --role worker --config cluster.hcl --offline --bundle ./bundle-k8s1.32.0-cilium1.16.1-20260705-multi.oci.tar.gz

# 在线模式（不推荐）
ko node add 10.0.0.24 --role worker --config cluster.hcl
```

ko 会：
1. 把 bundle 推到新机器（仅离线模式）
2. 在新机器装 runtime（containerd / docker）
3. 在新机器装 kubeadm/kubelet（离线从 bundle 解，在线从 apt/yum 源）
4. `kubeadm join --worker` 到集群
5. kubelet 启动时所有 image 拉取走 containerd mirror → `ko.local:5000`
6. 等 Node Ready 后退出

### 3.2 加 master

```bash
# 离线模式（项目主用）
ko node add 10.0.0.14 --role master --config cluster.hcl --offline --bundle ./bundle-k8s1.32.0-cilium1.16.1-20260705-multi.oci.tar.gz

# 在线模式（不推荐）
ko node add 10.0.0.14 --role master --config cluster.hcl
```

ko 会：
1. 装 runtime + kubeadm（同 §3.1）
2. `kubeadm join --control-plane`（自动从 m1 拷 CA + 签 100 年证书）
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

**v0.0.5 离线清理**：删节点时 `resetScript` 在 host 上额外做：
- `sed -i '/[[:space:]]ko\.local$/d' /etc/hosts` — 剥掉 airgap 写入的 ko.local 行，防止 stale IP 解析到老 master-1
- `rm -f /etc/containerd/config.toml` — 清 containerd mirror → ko.local:5000 的配置
- `rm -rf /etc/systemd/system/kubelet.service.d` — 清 kubelet drop-in
- master-1 的话还会清 in-cluster registry（`systemctl disable ko-registry` / `rm /usr/local/bin/registry` / `rm -rf /var/lib/ko-registry` 等，见 §4.3 / Task #6）

下次 `ko node add` 同 host 时这些会被幂等重写，无需手工补。

### 3.4 节点打 label

```bash
ko node label 10.0.0.21 role=gpu --config cluster.hcl
ko node label 10.0.0.21 role- --config cluster.hcl  # 删 label
```

## 4. containerd + kubelet 服务配置调优（v0.0.5）

ko 在离线 init 期间会对 containerd 和 kubelet 服务做配置调优，以适应 offline airgap 场景。调优由 `OfflineRunner` 自动完成，无需手工干预。

### 4.1 containerd 调优项

containerd 的 config 由 `containerd.Render()` Go 模板整体生成（见 `internal/containerd/config.go`）。`OfflineRunner.configureContainerd()` 调用 `containerd.OfflineConfig()` 生成完整 config 字符串，写到 `/etc/containerd/config.toml` 并 `systemctl restart containerd`。

调优项（v0.0.5 起，online + offline 都用同一套）：

| 配置项 | 值 | 说明 |
|---|---|---|
| `max_concurrent_downloads` | 3 | 限制并发 pull，避免 offline registry 压力过大 |
| `timeout` | 30s | pull 超时放宽（默认较短，在 slow network 下可能误报） |
| `disable_snapshot_annotations` | false | 允许 snapshot annotations，帮助 debug / 审计 |
| `max_container_log_line_size` | -1 | 无限制（避免 k8s 组件日志被截断） |
| `registry.mirrors.<upstream>.endpoint` | `["http://ko.local:5000"]` | 每个 upstream registry（quay.io / registry.k8s.io / docker.io / ghcr.io）独立 mirror block，全部指向 in-cluster registry |
| `registry.configs."ko.local:5000".tls.insecure_skip_verify` | true | 本地 registry 无 TLS |

**验证**：

```bash
# 看生效的 config.toml
containerd config dump | grep -A 2 "max_concurrent_downloads"
# max_concurrent_downloads = 3

# 看 mirror 配置
containerd config dump | grep -A 2 "mirrors.*registry.k8s.io"
# endpoint = ["http://ko.local:5000"]
```

### 4.2 kubelet 调优项

v0.0.5 起，ko 写一个 systemd drop-in 把 `KUBELET_KUBEADM_ARGS` 覆盖掉，drop-in 落在 `/etc/systemd/system/kubelet.service.d/20-ko-offline.conf`（这是 kubeadm / systemd 推荐的位置；kubeadm init 时启 kubelet，drop-in 会被自动加载）。

实现：`internal/cluster/kubelet.go` 的 `KubeletDropIn()` 渲染 drop-in 内容，`WriteKubeletDropIn()` 把内容写到 host 上（heredoc + `systemctl daemon-reload`）。`OfflineRunner.Run()` 在 step 9 写完 /etc/hosts 之后给所有 master+worker 各写一份；online init 在 `installRuntime` 循环之后也给所有 host 写一份（idempotent — online + offline 共用同一份 drop-in）。

| 标志 | 值 | 说明 |
|---|---|---|
| `--image-pull-progress-deadline` | 30m | airgap 仓库推/拉慢，kubeadm 默认 1m 会让 cilium 这类大镜像直接 ImagePullBackOff |
| `--registry-qps` | 5 | kubelet → registry QPS 上限（kubelet 默认就是 5，写出来便于审计） |
| `--registry-burst` | 10 | burst 上限（同上） |
| `--eviction-hard` | `memory.available<100Mi,nodefs.available<10%` | 磁盘满 / 内存满时驱逐 pod，而不是 CrashLoopBackOff |

**为什么不用 `--kubelet-extra-args`**：那是 kubeadm init 时一次性传的 flag，不会写进任何持久文件，下次 reset 之后就没了。drop-in 由 systemd 管理，`ko reset` 的 `resetScript` 会 `rm -rf /etc/systemd/system/kubelet.service.d` 把它清掉，下次 `ko init --offline` 会重写。

**验证**：

```bash
# 看 drop-in 文件
cat /etc/systemd/system/kubelet.service.d/20-ko-offline.conf
# [Service]
# Environment="KUBELET_KUBEADM_ARGS=--image-pull-progress-deadline=30m --registry-qps=5 ..."

# 看最终生效的 kubelet 进程参数
ps aux | grep kubelet | grep -o 'image-pull-progress-deadline=[^ ]*'
# image-pull-progress-deadline=30m

# 看 kubeadm 写入的 flags（drop-in 是叠加的）
cat /var/lib/kubelet/kubeadm-flags.env
```

### 4.3 registry 服务调优（systemd）

registry 本身以 systemd 服务运行（`ko-registry.service`），硬化的 unit 文件包含：

| 配置项 | 值 | 说明 |
|---|---|---|
| `User` / `Group` | 65534 (nobody) | 非 root 运行 |
| `MemoryLimit` | 2G | 内存上限 |
| `CPUQuota` | 200% | CPU 上限 |
| `LimitNOFILE` | 65536 | 文件描述符 |
| `LimitNPROC` | 4096 | 进程数 |
| `NoNewPrivileges` | true | 安全 |
| `ProtectSystem` | strict | 防止写 /usr /boot /etc |
| `CapabilityBoundingSet` | `CAP_NET_BIND_SERVICE` | 最小权限 |

**验证**：

```bash
systemctl cat ko-registry.service
# 看到上面所有配置项

systemctl status ko-registry.service
# Active: active (running) since ...
```

### 4.4 手工覆盖

如需调整，默认值在 `OfflineRunner` 脚本里写死。要改，有两种方式：

1. **改 ko 源码**：在 `offline.go` 里调配置，重新编译。
2. **init 后手工改**：
   ```bash
   # containerd
   vim /etc/containerd/config.toml
   systemctl restart containerd

   # kubelet（drop-in 是叠加在 kubeadm-flags.env 之上的）
   vim /etc/systemd/system/kubelet.service.d/20-ko-offline.conf
   systemctl daemon-reload
   systemctl restart kubelet

   # registry
   vim /etc/systemd/system/ko-registry.service
   systemctl daemon-reload
   systemctl restart ko-registry
   ```

> **注意**：手工改不持久（下次 ko init 会被覆盖）。如需持久化，建议提 issue 到 ko 仓库，把调优点做成 HCL config 可配置。

## 4. 主机调优

```bash
ko tune apply production --config cluster.hcl   # 默认高规格
ko tune apply dev        --config cluster.hcl   # 开发机
ko tune apply minimal    --config cluster.hcl   # 兜底

# 看当前实际生效配置
ko tune show --config cluster.hcl

# 恢复（删 sysctl 配置 + 重新加载内核默认）
ko tune reset --config cluster.hcl
```

### 4.1 profile 内容

| profile     | sysctl 重点                                 | swap  | modules           |
|-------------|---------------------------------------------|-------|-------------------|
| production  | net.core.somaxconn=65535, fs.file-max 高    | off   | br_netfilter, overlay |
| dev         | 通用，宽松                                  | off   | br_netfilter      |
| minimal     | 不动 / 关闭 swap                            | off   | 不动              |

### 4.2 自定义

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
# v0.0.5+：ko cluster restore 自动跑 stacked restore 全流程
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

### 7.0 init 报 `--offline requires --bundle`

v0.0.5 起，init 启动时 upfront 校验 `--offline` 必须配 `--bundle`。报错信息：

```
--offline requires --bundle <path-to-oci-tar.gz>; see `ko pack build` to produce one
```

修法：先用 `ko pack build --arch amd64 --output ./dist` 在能上网的机器上烤 bundle，再传 bundle 到目标机器用 `ko init --offline --bundle ./bundle-*.oci.tar.gz`。如果是 `ko node add` 误加了 `--offline` 但 host 本身已经有 runtime，删掉 `--offline` 即可（online add 走 apt/dnf 路径，不需要 bundle）。

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
ko init --config cluster.hcl --offline --bundle ./bundle-k8s1.32.0-cilium1.16.1-20260705-multi.oci.tar.gz
```

`--bundle` 必须是 multi-arch bundle 或与目标机器 arch 匹配的 single-arch bundle。`ko pack inspect <bundle>` 看 bundle 的 arches。

### 7.5 Dashboard 401

basic auth 失败。三种情况：
- 没带 `Authorization: Basic` header — 用 `curl -u user:pass`
- 密码错了 — 重启时换环境变量
- 用了 `--user ops` 但 curl 还是 `admin` — 对一下 user

## 8. 升级

v0.0.5 不支持集群内升级。等 v0.1.x 走 `ko upgrade` 子命令。过渡方案：

```bash
ko reset --config cluster.hcl
ko init --config cluster.hcl --offline --bundle ./bundle-k8s1.32.0-cilium1.16.1-20260705-multi.oci.tar.gz
```

应用层数据如果有持久卷（PV），不会被 reset 影响。

## 9. 监控

ko v0.0.5 不内置监控。建议：

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
# v0.0.5+：ko cluster restore 自动按 member 顺序 stop / move aside / restore / start
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

（`--regen-pki` 标志将在 v0.1.x 提供；当前 v0.0.5 重跑 `ko etcd install` 会
保留 CA 仅刷新 server/peer/client，幂等。）

### 10.6 故障排查

| 现象 | 排查 |
|---|---|
| `ko etcd status` 显示 `inactive` | `ssh etcd-1 systemctl status etcd` 看日志 |
| `endpoint_health=unhealthy` 但 `active=active` | `/etc/etcd/pki/client.crt` 过期或 SAN 不对，`journalctl -u etcd` 看 TLS 错误 |
| `waitForEtcdHealthy` 2 分钟超时 | `curl -k --cacert ... https://10.0.0.31:2379/health` 单点探测 |
| 8h timer 没跑 | `systemctl list-timers ko-etcd-backup.timer` 看上次/下次触发 |
| 备份目录满 | `find /var/backups/etcd -mtime +14 -delete`（脚本会自己滚）|