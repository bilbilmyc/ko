# Plan: `ko` v0.0.1

> Spec 冻结日期：2026-07-01
> 目标：交付 v0.0.1 首个可用版（详见 `docs/SPEC.md` §8）

---

## 1. 架构总览

```
                      ┌─────────────────────────────┐
                      │     ko (single binary)      │
                      │                             │
   ┌────────┐         │  ┌──────────┐  ┌────────┐  │
   │  Web   │◀──HTTP─▶│  │ Dashboard│  │  CLI   │  │
   │Browser │   WS    │  │ (REST+WS)│  │ (cobra)│  │
   └────────┘         │  └────┬─────┘  └───┬────┘  │
                      │       │            │        │
                      │  ┌────▼────────────▼────┐   │
                      │  │  internal/cluster/   │   │   集群编排
                      │  │  - config / executor │   │   kubeadm 包装
                      │  │  - kubeadm / certs   │   │   节点生命周期
                      │  │  - teardown / journald│  │
                      │  └────────┬─────────────┘   │
                      │           │                 │
                      │  ┌────────▼──────┐ ┌───────┴────┐
                      │  │ internal/     │ │ internal/  │
                      │  │ containerd    │ │ docker     │   运行时
                      │  │ + docker      │ │ (optional) │
                      │  └────────┬──────┘ └─────┬──────┘
                      │           │              │
                      │  ┌────────▼──────────────▼──────┐
                      │  │     internal/helm/           │   Helm SDK
                      │  │     (cilium / kube-vip /     │
                      │  │      flannel)                │
                      │  └────────┬─────────────────────┘
                      │           │                       │
                      │  ┌────────▼──────────┐ ┌─────────┴────┐
                      │  │ internal/tune/    │ │ internal/    │
                      │  │ (sysctl/modules/  │ │ image/       │   离线大镜像包
                      │  │  swap/systemd)    │ │ (OCI build)  │
                      │  └───────────────────┘ └──────────────┘
                      └─────────────────────────────┘
                                  │ SSH
                  ┌───────────────┼───────────────┐
                  ▼               ▼               ▼
            ┌──────────┐   ┌──────────┐   ┌──────────┐
            │ master-1 │   │ master-2 │   │ worker-1 │   Linux 节点
            │(control  │   │(control  │   │(kubelet) │   containerd
            │ plane)   │   │ plane)   │   │          │   + kube-vip
            └──────────┘   └──────────┘   └──────────┘
```

---

## 2. 组件依赖图

```
[executor] ─────────────────────────────────────────────┐
   │                                                    │
   ▼                                                    │
[config 解析] ──▶ [cluster 类型] ◀── [cluster.hcl]      │
                       │                                │
                       ▼                                │
                [kubeadm 包装] ◀────────────────────── [tune]
                       │                                │
                       ▼                                │
                [containerd 安装]                       │
                [docker 安装]                           │
                       │                                │
                       ▼                                │
                [helm 装 cilium / kube-vip / flannel]    │
                       │                                │
                       ▼                                │
                [node lifecycle]                        │
                       │                                │
                       ▼                                ▼
                [teardown] ─────▶ [doctor]          [dashboard API]
                       │                                │
                       ▼                                ▼
                  [journald] ─────────────────▶ [WS progress/logs]
                       │
                       ▼
                 [image/OCI builder] ◀── [ko pack]
                       │
                       ▼
                 [e2e tests]
```

**关键依赖**：

- executor 是地基，所有远程操作依赖它
- config 解析在所有命令之前
- dashboard 可以早期用 mock 数据并行开发
- 离线 pack 在所有功能稳定后做

---

## 3. 垂直切片（Vertical Slices）

按可交付顺序，不是按层堆。每条切片 = 一个可演示的能力。

### Slice 1：地基（5 天）

**目标**：`ko version` / `ko arch` / `ko doctor`（基础部分）能跑，SSH 通

