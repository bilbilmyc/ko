# ko

Kubernetes 集群生命周期管理工具，对标 [sealos](https://github.com/labring/sealos)，**离线优先**。

> 当前状态：**v0.0.4 已发布**（[release](https://github.com/bilbilmyc/ko/releases/tag/v0.0.4)）。规格见 [`docs/SPEC.md`](docs/SPEC.md)，实施计划见 [`docs/PLAN.md`](docs/PLAN.md)。

## 特点

- **真离线（S17）**：bundle 含 containerd + kubeadm + k8s 镜像 + cilium 镜像 + registry 镜像本身；init 时 master-1 自举 in-cluster registry（`ko.local:5000`），所有 kubeadm / cilium / node join 都从本地仓库拉，**全程不访问公网**
- **多架构**：amd64 + arm64，一个 image index 自动选；bundle 内跨架构层做内容可寻址去重
- **双运行时**：上游 containerd v2 + docker 可切
- **CNI 双轨**：Cilium（默认，kube-proxy 替换）+ Flannel（兜底）
- **HA 自带**：kube-vip 自动管 VIP，多 master 集群开箱即用
- **证书 100 年**：内部 CA 自签，集群所有证书 100 年有效期
- **节点生命周期**：随时加 master / worker，drain + delete + reset
- **主机调优**：sysctl / 内核模块 / swap / systemd 一键应用，profile 化
- **Web Dashboard**：basic auth + token bucket 限流 + 审计日志，REST API 看集群 / 加节点 / 调优 / 查证书
- **Doctor 预检**：kernel / swap / runtime / 端口 / 镜像加速器
- **etcd 备份/恢复**：`ko cluster backup` 一键 snapshot；`ko cluster restore` 支持 stacked + external 两种模式

## 不做什么（明确边界）

- ❌ **App store / ClusterApp** — 不做
- ❌ 不内置 registry 镜像加速器配置（让用户自己配）
- ❌ 不替代 kubectl（kubectl 还是操作集群的主工具）
- ❌ 不做 Kubernetes 自身升级（v0.x 范围外）

## 安装

### 方式 1：下载预编译二进制

从 [Releases](https://github.com/bilbilmyc/ko/releases) 下载对应架构：

```bash
# Linux amd64
curl -sSL https://github.com/bilbilmyc/ko/releases/latest/download/ko-linux-amd64 -o ko
chmod +x ko && sudo mv ko /usr/local/bin/

# Linux arm64
curl -sSL https://github.com/bilbilmyc/ko/releases/latest/download/ko-linux-arm64 -o ko
chmod +x ko && sudo mv ko /usr/local/bin/
```

### 方式 2：从源码构建

需要 Go 1.22+：

```bash
git clone https://github.com/bilbilmyc/ko.git
cd ko
make build           # 当前架构
# 或
make build-all       # 同时构建 amd64 + arm64
```

产物落在 `bin/ko`（或 `bin/ko-linux-{amd64,arm64}`）。

## 快速开始

### 离线安装（推荐 / 默认）

> 项目主用场景：公司内部 + 真离线。bundle 含所有镜像 + 自举 in-cluster registry，**全程不访问公网**。
>
> **版本控制说明**：ko 工具走 GitHub release（`v0.0.x`），bundle 走公司内部存储（`bundle-k8s<X.Y.Z>-cilium<X.Y.Z>-<YYYYMMDD>-<arch>.oci.tar.gz`）。两者解耦，按需组合。

#### 运维视角（部署新集群）

```bash
# 1. 从公司内部 NFS 拿 bundle（所有集群节点挂载同一份 NFS）
ls /mnt/ko-store/
# bundle-k8s1.32.0-cilium1.16.1-20260702-amd64.oci.tar.gz
# bundle-k8s1.32.0-cilium1.16.1-20260702-multi.oci.tar.gz
cp /mnt/ko-store/bundle-k8s1.32.0-cilium1.16.1-20260702-multi.oci.tar.gz .

# 2. 从 GitHub release 拿 ko 二进制
curl -sSL https://github.com/bilbilmyc/ko/releases/download/v0.0.4/ko-linux-amd64 -o ko
chmod +x ko

# 3. 生成 cluster.hcl（sealos 风格：选 profile，写带文档注释的配置）
#    可选 profile: single | ha | external-etcd
./ko init --generate-config=ha -o cluster.hcl

# 4. 按需编辑 cluster.hcl（已自动 include 每个字段的注释）

# 5. 预检
./ko doctor --config cluster.hcl

# 6. 离线 init（master-1 会自举 ko.local:5000 镜像仓库；
#    kubeadm / cilium / node join 全部从本地仓库拉，全程不访问公网）
./ko init --config cluster.hcl --offline --bundle ./bundle-k8s1.32.0-cilium1.16.1-20260702-multi.oci.tar.gz
```

完整流程和故障排查见 [RUNBOOK §1 离线部署](docs/RUNBOOK.md#1-离线部署s17真离线--in-cluster-registry)。

#### 烤包员视角（烤新 bundle 推到 NFS）

```bash
# 1. 在能上网的机器上烤新 bundle（自动同时构建 amd64 + arm64）
#    bundle 含 containerd + kubeadm 二进制 + k8s 控制面镜像 + registry:2
#    仓库镜像本身 + cilium 全部镜像 + cilium helm chart
ko pack build --arch all --output ./dist --version bundle-k8s1.32.0-cilium1.16.1-20260702
# 产物：dist/bundle-k8s1.32.0-cilium1.16.1-20260702-multi.oci.tar.gz  (~826MB amd64 layer)

# 2. 推到公司内部 NFS（运维挂载路径）
scp dist/bundle-k8s1.32.0-cilium1.16.1-20260702-multi.oci.tar.gz ko-nfs:/mnt/ko-store/

# v0.0.5+：pack 完后本机 docker/nerdctl 中临时拉入的 k8s/cilium/registry:2
# 镜像会自动 rmi（best-effort），本机磁盘释放 5-10 GB；bundle 自身和
# ~/.ko/cache/<sha>.tar 不动。
```

### 在线安装（备选 / 不推荐，仅供测试）

```bash
# 1. 生成 cluster.hcl
ko init --generate-config=ha -o cluster.hcl

# 2. 预检
ko doctor --config cluster.hcl

# 3. 初始化（在线模式：所有镜像从公网拉）
ko init --config cluster.hcl
```

### HA 集群

把 `nodes.masters` 列 3 台或更多，`ko init` 会自动：
1. `kubeadm init` 第一台 master
2. 部署 kube-vip（绑定 VIP）
3. 签 100 年证书，`kubeadm join --control-plane` 其余 master
4. `kubeadm join` 所有 worker
5. 安装 Cilium（kubeProxyReplacement=strict）

### 加 / 删节点

```bash
# 加 worker
ko node add 10.0.0.23 --role worker --config cluster.hcl

# 加 master（自动签 control-plane 证书）
ko node add 10.0.0.14 --role master --config cluster.hcl

# 删节点（drain + delete + reset）
ko node remove 10.0.0.23 --config cluster.hcl

# 列节点
ko node list --config cluster.hcl
```

### Web Dashboard

```bash
KO_DASHBOARD_PASSWORD=secret ko dashboard --config cluster.hcl
# → http://127.0.0.1:8080 （basic auth，user=admin）
```

带前端 UI（自己 build 的 React 静态文件）：

```bash
ko dashboard --config cluster.hcl --static-dir ./web/dist
```

## 命令清单

```
ko version                         工具版本（git describe 注入）
ko arch                            当前二进制 arch
ko doctor [--config]               预检（kernel / swap / runtime / 端口）
ko init   --config [master]        初始化集群（在线 / 离线）
ko init   --generate-config=PROF   生成 starter 配置（sealos 风格，单机/HA/外部 etcd）
ko node   list|add|remove|label    节点生命周期
ko tune   apply|show|reset         主机调优（profile 化）
ko reset [--purge]                释放集群（--purge 额外清镜像缓存和 ko 配置文件）
ko cluster info|certs|backup|restore 集群操作（备份 / 恢复 etcd / 查证书）
ko etcd   install|status|backup|uninstall  外部 etcd 生命周期（S14）
ko pack   build|inspect            离线 OCI bundle（支持 --arch all）
ko dashboard                      Web Dashboard（basic auth + REST + rate limit + audit log）
ko completion <bash|zsh|fish>      shell 补全
```

## 项目结构

```
cmd/ko/                     入口（main）
internal/cli/               cobra 命令（一个文件一个命令族）
internal/cluster/           kubeadm / 节点生命周期 / 卸载 / flannel
internal/containerd/        containerd v2 安装
internal/docker/            docker CE 安装
internal/helm/              helm chart 安装
internal/image/             OCI bundle 打包 / inspect / upstream 下载
internal/dashboard/         HTTP server + basic auth + REST
internal/doctor/            预检
internal/tune/              sysctl / modules / swap / systemd
internal/exec/              SSH executor（避免 import cycle）
pkg/config/                 HCL 配置解析
docs/                       SPEC / PLAN / CHANGELOG
.github/workflows/          CI / release
```

## 文档

- [`docs/SPEC.md`](docs/SPEC.md) — 规格说明
- [`docs/PLAN.md`](docs/PLAN.md) — 实施计划
- [`docs/RUNBOOK.md`](docs/RUNBOOK.md) — 生产部署 runbook
- [`docs/CHANGELOG.md`](docs/CHANGELOG.md) — 变更日志

## 路线图

| 版本 | 状态 | 目标 |
|---|---|---|
| **v0.0.5** | 📋 Unreleased | **registry 二进制 + systemd 硬化**（`ko-registry.service`）+ **containerd/kubelet 服务配置调优**（`max_concurrent_downloads`、`timeout` 等） |
| v0.0.4 | ✅ 已发布（2026-07-02） | dedup 改按 blob 内容 sha256（更稳健） |
| v0.0.3 | ✅ 已发布（2026-07-02） | bundle dedup 防御性 patch（`dedupDockerArchive`；当前 CI 是 no-op） |
| v0.0.2 | ✅ 已发布（2026-07-02） | **真离线** — S17 自举 in-cluster registry；bundle 含 containerd/kubeadm/k8s-images/cilium-images |
| v0.0.1 | 📦 历史 release | 首个可用版（bundle 仅含 containerd） |
| v0.1.x | 📋 候选 | HA 外部 etcd / 切换到用户魔改 containerd / eBPF 自动检测 / SSO |
| v0.2+ | 待定 | 看 v0.0.x 反馈决定 |

**v0.0.5 重点**：S17 in-cluster registry 改用 static Go binary（`distribution/distribution` v2.8.3）运行，以 systemd 服务（`ko-registry.service`）托管，带资源限制（MemoryLimit=2G, CPUQuota=200%）和安全沙盒（User=nobody, ProtectSystem=strict, SystemCallFilter 等）。同时对 containerd 服务做配置调优（`max_concurrent_downloads=3`, `timeout=30s`, `disable_snapshot_annotations=false`），提升 offline airgap 场景稳定性。

## License

TBD