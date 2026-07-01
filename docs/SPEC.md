# Spec: `ko` — Kubernetes 集群生命周期管理工具

> Status: **FROZEN v0.0.1** · 2026-07-01 · 进入 Phase 2 (Plan)

---

## 1. Objective

构建一个对标 sealos 的 Kubernetes 集群管理工具 `ko`，提供：

1. **离线优先** 的 K8s 集群安装（air-gapped 镜像包 + 完整 manifest，**所有组件打入同一个 OCI 大镜像**）
2. **节点生命周期管理**（添加 / 删除 master & worker）
3. **主机配置调优**（sysctl、systemd、磁盘、内核模块、cgroup 等一键优化）
4. **Web Dashboard**（覆盖：集群安装、节点管理、主机调优、**journald 实时日志**、**集群释放**），**带 basic auth 鉴权**
5. **registry 镜像仓库**默认配置（sealos 风格：默认带阿里云 / 公开镜像源，containerd + docker 都生效）
6. **CNI 双轨**：Cilium（默认，kube-proxy 替换）+ Flannel（兜底，老机器 / 内核不支持 eBPF 时按节点选）
7. **Helm** 作为 cluster bootstrap 组件（Cilium / kube-vip / 后续 ingress 等）的安装器

**当前阶段聚焦**（用户确认）：

- 优先级 = **离线部署**（air-gapped 场景是主战场）
- v0.0.1 runtime 走 **upstream containerd v2 latest + docker**（从官方 release 下载）
- 用户的魔改 containerd 留到 v0.1.x：v0.0.1 只跑通 upstream 路径，验证架构可承载切换

**非目标**（明确不做，避免 scope 蔓延）：

- 不做**应用商店** / ClusterApp / 一键 helm 部署应用（**用户确认不做**）
- 不做集群监控 / 日志聚合 / 集群迁移
- 不嵌入 K8s Dashboard
- 不支持多集群联邦
- 不做 Windows / macOS 节点
- v0.0.1 不做 Calico（v0.0.x 永不做，按用户决定：只用 Cilium + Flannel）
- 不做 plugin 机制（CNI / 运行时写死选项）
- **v0.0.1 不做用户自带的魔改 containerd 集成**（先用 upstream 容器，v0.1.x 再切到 vendored 魔改版）

**用户画像**：

- 中小团队的 SRE / DevOps，需要在公司内网或工厂内快速交付多套 K8s 集群
- 对国产化 OS / 信创 / arm64 服务器场景有需求
- 容器运行时：用户自带的**魔改 containerd**（必选）+ 官方 Docker（可选）

**成功的样子**：

```bash
# 三步交付一套生产可用 K8s 集群
ko init --config cluster.yaml
ko node add master --hosts 10.0.0.10,10.0.0.11
ko node add worker --hosts 10.0.0.20..10.0.0.25
```

**架构支持**：

- v0.0.1 起 **amd64 + arm64** 并行支持
- 离线包按架构分两个 tag（`ko:v0.0.1-amd64` / `ko:v0.0.1-arm64`），OCI manifest list 顶层指向
- 节点识别到自己的 arch 后自动拉对应包

---

## 2. Tech Stack

| 层 | 选型 | 理由 |
|---|---|---|
| CLI 主体 | Go 1.22+ | 跟 k8s 生态（client-go、kubespray、kubeadm）完全复用 |
| SSH 客户端 | `golang.org/x/crypto/ssh` + `mitchellh/go-homedir` | 不引入 `ssh` 二进制依赖 |
| K8s 客户端 | `k8s.io/client-go` | 节点增删、证书签发都走这个 |
| 配置渲染 | HashiCorp `hcl/v2` + YAML | 集群描述用 HCL，集群内部资源用 YAML |
| 资源打包 | OCI Image（自定义 mediaType） | 镜像包复用 containerd 拉取链路，无需重新发明 |
| 前端 | React 18 + TypeScript + Vite | 主流、类型安全、构建快 |
| 前端 UI | Ant Design 5.x | 表格、表单、卡片开箱即用，运维场景颜值不差 |
| 状态管理 | TanStack Query + Zustand | 服务端状态 / 客户端状态分离 |
| 后端 API | Go（同一进程，HTTP on `:8080`） | 单二进制分发 |
| WebSocket | `gorilla/websocket` | 节点安装进度实时推送 |
| 进程内总线 | 简单 channel + sync.Map | 不引入消息队列 |
| 测试 | `testing` + `testify` + `envtest` | k8s 生态标准 |
| 跨平台 | `GOARCH=amd64` / `arm64` 双架构编译 | 信创 / arm 服务器需求 |
| Chart 安装器 | Helm 3 SDK（`helm.sh/helm/v3`） | Cilium / kube-vip / 后续 ingress 等组件都走 Helm install |
| 远程日志 | `journalctl` over SSH + WebSocket 转推 | 实时推送到 Web Dashboard |
| Dashboard 鉴权 | basic auth（`crypto/subtle` 做常量时间比较） | 单进程简单方案；v0.1.x 可换 token / OIDC |
| 二进制分发（v0.0.1 upstream containerd） | `ko pack` 从 GitHub release 拉 `containerd-*-linux-<arch>.tar.gz` | 构建期一次性联网，运行期完全离线 |