| Task | 文件 | Verify |
|---|---|---|
| 1.1 仓库骨架 + go.mod + Makefile | `cmd/ko/main.go`, `Makefile`, `go.mod` | `make build` 出 `bin/ko` |
| 1.2 cobra CLI 框架 | `internal/cli/root.go`, `version.go`, `arch.go` | `ko --help`, `ko version` |
| 1.3 slog 日志 | `internal/logger/logger.go` | 输出带 level / 时间戳 |
| 1.4 HCL 配置解析 | `pkg/config/config.go`, `internal/cluster/config.go` | `ko doctor --config` 解析不报错 |
| 1.5 executor 抽象 + local + mock | `internal/cluster/executor.go`, `local_exec.go`, `mock_exec.go` | 单元测试 100% |
| 1.6 executor SSH | `internal/cluster/ssh_exec.go` | 连真实机器跑 `uname -a` |
| 1.7 doctor 基础（SSH/OS/磁盘） | `internal/cli/doctor.go` | `ko doctor` 输出 4 项检查 |

### Slice 2：单机 init（7 天）

**目标**：`ko init` 在 1 master + 0 worker 上能完成

| Task | 文件 | Verify |
|---|---|---|
| 2.1 upstream containerd 安装 | `internal/containerd/install.go` | 节点上 `containerd --version` 是 v2.x |
| 2.2 containerd systemd unit + config.toml 写入 | `internal/containerd/config.go` | `systemctl status containerd` active |
| 2.3 docker 安装（在线 + 离线 deb） | `internal/docker/install.go`, `daemon.go` | e2e 切 docker 跑通 |
| 2.4 registry mirror 写入（containerd + docker） | `internal/containerd/config.go`, `internal/docker/daemon.go` | `ctr info` 看到 mirror |
| 2.5 kubeadm init 包装 | `internal/cluster/kubeadm.go` | 1 节点 init 成功，apiserver up |
| 2.6 kube-vip（单节点模式） | `internal/cluster/kube_vip.go` | VIP 可达，apiserver 健康 |
| 2.7 Helm 装 Cilium（kube-proxy 替换） | `internal/helm/install.go`, `internal/cluster/cilium.go`, `cilium_kpfree.go` | `cilium status` strict，节点 Ready |
| 2.8 `ko init` 命令 | `internal/cli/init.go` | 单 master e2e 全绿 |
| 2.9 端到端 e2e：单 master | `test/e2e/single_master_test.go` | case 1 全绿 |

### Slice 3：HA + 多 master（5 天）

**目标**：`ko init` 在 3 master HA 拓扑上完成

| Task | 文件 | Verify |
|---|---|---|
| 3.1 stacked etcd 拓扑配置 | `internal/cluster/kubeadm.go` | `kubectl get nodes` 看到 3 master |
| 3.2 kube-vip HA（DaemonSet 模式） | `internal/cluster/kube_vip.go` | 杀一个 master，VIP 仍可达 |
| 3.3 cert 分发（**100 年有效期**） | `internal/cluster/certs.go` | 第二个 master join 成功；`ko cluster certs` 显示剩余 ~100y |
| 3.4 端到端 e2e：HA 3 master | `test/e2e/ha_stacked_test.go` | case 2 全绿 |
| 3.5 端到端 e2e：Flannel 兜底（某节点 cni=flannel） | `test/e2e/flannel_fallback_test.go` | case 11 全绿 |

### Slice 4：节点生命周期（5 天）

**目标**：`ko node add/remove` 在运行中集群上工作

| Task | 文件 | Verify |
|---|---|---|
| 4.1 node add worker | `internal/cluster/node_lifecycle.go` | 新节点 Ready，耗时 < 2min |
| 4.2 node add master（带 cert 签发） | 同上 | 新 master Ready，etcd 成员 +1 |
| 4.3 node remove（drain → delete → reset） | 同上 | 节点从集群消失，本地 kubelet 清理 |
| 4.4 node list / label | `internal/cli/node.go` | 列表正确，label 生效 |
| 4.5 端到端 e2e：add/remove 5 worker | `test/e2e/node_lifecycle_test.go` | case 3 全绿 |

### Slice 5：主机调优（3 天）

**目标**：`ko tune apply/show/reset`

| Task | 文件 | Verify |
|---|---|---|
| 5.1 sysctl 读写 | `internal/tune/sysctl.go` | `sysctl net.ipv4.ip_forward` = 1 |
| 5.2 内核模块 | `internal/tune/modules.go` | `lsmod \| grep br_netfilter` 有 |
| 5.3 swap / systemd / 磁盘 | `internal/tune/{swap,systemd,disk}.go` | 各项检查 OK |
| 5.4 profiles（production/dev/minimal） | `internal/tune/profiles.go` | 切 profile 后 sysctl 变化正确 |
| 5.5 `ko tune apply/show/reset` 命令 | `internal/cli/tune.go` | 端到端 e2e：case 5 |

