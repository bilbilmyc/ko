# ko

Kubernetes 集群生命周期管理工具，对标 [sealos](https://github.com/labring/sealos)，**离线优先**。

> 当前状态：**v0.0.1 开发中**（首个可用版）。规格见 [`docs/SPEC.md`](docs/SPEC.md)，实施计划见 [`docs/PLAN.md`](docs/PLAN.md)。

## 特点

- **离线优先**：所有组件打成一个 OCI 大镜像包，断网环境也能装
- **多架构**：amd64 + arm64，一个 image index 自动选
- **双运行时**：上游 containerd v2 + docker 可切
- **CNI 双轨**：Cilium（默认，kube-proxy 替换）+ Flannel（兜底）
- **HA 自带**：kube-vip 自动管 VIP，多 master 集群开箱即用
- **证书 100 年**：内部 CA 自签，集群所有证书 100 年有效期
- **节点生命周期**：随时加 master / worker，drain + delete + reset
- **主机调优**：sysctl / 内核模块 / swap / systemd 一键应用，profile 化
- **Web Dashboard**：basic auth，REST API 看集群 / 加节点 / 调优 / 查证书
- **Doctor 预检**：kernel / swap / runtime / 端口 / 镜像加速器
- **etcd 备份**：`ko cluster backup` 一键 snapshot

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

### 在线安装（默认）

```bash
# 1. 生成一份 cluster.hcl（sealos 风格：选 profile，写带文档注释的配置）
#    可选 profile: single | ha | external-etcd
ko init --generate-config=ha -o cluster.hcl

# 2. 按需编辑 cluster.hcl（已自动 include 每个字段的注释）

# 3. 预检
ko doctor --config cluster.hcl

# 4. 初始化（先 init 第一个 master，后续会自动 join 其余 master 和 worker）
ko init --config cluster.hcl
```

### 离线安装

```bash
# 1. 在能上网的机器上打包（自动同时构建 amd64 + arm64）
ko pack build --arch all --output ./dist --version v0.0.1
# 产物：dist/ko-v0.0.1-multi.oci.tar.gz

# 2. 把 ko 二进制 + bundle 拷到目标机器后离线 init
ko init --config cluster.hcl --offline --bundle ./ko-v0.0.1-multi.oci.tar.gz
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
ko dashboard                      Web Dashboard（basic auth + REST）
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

| 版本 | 目标 |
|---|---|
| v0.0.1 | 首个可用版：init / node / tune / reset / dashboard + 离线大镜像包 + amd64+arm64 |
| v0.1.x | HA 外部 etcd / 切换到用户魔改 containerd / eBPF 自动检测 / SSO |
| v0.2+ | 看 v0.0.1 反馈决定 |

## License

TBD