**运行时支持**：

| 运行时 | 安装方式 | 配置方式 |
|---|---|---|
| **containerd**（魔改版，必装） | 从 vendored 离线包直接拷贝二进制 | 生成 `/etc/containerd/config.toml`，systemd unit 托管 |
| **docker**（可选） | OS 官方源安装 `docker-ce`（在线）/ 离线包内 deb/rpm 预下载（离线） | 生成 `/etc/docker/daemon.json`，配置 cgroup driver、registry mirror |

**containerd 集成方式**：

- v0.0.1 走 **upstream**：构建时 `ko pack` 从 `github.com/containerd/containerd/releases` 下载官方二进制，烤进离线大镜像包
- v0.1.x 走 **vendor**：用户把魔改 containerd 放到 `vendor/containerd/<arch>/containerd`；`ko pack` 检测到就用 vendor，没检测到 fallback 到 upstream
- 工具本身**不**重新编译 containerd
- 工具做的是：把 containerd 二进制塞进离线包、生成对应的 systemd unit、写 `/etc/containerd/config.toml`

**离线大镜像包（sealos 风格）**：

- 一个 OCI image，**全部**组件打进去：
  - k8s control plane / node 二进制
  - 魔改 containerd 二进制（每个 arch 各一份；缺失则 `ko pack --arch=xxx` 报错退出）
  - docker 二进制（离线安装用）+ dockerd-rootless 依赖
  - CNI 插件（cilium / flannel）
  - pause 镜像
  - 所有 k8s 镜像（coredns、cni 等；**不包含 kube-proxy 镜像**，因走 Cilium 替换）
  - helm / cilium cli / kube-vip 镜像等辅助工具
  - 集群 manifest（一个描述本包内容的 `manifest.yaml`，记录版本、sha256、组件清单）
- mediaType: `application/vnd.ko.bundle.v1`（自定义）
- 顶层走 OCI manifest list，按 arch 分发

---

## 3. Commands

### 3.1 CLI（用户视角）

```bash
# 工具自身
ko version                                    # 输出版本、commit、containerd 版本
ko doctor [--config cluster.yaml]             # 预检：SSH 连通性、OS、内核（eBPF?）、磁盘、containerd 缺失项
ko arch                                       # 打印当前二进制 arch (amd64/arm64)
ko completion <bash|zsh|fish>                 # 补全脚本

# 集群生命周期
ko init --config cluster.yaml [--offline] [--runtime=containerd|docker]
ko join --config cluster.yaml                 # 其它节点加入（kubeadm 流程）
ko reset [--purge] [--force]                  # 销毁集群：drain → delete node → reset kubeadm → 清理 containerd / docker / 网络 / VIP

# 节点管理
ko node add master|worker --hosts <list> [--config cluster.yaml] [--cni=cilium|flannel]
ko node remove <node-name> [--force]          # 驱逐 workload → drain → delete → reset
ko node list                                  # 列出所有节点 + 状态
ko node label <name> key=val [--remove]       # 节点标签维护

# 主机调优
ko tune apply [--config cluster.yaml]         # 应用调优模板
ko tune show [<host>]                         # 展示当前主机配置（差异对比）
ko tune reset                                 # 还原默认

# 离线包制作（开发者/发布者）
ko pack [--arch amd64,arm64] [--version 1.35|1.36]
ko pack push --to registry.example.com/ko:v0.0.1
ko pack inspect ko:v0.0.1                     # 查看包内组件、sha256、版本

# 集群操作（运行中集群）
ko cluster info                               # 集群 ID / endpoint / 版本 / 节点数
ko cluster certs                              # 证书到期时间一览
ko cluster backup                             # etcd snapshot（v0.0.x 仅 stacked 模式）

# Web Dashboard
ko dashboard [--addr :8080]                   # 启动内置 Web UI（默认 :8080）
# 内置面板：Install / Nodes / Tune / Logs (journald 实时) / Release (reset) / Settings
```