### Slice 6：集群释放 / 杂项（3 天）

**目标**：`ko reset` 干净回滚 + `ko cluster info/certs/backup` + completion

| Task | 文件 | Verify |
|---|---|---|
| 6.1 `ko reset` 流程 | `internal/cluster/teardown.go`, `internal/cli/reset.go` | 3 次 init 不出脏数据 |
| 6.2 `ko cluster info / certs` | `internal/cli/cluster.go` | 信息正确，证书到期时间列出 |
| 6.3 `ko cluster backup`（etcd snapshot，stacked only） | 同上 | 恢复后集群正常 |
| 6.4 `ko completion` | `internal/cli/completion.go` | bash/zsh/fish 都能 source |
| 6.5 端到端 e2e：reset + 重复 init | `test/e2e/reset_test.go` | case 13 全绿 |

### Slice 7：离线大镜像包（7 天）

**目标**：`ko pack` 出 OCI image，`ko init --offline` 跑通

| Task | 文件 | Verify |
|---|---|---|
| 7.1 OCI image builder | `internal/image/builder.go` | 输出 tar.gz 用 `crane manifest` 可看 |
| 7.2 manifest list（per arch） | `internal/image/manifest_list.go` | amd64 + arm64 两个子清单 |
| 7.3 upstream containerd 下载（GitHub release） | `internal/image/upstream.go` | 下载到 `vendor/_cache/containerd-<v>-linux-<arch>.tar.gz` |
| 7.4 docker deb/rpm 预下载 | `internal/image/docker_pkgs.go` | OS-detect 拉对应包 |
| 7.5 helm chart 打包（cilium / kube-vip / flannel） | `internal/helm/pull.go` | tgz 在 `vendor/helm/` 下 |
| 7.6 k8s 镜像 ctr pull → tar | `internal/image/k8s_images.go` | 包内含 k8s 镜像 |
| 7.7 `ko pack / pack push / pack inspect` | `internal/cli/pack.go` | 三条命令可用 |
| 7.8 runtime 离线加载（ctr import） | `internal/image/puller.go` | `ko init --offline` 成功 |
| 7.9 端到端 e2e：离线（断网容器） | `test/e2e/offline_test.go` | case 6 全绿 |

### Slice 8：Doctor 完整化（2 天）

**目标**：`ko doctor` 全绿

| Task | 文件 | Verify |
|---|---|---|
| 8.1 eBPF 内核检测 | `internal/doctor/ebpf.go` | 不支持时警告 + 建议 flannel |
| 8.2 containerd 状态检查 | `internal/doctor/containerd.go` | running / version 报告 |
| 8.3 docker 状态检查 | `internal/doctor/docker.go` | running / version 报告 |
| 8.4 网络/磁盘/端口检查 | `internal/doctor/{net,disk,ports}.go` | VIP/6443/2379/2380 等 |
| 8.5 `ko doctor` 命令（已完成基础，补全） | `internal/cli/doctor.go` | 所有检查项报告 |

### Slice 9：Dashboard（10 天）

**目标**：`ko dashboard` 启动后能完成 install / node add / tune / release

| Task | 文件 | Verify |
|---|---|---|
| 9.1 HTTP server + basic auth middleware | `internal/dashboard/server.go`, `auth.go` | 未授权 401，basic auth 通过 |
| 9.2 REST API：cluster | `internal/dashboard/api/cluster.go` | 启动/状态/reset 端点 |
| 9.3 REST API：node | `internal/dashboard/api/node.go` | list/add/remove |
| 9.4 REST API：tune | `internal/dashboard/api/tune.go` | apply/show |
| 9.5 REST API：logs (历史) | `internal/dashboard/api/logs.go` | journalctl 查询接口 |
| 9.6 WebSocket：progress | `internal/dashboard/ws/progress.go` | 任务进度实时 |
| 9.7 WebSocket：logs (实时 tail) | `internal/dashboard/ws/logs.go` | journalctl -f |
| 9.8 前端：Vite + React + TS 骨架 | `web/` | `make web-dev` 起 dev server |
| 9.9 前端：Install 页面 | `web/src/pages/Install/` | 集群初始化表单 |
| 9.10 前端：Nodes 页面 | `web/src/pages/Nodes/` | 节点增删 |
| 9.11 前端：Tune 页面 | `web/src/pages/Tune/` | 调优 apply |
| 9.12 前端：Logs 页面 | `web/src/pages/Logs/` | 实时 + 历史 |
| 9.13 前端：Release 页面 | `web/src/pages/Release/` | 集群释放确认 |
| 9.14 前端：Settings 页面 | `web/src/pages/Settings/` | 集群信息 / 证书到期 |
| 9.15 embed.FS 把前端塞进 Go 进程 | `internal/dashboard/static/` | 单二进制无外部资源 |
| 9.16 端到端 e2e：Dashboard 鉴权 + 主流程 | `test/e2e/dashboard_test.go` | case 12 全绿 |

### Slice 10：多架构 arm64（3 天）

**目标**：amd64 + arm64 双架构 e2e 通过

| Task | 文件 | Verify |
|---|---|---|
| 10.1 Makefile 加 `amd64 / arm64` 编译目标 | `Makefile` | `make build-arch` 出两版二进制 |
| 10.2 CI 矩阵：qemu-aarch64 e2e | `.github/workflows/ci.yml` | arm64 模拟器跑核心 e2e |
| 10.3 端到端 e2e：arm64（qemu） | `test/e2e/arm64_test.go` | case 7 arm64 部分全绿 |
| 10.4 文档：arm64 smoke 测试清单 | `docs/RUNBOOK.md` | 物理机 smoke 步骤 |

### Slice 11：发布 / 文档（3 天）

**目标**：v0.0.1 release 可发

| Task | 文件 | Verify |
|---|---|---|
| 11.1 README 完整（quick start） | `README.md` | 新人 5min 跑通 |
| 11.2 RUNBOOK 运维手册 | `docs/RUNBOOK.md` | 升级 / 备份 / 故障排查 |
| 11.3 CHANGELOG | `docs/CHANGELOG.md` | v0.0.1 条目 |
| 11.4 GitHub Actions：lint + test + e2e + release | `.github/workflows/*.yml` | push 自动跑 |
| 11.5 `ko pack` GitHub Action（release 流水线） | `.github/workflows/release.yml` | tag 触发自动 pack + push |

---

## 4. 总时间线

| 切片 | 周 | 主要风险 |
|---|---|---|
| S1 地基 | W1 | executor SSH 兼容性 |
| S2 单机 init | W2-W3 | upstream containerd 安装路径 / Cilium 兼容 |
| S3 HA | W3-W4 | cert 分发 / etcd 集群一致性 |
| S4 节点生命周期 | W4-W5 | drain 边界 / master 移除的回滚 |
| S5 tune | W5 | 不同 OS 的 sysctl 差异 |
| S6 reset + 杂项 | W5-W6 | teardown 残留清理 |
| S7 离线包 | W6-W7 | OCI builder 复杂度 / 体积控制 |
| S8 doctor | W7 | eBPF 检测跨内核版本 |
| S9 dashboard | W7-W9 | WS 鉴权 / 前端集成 |
| S10 arm64 | W9 | qemu-aarch64 性能 / arm 内核差异 |
| S11 release | W9-W10 | 文档 / CI 跑通 |

**关键路径**：S1 → S2 → S3 → S7 → S9 → S11

---

## 5. 风险与缓解

| 风险 | 影响 | 缓解 |
|---|---|---|
| containerd 2.x 上游 API / 配置破坏 | 切片 S2、S7 | 钉死 minor 版本（v0.0.1 用 v2.0.x），CI 每日跑 upstream 监测 |
| K8s 1.35/1.36 客户端 API 变化 | 全局 | 用 `k8s.io/client-go` 对应版本，先在 S1 拉好依赖 |
| Cilium kube-proxy 替换 在某些 arm64 内核不可用 | 切片 S3、S10 | 节点级 `cni = "flannel"` 兜底；doctor 检测并告警 |
| Helm SDK + 离线 chart 复杂度 | 切片 S7 | 早期在 S1/S2 用 `helm template` 而非 `helm install`，后期再升级到 install |
| 离线大镜像包体积膨胀 | 切片 S7 | 分层打包（k8s 镜像 base + ko overlay），分 tag 复用；每层算 sha256 |
| WebSocket basic auth 在浏览器端的怪行为 | 切片 S9 | 第一次连接时浏览器带 Authorization header，握手后服务端校验 |
| arm64 qemu 模拟器慢 | 切片 S10 | CI 用并行 + 仅跑核心 case（init + add worker） |
| 用户魔改 containerd 切换（v0.1.x） | 未来 | v0.0.1 走 upstream，把 `source` 字段、vendor/ 目录预留好，切换成本低 |