### 3.2 开发者命令

```bash
make build                                    # 编译 ko 单二进制到 bin/ko
make test                                     # 跑单元测试
make test-e2e                                 # e2e（依赖 kind）
make lint                                     # golangci-lint + 前端 eslint
make web-dev                                  # 启动前端 dev server
make web-build                                # 前端产出嵌入到 Go 嵌入资源
make pack                                     # 打包离线镜像包到 dist/ko-<version>.tar.gz
make clean
```

### 3.3 集群配置文件骨架

```hcl
# cluster.hcl
cluster {
  name     = "prod-shanghai"
  version  = "1.35"                          // 1.35 | 1.36
  cidr     = "10.244.0.0/16"                 // pod CIDR
  svc_cidr = "10.96.0.0/12"
}

image {
  registry   = "registry.cn-hangzhou.aliyuncs.com"  // 镜像仓库（在线模式）
  repository = "ko"
  tag        = "v0.0.1"
  // 离线模式下：打包路径，由 ko pack 阶段写入
  // offline_pack = "ko-1.35-v0.0.1.tar.gz"

  // 镜像仓库 mirror（sealos 风格：默认有，containerd + docker 都生效）
  registry_mirrors = [
    "https://docker.m.daocloud.io",
    "https://dockerproxy.com",
    "https://docker.mirrors.ustc.edu.cn",
  ]
  // 私有 insecure registry（如内网 Harbor）
  // insecure_registries = ["harbor.example.com"]
}

containerd {
  // v0.0.1 锁定 upstream；v0.1.x 切到 vendor（用户魔改版）
  source  = "upstream"                       // upstream | vendor
  version = "v2.x.x"                        // upstream 模式下：从 GitHub release 拉

  // upstream 模式：`ko pack` 自动从官方 release 下载并烤入离线包
  //   https://github.com/containerd/containerd/releases/download/<v>/containerd-<v>-linux-<arch>.tar.gz
  // vendor 模式（v0.1.x 启用）：从仓库内 vendor/containerd/<arch>/containerd 取
  //   vendor/containerd/amd64/containerd
  //   vendor/containerd/arm64/containerd
  //   缺失时 `ko pack` 报错退出

  // containerd 配置（所有节点共用一份）
  // upstream 模式：用 `vendor/containerd/config.toml` 仓库默认值
  // vendor 模式：同上；v0.1.x 起用户可改
  // 运行时写到 /etc/containerd/config.toml
}

runtime {
  // 集群级默认运行时：containerd（推荐，配套魔改版）或 docker
  default = "containerd"
  // 节点级可覆盖：nodes[].runtime
  docker = {
    // 仅在 default = "docker" 或节点选 docker 时生效
    version          = "27.x"               // 安装的 docker-ce 版本
    cgroup_driver    = "systemd"
  }
}

etcd {
  mode = "stacked"                           // stacked | external
  // external 模式下：
  // endpoints = ["https://10.0.0.5:2379", ...]
}

certificates {
  // 集群所有证书有效期（用户要求）：100 年
  // 实现：ko init 启动时自建内部 CA，签发 100 年证书，kubeadm init 用 --rootfs 注入
  validity = "876000h"                        // 100 * 365 * 24 = 876000h
  // 证书列表：
  //   - apiserver / apiserver-kubelet-client / front-proxy-ca / etcd-ca / etcd/server,peer ...
  //   - service-account signing key
  //   - 所有节点 client / server cert
  // `ko cluster certs` 列出剩余有效期（100 年起步 → 100y0d 形式）
}

ha {
  vip      = "10.0.0.100"                    // kube-vip 虚拟 IP
  iface    = "eth0"
  // kube-vip 走 DaemonSet 模式，跟随 :latest
  kube_vip_image = "ghcr.io/kube-vip/kube-vip:latest"
}

cni {
  // 集群级默认：cilium（kube-proxy 替换）
  plugin = "cilium"                          // cilium | flannel
  cilium = {
    // 用 :latest 镜像版本（你确认）
    kube_proxy_replacement = "strict"        // strict 需内核 4.19+ 且开启相关 eBPF
  }
  flannel = {
    // 老机器 / 内核不支持 eBPF 时按节点选 flannel
    backend = "vxlan"                        // vxlan | host-gw
  }
}

nodes {
  masters = ["10.0.0.5", "10.0.0.6", "10.0.0.7"]
  workers = ["10.0.0.20", "10.0.0.21", "10.0.0.22", "10.0.0.23"]

  // 可选：节点级覆盖 runtime / arch / cni（不写则用 cluster 级）
  // overrides = [
  //   { host = "10.0.0.20", runtime = "docker" },
  //   { host = "10.0.0.21", arch = "arm64" },
  //   { host = "10.0.0.22", cni = "flannel" },   // 这台机器内核老，跑不了 cilium
  // ]

  ssh = {
    user     = "root"
    port     = 22
    key_file = "~/.ssh/id_rsa"
  }
}

tune {
  profile         = "production"             // production | development | minimal
  swap_off        = true
  kernel_modules  = ["br_netfilter", "ip_vs", "ip_vs_rr", "overlay"]
  sysctl = {
    "net.ipv4.ip_forward"                 = "1"
    "net.bridge.bridge-nf-call-iptables"  = "1"
    "net.bridge.bridge-nf-call-ip6tables" = "1"
    "vm.swappiness"                       = "0"
    "fs.file-max"                         = "2097152"
  }
  systemd = {
    LimitNOFILE = 65536
    LimitNPROC  = 65536
  }
}

dashboard {
  // 监听地址，默认仅本机（推荐 SSH 隧道访问）
  // 设为 0.0.0.0 时必须配置 basic_auth（启动校验）
  listen = "127.0.0.1:8080"

  // basic auth（开启监听 0.0.0.0 时必填）
  // 不填则启动时自动生成随机密码，打印到 stderr（一次性的、强制用户记下）
  basic_auth = {
    user     = "admin"
    password = ""                            // 留空 = 启动时随机生成并打印
  }

  // 是否允许跨域（生产建议 false，调试前端时可临时开）
  allow_origin = ""
}
```