---

## 6. 并行机会

| 切片 | 可并行的工作 |
|---|---|
| S1 | 没人能并行（地基） |
| S2 | 9.8-9.9（前端骨架 + Install mock）可与 S2 后半段并行 |
| S3 | S4 节点移除的 drain 逻辑可先并行设计 |
| S4 | 9.10-9.14（其他前端页面）可并行 |
| S5 | 独立，8.x（doctor 各项）可并行 |
| S6 | 独立 |
| S7 | 大头在 image 模块；其他人继续 e2e / doc |
| S9 | 前端 / 后端可严格并行（先定 API 契约） |
| S10 | arm64 物理机 smoke 由用户/团队单独跑，CI 不卡 |

---

## 7. 验证检查点（Phase 间门）

每条切片完成 = 一个 checkpoint：

- [x] S1 完：`ko version` / `ko arch` / `ko doctor`（基础）能跑，SSH 通
- [x] S2 完：1 节点集群能起来，`kubectl get nodes` Ready
- [x] S3 完：3 master HA 集群，VIP 切主后 apiserver 仍可达
- [x] S4 完：add/remove worker 跑通，add master 跑通
- [x] S5 完：tune apply 后关键 sysctl 生效
- [x] S6 完：reset 干净回滚 3 次不出问题
- [x] S7 完：离线环境 `ko init --offline` 成功
- [x] S8 完：doctor 全绿
- [x] S9 完：Dashboard 完成 install/node/tune/release/logs 主流程
- [x] S10 完：amd64 + arm64 e2e 都过
- [x] S11 完：v0.0.1 release tag 推送，CI release 流水线跑通
- [x] S14 完：外部 etcd + mTLS + systemd + 8h 备份 全部跑通
- [x] S15 完：`ko cluster restore` (stacked + external) 跑通 — `c41ebba`
- [x] S16 完：`ko reset --purge` 深度清理 — `860f98d`
- [x] S17 完：真实离线 — bundle 含 registry/kubeadm/k8s-images/cilium-images；master-1 自举 in-cluster registry + containerd mirror rewrite + `ko.local` host 解析

**v0.0.1 发布门**：S1–S11 + S14 全过 + SPEC §8 全部勾选。
S15 / S16 不在 SPEC §8 范围内（v0.0.1 → v0.1.x 过渡补强），已合入 Unreleased。

---

## 8. 当前状态 + 下一项

> 实时状态：v0.0.5 Unreleased。**当前主线 = 纯离线快速部署验证 + registry 服务调优落地**（单节点 / 多节点集群离线 init 5 分钟内 ready）。
>
> **关键约束**（2026-07-02 用户确认）：
> - **纯离线**：摒弃在线模式。SPEC §8 / README / RUNBOOK / PLAN §8.1 都把离线当主线，在线模式作为 fallback 或折叠。
> - **公司内部使用**：bundle 通过**公司内部存储**分发（非 GitHub release）；交付时 ko 二进制 + bundle tar 一并交付。
> - **低频大版本升级**：bundle 烤一次用很久；不做极致体积优化；不需要做太多升级自动化。
> - **registry 服务调优**：S17 v0.0.5 让 in-cluster registry 改用 static Go binary + systemd 硬化，同时对 containerd/kubelet 服务做配置调优（`max_concurrent_downloads`、`timeout`、`eviction-hard` 等），提升 offline airgap 场景稳定性

### 8.1 当前主线 P0：纯离线快速部署验证 + registry 调优落地

> 目标：证明 `ko init --offline --bundle ...` / `ko node add/remove --offline` 在单节点（1m0w）和多节点（3m3w）拓扑上端到端可用，registry 改 systemd 硬化运行，containerd/kubelet 服务配置调优生效，并实测部署耗时。结果回填到 SPEC §8 + RUNBOOK §1。

**子任务**（按推进顺序，每条独立可验证；**所有场景必须离线**）：

- [ ] **(a) kind 单 master 离线 init 跑通** — kind 起 1 节点 cluster，`ko init --offline --bundle ./bundle-k8s1.32.0-cilium1.16.1-20260705-multi.oci.tar.gz` 走完，`kubectl get nodes` Ready。**基线**。
- [ ] **(b) kind 1m + 1w 离线 add 跑通** — 加 1 个 worker 容器，`ko node add --offline --bundle ... --role worker` 走通，`kubectl get nodes` 两台 Ready。
- [ ] **(c) kind 1m + 3w 离线 add 跑通** — SPEC §8 "一次 add ≥ 5 worker" 简化为先 3 worker 离线 add 验证，5+ 留到真集群。
- [ ] **(d) kind HA 3m + 3w 离线跑通** — 3 master 容器 + 3 worker 容器；kube-vip 切主验证；`kubectl get nodes` 6 台 Ready。**SPEC 核心拓扑，全程离线**。
- [ ] **(e) 实测耗时 + 瓶颈** — 每个拓扑记录离线 init 端到端耗时（不含 pack），输出到 RUNBOOK §1；识别瓶颈（bundle scp / ctr images import / registry systemd 起来时间 / kubeadm phases / CNI ready）。
- [ ] **(f) 加 / 删节点离线耗时** — `ko node add worker` / `ko node remove <name>` 离线实测（含 bundle 重传成本）。
- [ ] **(g) registry 调优验证** — init 完后 `systemctl status ko-registry.service` 看 `User=65534`、`MemoryLimit=2G`、`CPUQuota=200%` 等是否生效；`containerd config dump` 看 `max_concurrent_downloads=3`、`timeout=30s`；`cat /var/lib/kubelet/kubeadm-flags.env` 看 `--image-pull-progress-deadline=30m`。
- [ ] **(h) CI 集成 kind e2e（离线）** — `.github/workflows/ci.yml` 加 `e2e-kind-offline` job，daily 跑（或 PR 触发）。bundle 烤入 job artifact，e2e job 拉 artifact 跑离线 init，失败直接红。**这是真集群 E2E 的最低成本实现**。
- [ ] **(i) 真集群一次（可选）** — 用户/团队在物理机或 libvirt 上跑一次 3m3w 离线 init，结果归档到 RUNBOOK §1.1。

**完成门**：
- (a)–(e) 全绿 → 把实测耗时填进 SPEC §8 success criteria 第 1 条 + RUNBOOK §1
- (g) 全绿 → 确认调优落地
- (h) 全绿 → 纯离线快速部署门槛正式落地；CI 每日回归
- (i) 跑了 → 把经验 / 坑写进 RUNBOOK §1.1

**SPEC §8 success criteria 第一条要改的措辞**（实测后回填）：
> `ko init --offline --bundle <bundle.tar.gz>` 在 3 master + 3 worker 拓扑上 5 分钟内完成（kind 实测 XX 分钟；真机 XX 分钟）

### 8.2 已合入 Unreleased

| Commit | 内容 |
|---|---|
| `f9df893` | S14 外部 etcd + generate-config |
| `c41ebba` | S15 `ko cluster restore`（stacked + external） |
| `860f98d` | S16 `ko reset --purge` 深度清理 |
| `ed50f66` | Dashboard 加固（rate limit + audit log，`golang.org/x/time/rate`） |
| `888e076` | CI 修复：arm64 job 装 `binfmt-support` |
| `500731e` | release.yml 修复：files glob 匹配 `dist/*.oci.tar.gz` |
| `2dd91ce` | S17 真实离线（代码） — bundle 加 registry/kubeadm/k8s-images/cilium-images layers；`OfflineRunner` 在 master-1 自举 in-cluster registry；containerd mirror rewrite + `ko.local` hosts 解析 |
| `991214f` | S17 真实离线（文档） — CHANGELOG Unreleased + PLAN §8 + RUNBOOK §2 + README |
| `bundle dedup` | `cliPuller.Save` 自己 dedup docker-archive（v0.0.3 / v0.0.4 内容 sha256 路径去重；当前 CI runner storage driver 自己 dedup，patch 是兜底防线） |
| `620185f` | docs: 修正 v0.0.3 / v0.0.4 bundle dedup 描述（doc-only，未 push） |
| `c6d2677` | pack: bundle 命名默认按 k8s+cilium+date，独立于 ko 版本 |
| `WIP` | v0.0.5 registry 改二进制运行（distribution/distribution v2.8.3）+ systemd 硬化（User=65534 / MemoryLimit=2G / CPUQuota=200% / ProtectSystem=strict）+ containerd 调优改 Go 模板（修了 endpoint no-op bug） |
| `WIP` | v0.0.5 kubelet systemd drop-in（`/etc/systemd/system/kubelet.service.d/20-ko-offline.conf`，airgap image-pull-deadline=30m + eviction 阈值） |
| `WIP` | v0.0.5 bundle push 后清 ctr cache + `/tmp/ko-bundle.oci.tar.gz`，省 master-1 disk（用户 2026-07-06 决策） |
| `WIP` | v0.0.5 containerd 默认拉 GitHub latest stable（cache 24h），docker CE 改为 channel latest，不再写死版本号（用户 2026-07-06 决策） |
| `WIP` | v0.0.5 `ko node remove` / `ko reset` 离线清理补全：`/etc/hosts` ko.local 行 strip + registry service/binary/data/config 清理 |
| `WIP` | v0.0.5 `--offline` 强制 `--bundle` 校验：init 启动时 upfront 拒绝 silent footgun |