---

## 4. Project Structure

```
ko/
├── cmd/
│   ├── ko/                          # 主入口
│   │   └── main.go
│   └── koctl/                       # 可选：拆出独立工具（v2 考虑）
├── internal/
│   ├── cli/                         # cobra 命令
│   │   ├── root.go
│   │   ├── init.go
│   │   ├── join.go
│   │   ├── reset.go                 # 释放/销毁集群
│   │   ├── node.go
│   │   ├── tune.go
│   │   ├── cluster.go               # info / certs / backup
│   │   ├── pack.go                  # 离线大镜像包制作
│   │   ├── dashboard.go
│   │   └── doctor.go
│   ├── cluster/                     # 集群编排核心
│   │   ├── config.go                # 解析 cluster.hcl
│   │   ├── executor.go              # 抽象：local / ssh
│   │   ├── ssh_exec.go
│   │   ├── kubeadm.go               # kubeadm init/join 包装
│   │   ├── certs.go                 # 证书生成、scp、分发
│   │   ├── cilium.go                # 通过 helm 装 cilium
│   │   ├── cilium_kpfree.go         # kube-proxy 替换模式配置（清理 kube-proxy、关闭 IPVS/iptables）
│   │   ├── flannel.go               # 通过 helm 装 flannel
│   │   ├── kube_vip.go              # 通过 helm 装 kube-vip
│   │   ├── node_lifecycle.go        # add/remove 业务逻辑
│   │   ├── teardown.go              # reset 流程
│   │   └── journald.go              # journalctl 拉取（Web Dashboard 用）
│   ├── helm/                        # Helm SDK 封装
│   │   ├── install.go               # 离线 chart 安装
│   │   ├── pull.go                  # 离线模式：tgz 解包后渲染
│   │   └── values.go                # 渲染 values override
│   ├── containerd/                  # 容器运行时（魔改版）
│   │   ├── install.go               # 拷贝二进制、写 unit、生成 config.toml
│   │   ├── config.go                # CRI / sandbox image / registry mirror
│   │   └── images.go                # 镜像预加载
│   ├── docker/                      # Docker 运行时（可选）
│   │   ├── install.go               # 在线/离线安装 docker-ce
│   │   ├── daemon.go                # 生成 /etc/docker/daemon.json
│   │   └── images.go                # 镜像 load 到 docker
│   ├── image/                       # OCI 镜像包
│   │   ├── builder.go               # ko pack 流程
│   │   ├── puller.go                # 运行时拉取（在线模式）
│   │   ├── manifest_list.go         # 跨 arch 分发
│   │   └── media.go                 # 自定义 mediaType: application/vnd.ko.bundle.v1
│   ├── tune/                        # 主机调优
│   │   ├── sysctl.go
│   │   ├── modules.go
│   │   ├── swap.go
│   │   ├── systemd.go
│   │   ├── disk.go
│   │   └── profiles.go              // production/development/minimal
│   ├── dashboard/                   # 内置 Web
│   │   ├── server.go                # HTTP server
│   │   ├── auth.go                  # basic auth middleware
│   │   ├── api/                     # REST handlers
│   │   │   ├── cluster.go           # init / status / reset
│   │   │   ├── node.go              # list / add / remove
│   │   │   ├── tune.go              # apply / show
│   │   │   ├── logs.go              # journald 历史/回放
│   │   │   └── pack.go              # 离线包 inspect
│   │   ├── ws/                      # WebSocket
│   │   │   ├── progress.go          # 任务进度
│   │   │   └── logs.go              # journald 实时 tail
│   │   └── static/                  # embed.FS 嵌入前端
│   ├── logger/
│   └── version/
├── web/                             # 前端
│   ├── src/
│   │   ├── pages/
│   │   │   ├── Install/
│   │   │   ├── Nodes/
│   │   │   ├── Tune/
│   │   │   ├── Logs/                # journald 实时/历史
│   │   │   ├── Release/             # 集群释放确认页
│   │   │   └── Settings/            # 集群信息 / 证书到期 / 包信息
│   │   ├── components/
│   │   ├── api/                     # fetch 封装
│   │   └── main.tsx
│   ├── package.json
│   └── vite.config.ts
├── pkg/                             # 可被外部 import 的稳定 API
│   ├── config/                      # HCL schema
│   └── types/
├── vendor/
│   ├── containerd/
│   │   ├── config.toml              # 所有节点共用的 containerd 配置（v0.0.1 起就有）
│   │   ├── amd64/                   # v0.1.x 起放用户魔改二进制（v0.0.1 留空）
│   │   │   └── (placeholder)        #   缺失时 `ko pack` fallback 到 upstream
│   │   └── arm64/
│   │       └── (placeholder)
│   ├── docker/                      # 预下载的 docker deb/rpm（离线用，已确认包含）
│   │   ├── amd64/
│   │   └── arm64/
│   ├── helm/                        # 离线用 helm charts
│   │   ├── cilium-<ver>.tgz
│   │   └── kube-vip-<ver>.tgz
│   └── images/                      # 离线镜像列表（打包时 ctr pull → tar）
├── docs/
│   ├── SPEC.md                      # 本文件
│   ├── ARCHITECTURE.md              # 架构图
│   ├── RUNBOOK.md                   # 运维手册
│   └── CHANGELOG.md
├── test/
│   ├── e2e/                         # kind + ko 端到端
│   ├── integration/                 # 用 envtest 跑节点增删
│   └── fixtures/
├── scripts/
│   ├── pack.sh                      # 本地打包
│   └── ci.sh
├── Makefile
├── go.mod
└── README.md
```

---

## 5. Code Style

### 5.1 Go 风格

- 包名小写单数（`cluster` 不是 `clusters`）
- 错误处理：error 用 `fmt.Errorf("xxx: %w", err)` 包装
- 日志：使用 `log/slog`，不引入第三方 logger
- context 透传：所有长时间操作第一参数是 `ctx context.Context`
- 接口隔离：小接口（≤ 3 方法），按消费者侧定义（`internal/cluster/executor.go`）

```go
// internal/cluster/executor.go
type Executor interface {
    Run(ctx context.Context, host string, cmd string) (stdout, stderr []byte, err error)
    Scp(ctx context.Context, host, src, dst string) error
}
```

### 5.2 配置风格

- 集群描述用 HCL（嵌套结构 + 注释友好）
- K8s manifest 仍用 YAML（生态通用）

### 5.3 错误信息

```go
// 好
return fmt.Errorf("ssh to %s: handshake: %w", host, err)

// 不好
return errors.New("ssh failed")
```

### 5.4 前端风格

- 函数组件 + hooks
- 严格 TS（`strict: true`，`noUncheckedIndexedAccess: true`）
- 不用 CSS-in-JS，用 CSS Modules + Ant Design tokens
- 路径别名 `@/` 指向 `src/`

---

## 6. Testing Strategy