**v0.0.1 已发布** — tag 指向 `500731e`，release 产物：`ko-linux-amd64` / `ko-linux-arm64` / `ko-v0.0.1-multi.oci.tar.gz`（**注意**：v0.0.1 的 bundle 只含 containerd，真离线能力随 S17 发布）

**v0.0.2 已发布** — 真离线 release（amd64 bundle 826M）

**v0.0.3 已发布** — bundle dedup 防御性 patch（`dedupDockerArchive` 单元测试覆盖，当前 CI 是 no-op）

**v0.0.4 已发布** — dedup 改用内容 sha256 而非文件路径，更稳健（amd64 bundle 仍 826M；用户确认大小无所谓）

### 8.3 v0.0.1 收尾 P0（必须）

- [ ] **真集群 E2E** = §8.1 (g)/(h) 两条；其余功能已发 v0.0.4
- [x] **Dashboard auth 加固**（rate limit + audit log，`golang.org/x/time/rate`）
- [x] **打 v0.0.1 tag + 触发 release workflow**（`S11` 收尾）
- [x] **S17 真实离线**（bundle 含所有镜像 + in-cluster registry）

### 8.4 v0.1.x 候选（按"用户最痛"排）

- **secrets 加解密**（SSH password 走 HCL 现在是明文）— 安全硬伤，先做
- **Registry mirror 默认配置（sealos 风格）** — `image` 块加 `registry_mirrors` 默认值；离线 bundle 烤入 mirror index
- **`--log-format=json`** — 让 dashboard / journal 都能 grep
- **节点增删 WebSocket 实时推送** — dashboard UX 升级
- **Dashboard 真前端**（Vite + Vue/React — SPEC 提的是 React，团队熟 Vue 待确认）
- **`ko upgrade`**（SPEC 明确 v0.0.x 不做，v0.1.x 切）
- **Prometheus `/metrics`** + structlog
- **IPv6 single-stack / dual-stack**
- **eBPF 自动检测 / 节点级 runtime/arch/cni UI 化**（SPEC §8 v0.1.x）

### 8.5 不做（明确边界）

- App store / ClusterApp
- 集群内升级（v0.0.x 范围外，v0.1.x 起；且低频升级优先级更低）
- Windows / macOS 节点
- 多集群联邦
- 集群迁移
- **公网 / 在线模式相关**：不做在线 init 流程优化；bundle 不上 GitHub release 之外的公网 registry（GitHub release 留给开发自测）

### 8.6 ko / bundle 双轨版本控制（用户 2026-07-02 决策）

> **核心原则**：**ko 工具本身**走 GitHub release + git tag（独立版本号 v0.0.x）；**bundle（镜像 + chart + 二进制）**走公司内部存储（独立版本号）。**两者解耦**——bundle 不跟 ko release tag，新 k8s 版本 / cilium 版本可以单独烤 bundle，不动 ko 代码。

**已定决策**：