| 层级 | 范围 | 工具 | 目标覆盖率 |
|---|---|---|---|
| 单元 | internal/* 纯函数 | `testing` + `testify` | ≥ 70% |
| 集成 | SSH / containerd / kubeadm 流程 | `dockertest` 跑容器模拟节点 | ≥ 50% |
| e2e | 全链路 | `kind` + 真实 kubeadm | 关键路径必须有 |

**测试位置约定**：

- 单元测试与被测代码同包：`foo.go` 旁边有 `foo_test.go`
- 集成测试放 `test/integration/`，build tag `//go:build integration`
- e2e 放 `test/e2e/`，build tag `//go:build e2e`

**e2e 关键路径**（每条对应一个 case）：

1. `ko init` 单 master 成功
2. `ko init` HA stacked 3 master 成功
3. `ko node add worker` 在线/离线各一条
4. `ko node remove` 成功排空
5. `ko tune apply production` 后内核参数生效
6. 离线包在断网容器中能完整安装
7. **amd64 + arm64 各跑通 1-6**（CI 用 qemu-aarch64 模拟器；arm 物理机由用户/团队 smoke）
8. **upstream containerd v2** 路径完整 e2e（v0.0.1 主线）
9. **vendor containerd 路径** 留 placeholder，v0.1.x 切
10. **Cilium kube-proxy 替换** 跑通
11. **Flannel 兜底** 跑通（某节点 `nodes[].cni = "flannel"`）
12. **Dashboard 鉴权**：未授权 401、basic auth 通过正常访问
13. `ko reset` 干净回滚（重复 init 3 次不出脏数据）

**不写的测试**：

- containerd 内部行为（由 upstream / 魔改方负责）
- Kubernetes 自身（依赖社区测试）
- 用户魔改 containerd 集成（v0.0.1 不做；v0.1.x 切）

---

## 7. Boundaries

### Always do

- 跑 `make test` 和 `make lint` 后再 commit
- 修改 `cluster.hcl` schema 同步更新 `docs/SPEC.md`
- 新增 command 必须有 `--help` 输出 + e2e 一条
- 所有 SSH/网络操作带超时（默认 30s，可配置）
- 任何破坏性操作（`reset`、`node remove --force`）必须二次确认（CLI `--yes` 跳过，Web 弹窗确认）
- **Dashboard 任何 API 路径都要走 basic auth middleware**（含 WebSocket 握手）
- **不在仓库提交构建产物**（`dist/`、`bin/`、`node_modules/`、镜像 tarball）

### Ask first

- 添加新依赖（`go get` 任何东西）
- 改 K8s 支持版本矩阵
- 改 OCI 媒体类型
- 改 HCL schema 字段（破坏性）
- 改 containerd source 模式（`upstream` → `vendor` 切换是用户后续动作）
- 改 Web Dashboard 端口 / 鉴权方案
- 改默认 registry mirror 列表

### Never do

- 不直接 `rm -rf /` 之类破坏性命令
- 不在 v0.0.1 引入用户魔改 containerd 的代码路径（v0.1.x 才切）
- 不在仓库内打包 k8s 镜像（镜像包构建产物在 `dist/`，gitignore）
- 不把 `~/.kube/config`、证书私钥、私有 IP、Dashboard 密码写进仓库或日志
- 不跳过 e2e 就合入 main
- 不在 v0.0.1 加任何"应用商店 / ClusterApp / 一键 helm 部署应用"相关的代码（永不做）

---

## 8. Success Criteria

**v0.0.1（首个可用版）**：

- [ ] `ko init` 在 3 master + 3 worker 拓扑上 5 分钟内完成
- [ ] `ko init` 生成的集群 `kubectl get nodes` 全 Ready
- [ ] 运行时可切：`--runtime=containerd`（upstream v2 latest）和 `--runtime=docker` 两条路径都 e2e 通过
- [ ] **amd64 + arm64 各跑通一次 e2e**（在 arm 物理机或 qemu-aarch64 模拟器中）
- [ ] **arm64 走 upstream 路径**：`ko pack --arch=arm64` 从 GitHub release 拉 arm64 版 containerd；`vendor/containerd/arm64/` 留空也 OK
- [ ] `ko node add worker` 一次添加 ≥ 5 个 worker 节点成功
- [ ] `ko node remove <name>` 工作负载被驱逐、节点从集群消失、节点上 kubelet/容器被清理
- [ ] `ko tune apply production` 后 `sysctl net.ipv4.ip_forward == 1` 等关键参数生效
- [ ] 离线包在断网环境能完成全部上述操作（amd64 + arm64 各一次）
- [ ] 离线大镜像包**包含 docker-ce deb/rpm**（每个 arch +200MB 已确认）
- [ ] `ko reset` 干净回滚：drain → delete node → kubeadm reset → 清理 containerd / docker / 网络 / VIP
- [ ] **containerd 走 upstream v2 latest**：`ko pack --arch=amd64` 从 GitHub release 下载 containerd 二进制并烤入；运行时 `/usr/local/bin/containerd --version` 是 v2.x.x
- [ ] **Dashboard 鉴权**：未带 Authorization 头的请求返回 401；basic auth 通过后可访问全部面板
- [ ] **Dashboard 随机密码**：basic_auth.password 留空时 `ko dashboard` 启动时打印一次性的随机密码到 stderr（用户必须记录）
- [ ] **Dashboard 默认仅本机**：`dashboard.listen = "127.0.0.1:8080"` 是默认值，绑定 0.0.0.0 强制要求配置密码
- [ ] **离线构建链路**：`ko pack` 在有网环境运行；产出的 `ko:v0.0.1-amd64` OCI image 在完全断网环境能装集群
- [ ] Web Dashboard `ko dashboard` 启动后能在浏览器完成 install / node add / tune / release
- [ ] Web Dashboard **Logs 页面**：实时显示指定节点的 `journalctl -u kubelet -f` 等输出
- [ ] `ko pack` 产出的 OCI image 用 `crane manifest` 能看到 manifest list，amd64/arm64 两个子清单都在
- [ ] **默认 registry mirror** 配置生效：containerd `config.toml` 和 docker `daemon.json` 都带 mirror
- [ ] **Helm 装 Cilium**：`helm list -n kube-system` 能看到 cilium release
- [ ] **Helm 装 kube-vip**：`helm list -n kube-system` 能看到 kube-vip release
- [ ] **CNI 按节点覆盖**：集群里某 worker `nodes[].cni = "flannel"`，e2e 跑通；该节点 `kubectl -n kube-system get ds` 看到 flannel 看不到 cilium
- [ ] `make build` 产出单二进制 ≤ 80MB（含前端）
- [ ] `make test` 全部通过
- [ ] **Cilium kube-proxy 替换**：集群内 `kubectl -n kube-system get ds` 看不到 kube-proxy；Cilium ConfigMap 含 `kube-proxy-replacement: strict`；`cilium status` 显示 `KubeProxyReplacement: Strict`；NodePort / ClusterIP 服务连通正常
- [ ] 镜像版本：`cilium/cilium` 和 `kube-vip/kube-vip` 都用 `:latest` tag（按你要求）
- [ ] containerd config 从 `vendor/containerd/config.toml` 一份烤入（v0.0.1 同源 amd64/arm64），运行时写到 `/etc/containerd/config.toml`
- [ ] **v0.0.1 不依赖用户的魔改 containerd**：vendor/containerd/<arch>/ 留空也能 pack 和 init；CI 跑通后切 v0.1.x 验证 vendor 路径（届时用户二进制到位）

**v0.1.x（紧随其后）**：

- [ ] HA 模式外部 etcd 支持
- [ ] 节点增删进度 WebSocket 实时推送
- [ ] `ko doctor` 全绿（eBPF 内核检测 / SSH / 磁盘 / containerd 状态）
- [ ] 节点级 runtime / arch / cni override 全 UI 化

---

## 9. Open Questions

> 已消化完。spec 冻结前**只剩执行层确认**——下面两件你点头即可冻结。

### 9.1 sealos 对标清单（用户确认结果）

| sealos 能力 | ko v0.0.1 是否要做 | 备注 |
|---|---|---|
| CLI install / join / reset | ✅ 必做 | 已规划 |
| 节点 add/remove | ✅ 必做 | 已规划 |
| 主机 tune | ✅ 必做 | 已规划 |
| Web Dashboard | ✅ 必做（install/node/tune/logs/release/settings） | **带 basic auth** |
| 离线大镜像包 | ✅ 必做 | **主战场** |
| 多架构（amd64+arm64） | ✅ 必做 | 已规划 |
| registry mirror 默认 | ✅ 必做 | sealos 风格 |
| 容器运行时：upstream containerd v2 + docker | ✅ 必做 | **v0.0.1 阶段** |
| 容器运行时：用户魔改 containerd | ❌ v0.0.1 不做 | 留到 v0.1.x 切换 |
| 应用商店 / ClusterApp / 一键 helm 部署应用 | ❌ 永不做（用户确认） | 非目标 |
| 多集群联邦 | ❌ 不做 | 非目标 |
| 集群迁移 | ❌ 不做 | 非目标 |
| 公网/局域网自建镜像仓库 (sealos `registry` 子命令) | ❓ v0.0.1 不做，可选 v0.1+ | 待你最终定 |
| 用户/SSO 认证 | ❓ v0.0.1 走 basic auth，v0.1+ 再看 OIDC | 已规划 |
| 自动 eBPF 内核检测（推荐 Cilium / 降级 Flannel） | ❓ v0.0.1 严格按用户配置 | 留 v0.1+ |
| OpenAPI / SDK 暴露 ko 能力给上层 | ❓ v0.0.1 不做 | 待你最终定 |

**冻结前最后两件**：

- 上面 4 个 ❓ 全部按"v0.0.1 不做"处理（registry 子命令、SSO、eBPF 自动检测、SDK），OK 吗？
- 若有其他 sealos 能力你希望 v0.0.1 就做，列出来

---

## 10. 后续 Phase

spec 冻结后：

- **Phase 2 — Plan**：从 init 流程倒推：executor → config 解析 → kubeadm 包装 → containerd 安装（含 registry mirror）→ helm 装 cilium + kube-vip → dashboard 启动。识别并行任务
- **Phase 3 — Tasks**：每个 task 单 session 可完成，每条带 acceptance + verify
- **Phase 4 — Implement**：先 executor（mock 实现 → ssh 实现），再 init，再 node add/remove，再 tune，再 reset，再 dashboard，最后 `ko pack` 闭环

---

## 11. References

- **sealos（主参考）**：<https://github.com/labring/sealos> — ko 的功能对标对象，UX / 命令风格 / 离线包格式借鉴
- **sealos-controller**：<https://github.com/labring/sealos/tree/main/controllers> — Helm release controller 设计参考（v0.0.1 不实现，但留接口）
- kubeadm 内部设计：<https://kubernetes.io/docs/setup/production-environment/tools/kubeadm/>
- containerd CRI：<https://github.com/containerd/containerd/blob/main/docs/PLUGINS.md>
- kube-vip HA：<https://kube-vip.io/>
- Cilium 安装（含 kube-proxy 替换）：<https://docs.cilium.io/en/stable/installation/k8s-install-kubeadm/>
- Helm SDK：<https://helm.sh/docs/topics/advanced/#go-sdk>
- Kubespray（兜底参考，复杂路径时翻）：<https://github.com/kubernetes-sigs/kubespray>