- ✅ **NFS / 文件共享挂载**（#60 已完成）— 所有集群节点挂载同一份 NFS（如 `/mnt/ko-store/`），bundle 直接放文件系统。`ko init --bundle /mnt/ko-store/bundle-k8s1.32.0-cilium1.16.1-20260702-multi.oci.tar.gz`。
- ✅ **单 tar 交付物**（#61 已完成）— `ko pack ship --output delivery.tar.gz --bundle /mnt/ko-store/<bundle-name>.oci.tar.gz --include-configs` 打一份 tar（含 `ko` 二进制 + 指定 bundle + 三个 profile 模板），运维解压即用。
- ✅ **强制离线**（#64 待做）— `ko init` / `ko node add` 必须带 `--offline --bundle <path>`；不带就报错退出。开发自测在线场景用 `--allow-online` flag 跳过。
- ✅ **(iv) bundle 版本号命名约定**（#63 完成）— `bundle-k8s<X.Y.Z>-cilium<X.Y.Z>-<YYYYMMDD>-<arch>.oci.tar.gz`（例：`bundle-k8s1.32.0-cilium1.16.1-20260702-multi.oci.tar.gz`）。`internal/cli/pack.go` 默认 bundle 名按此格式生成（基于 k8s/cilium 当前常量 + 当天日期），用户可用 `--version` override。

**执行步骤**（每条独立可验证）：

- [x] **(γ) #63 bundle 命名约定落地（代码）** — `internal/cli/pack.go` 新增 `defaultBundleName(k8s, cilium string) string` helper；默认 `--version` 从 `"v0.0.1"` 改成 `defaultBundleName(...)`。单元测试覆盖（`internal/cli/pack_test.go`）。
- [ ] **(α) #64 强制离线（代码）** — `internal/cli/init.go` 和 `internal/cli/node.go` 加：检测 `!cmd.Flags().Changed("offline")` 时 `return fmt.Errorf("纯离线项目，ko init 必须带 --offline --bundle <path>（开发自测用 --allow-online 跳过）")`。同步加 `--allow-online` flag。单元测试覆盖。
- [ ] **(β) `ko pack ship` 命令**（#65 待创建）— 新增 `internal/cli/pack.go::newPackShipCmd`，把 ko 二进制 + bundle + 三个 profile 模板（single / ha / external-etcd）打成一个 tar。命令格式见上文。RUNBOOK §1.5 引用。
- [ ] **(δ) RUNBOOK §1.5 + README 离线示例** 改 NFS 路径 + delivery tar — 见 #62 子任务。

### 8.7 代码 / 决策锚点（防止后续 session 想"为什么这么写"）

### 8.7 代码 / 决策锚点（防止后续 session 想"为什么这么写"）

- **证书 100 年**：`internal/cluster/kubeadm.go` 的 `CertificateValidity = 876000h`，kubeadm init + join 都强制
- **Cilium kube-proxy 替换 strict**：`internal/cluster/init.go` 默认，`needsFlannel()` 兜底降级
- **stacked vs external etcd**：`etcd.mode` 切换；external 模式走 `internal/cluster/etcd_external.go` 全套 mTLS
- **离线 bundle 自定义 mediaType**：`application/vnd.ko.layer.*.v1`，不能直接 `docker load`
- **S17 in-cluster registry（v0.0.5）**：`ko.local:5000`（master-1 IP 写每节点 `/etc/hosts`）；`OfflineRunner` 从 bundle 解 `registry` Go 二进制到 `/usr/local/bin/registry`，以 systemd `ko-registry.service` 硬化运行（`User=65534`、`MemoryLimit=2G`、`CPUQuota=200%`、沙盒）；registry 配置监听 `:5000` + `:5001`（debug/prometheus）；kubeadm init/join 都用 `--image-repository=ko.local:5000` 绕开公网
- **containerd mirror 自动 rewrite**：`[plugins."io.containerd.grpc.v1.cri".registry.mirrors."quay.io"]` 等四个 upstream 全部 mirror 到 `http://ko.local:5000`；`insecure_skip_verify = true`（无 TLS，内网）
- **exec 包独立**（`internal/exec/`）：断 `cluster ← containerd/docker` 的 import cycle
- **dashboard 默认 127.0.0.1:8080**：要监听 0.0.0.0 必须显式 `--listen`，生产加 nginx + TLS（RUNBOOK §5.2）

---

## 8. Phase 3 — 任务拆分

见 `docs/TASKS.md`（待生成），每条任务单 session 可完成，带 acceptance + verify。

---

## 9. Phase 4 — 实施原则

- 每个 slice 完成后跑对应 e2e 再进入下一个
- 任何 HCL schema 字段变化先更新 SPEC.md 再改代码
- 任何 `internal/` 包的结构变化先在本 PLAN 标注
- e2e 失败 = 阻塞，不允许"等下个 slice 修"
- arm64 / amd64 CI 矩阵都绿才能进 release
