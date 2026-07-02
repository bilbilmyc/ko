# Changelog

ko 的所有重要变更都会记在这里。格式基于 [Keep a Changelog](https://keepachangelog.com/)，
版本遵循 [Semantic Versioning](https://semver.org/)。

## [Unreleased]

### Fixed — 修正 v0.0.3 / v0.0.4 的 bundle dedup 描述（backport 到上面的 entry）

实测 v0.0.2 / v0.0.3 / v0.0.4 三个 release 的 amd64 bundle 体积都是 ~826M（差异 < 10KB 是 round-trip 噪声）。原因是当前 GitHub Actions runner 用 docker vfs storage driver，自己已经按内容 dedup 了，`dedupDockerArchive` 在这套 storage driver 上没有 duplicate payload 可去。

之前 CHANGELOG / README / PLAN / RUNBOOK 里写的 "826M → ~280M" 是基于 arm64 本地烤包体积的误解：~280M 是 **arm64 image** 真实 unique content 体积，不是 dedup 效果。amd64 image 真实 unique content 就是 ~826M。

为什么 v0.0.3 / v0.0.4 仍然保留：换 storage driver / runner / nerdctl 版本时 dedup 是兜底防线。功能正确，单元测试覆盖（合成 docker-archive + 真 docker save + 拒绝非 docker-archive 三种场景）。

本版本无代码改动，仅文档修正（不上 tag）。

## [v0.0.4] — 2026-07-02

### Fixed — `dedupDockerArchive` 按 blob 内容 sha256 去重，不按文件路径

v0.0.3 用的是路径去重（`blobs/sha256/<digest>` 同名收一份）。改成 **内容 sha256 去重**：hash 每个 blob 的实际 payload，第一份保留，后续同内容直接丢弃。这更稳健——即便上游保存层把同一份 blob 写到不同路径（极少见但理论上可能），dedup 也认。

实测结果：在当前 GitHub Actions runner（docker vfs storage driver）烤出的 amd64 bundle 里，dedup 仍然是 no-op，因为 storage driver 自己已经按内容去重了一层，没有重复 payload。release asset 体积三个版本都在 826M（差异 < 10KB 是 round-trip 噪声）。

为什么仍然保留这个 patch：当 storage driver 是 overlay2 / fuse-overlayfs 等不自动 dedup 的实现（很多 CI 自托管 runner 是这样），或者 nerdctl save 在某些 platform 行为变化时，dedup 是兜底防线。功能正确，单元测试覆盖。

新增 1 个测试：`TestDedupDockerArchive_CollapsesDuplicateBlobs`（在 v0.0.3 基础上扩展，覆盖同内容不同路径场景）。

## [v0.0.3] — 2026-07-02

### Added — `cliPuller.Save` 自己 dedup docker-archive（防御性 patch）

`docker save`（和 `nerdctl save` 默认）输出的 docker-archive tar 在 storage driver 不支持 cross-image dedup 时（GitHub Actions runner 的 docker vfs 是这种），每个 image 的每个 layer blob 都单独存一份，跨 image 的共享 layer 被复制多遍。

`cliPuller.Save` 在 save 之后立即跑 `dedupDockerArchive`：读 docker-archive tar，把重复的 `blobs/sha256/<digest>` 收成一份，重写 tar。`manifest.json` 仍按 sha256 路径引用 blob，**对 `ctr images import` 完全透明**——不需要换 offline runner 的导入逻辑。

实测结果：当前 GitHub Actions 烤出的 amd64 bundle 体积 v0.0.2 / v0.0.3 / v0.0.4 **三个版本都在 826M**（差异 < 10KB 是 round-trip 噪声）。这是 amd64 image 真实 unique content 大小——不是 dedup 没起作用，而是当前 CI runner 的 storage driver 自己就已经按内容去重了，dedup 是 no-op。

为什么仍然保留这个 patch：换 storage driver / 换 runner / 换 nerdctl 版本 时，dedup 是兜底防线。**功能正确，单元测试覆盖（合成 docker-archive + 真 docker save 两种场景）**。

新增 3 个测试：`TestDedupDockerArchive_CollapsesDuplicateBlobs`、`TestDedupDockerArchive_OnRealDockerSave`、`TestDedupDockerArchive_RefusesUnrecognizedTar`。

> **bundle 体积预期**：~280M 是 **arm64 image** 在本地烤的体积，不是 amd64 bundle dedup 效果。amd64 bundle 真实 unique content 就是 ~826M（k8s-images layer ~559M + cilium-images layer ~210M + 其他）。

## [v0.0.2] — 2026-07-02

### Added — S17 真实离线（in-cluster registry + 镜像仓库自举）

v0.0.1 的 bundle 只烤了 containerd；S17 让 bundle 里同时含 `kubeadm` 二进制、`k8s 1.32` 控制面镜像、`registry:2` 镜像、`cilium 1.16` chart、cilium 全部镜像；`ko init --offline` 会在 master-1 起一个 in-cluster Docker distribution registry，把这些镜像 re-tag 后 push 进去，然后配置 containerd 把 `quay.io` / `registry.k8s.io` / `docker.io` / `ghcr.io` 全部 mirror 到 `ko.local:5000`；kubeadm / cilium / node join 都通过 `--image-repository=ko.local:5000`（或 containerd mirror 自动 rewrite）从本地仓库拉，**完全不再访问公网**。

- **`OfflineRunner`**（`internal/cluster/offline.go`）：scp bundle 到 master-1 → 按 mediaType 拆 layer → 解包 containerd + kubeadm → `ctr -n=k8s.io images import` 拉 k8s/registry/cilium 镜像 → `nerdctl run registry:2 --net=host` 起仓库（带 30s readiness probe）→ retag + `ctr push --plain-http` 把每个镜像推到 `ko.local:5000/<repo>` → 把 `<master-1-IP> ko.local` 写入每个节点 `/etc/hosts`（幂等：`grep -qF` 守卫）
- **`image.MediaTypeKo{RegistryImage,KubeadmBinary,CiliumImagesTar}`** — 3 个新自定义 mediaType，让 bundle 里每类 artifact 自带类型
- **`image.K8sImagesForVersion(v, arch)`** / **`image.CiliumImagesForVersion(v)`** — 固化 k8s 1.32（kube-apiserver/controller-manager/scheduler/proxy、coredns、pause、etcd）+ cilium 1.16（cilium/operator-generic/hubble-relay/hubble-ui/hubble-ui-backend/certgen）两套完整镜像清单
- **`image.ImagePuller`** — nerdctl 优先、docker 兜底；让 cilium images tar 的烤包可复现
- **`Init.imageRepositoryOverride`** — offline 模式下 kubeadm init / join 都从 `ko.local:5000` 拉镜像（不再走 `cfg.Image.Registry` / `cfg.Image.Repository`）
- **containerd mirror config** — `[plugins."io.containerd.grpc.v1.cri".registry.mirrors."<host>"]` + `insecure_skip_verify = true`，自动重写所有 upstream 拉取到本地 registry

新增 17 个测试（`internal/cluster/offline_test.go` 11 个 + `internal/image/upstream_test.go` 6 个），全绿。bundle 体积从 v0.0.1 的 68M → 281M，跨架构内容可寻址去重后实际增量更小。

> **迁移说明**：v0.0.1 的 bundle 不含完整镜像，离线 init 实际还需访问外网拉镜像。**v0.0.2 才是真离线 release**，建议直接用 v0.0.2。

## [v0.0.1] — 2026-07-02

首个可用版本（v0.0.1）。范围：单机 init、HA 多 master、节点生命周期、主机调优、集群操作、离线 OCI bundle、Doctor 预检、Web Dashboard、多架构、外部 etcd、cluster restore、reset --purge、dashboard 硬化。

### Added

- **Dashboard 速率限制 + 审计日志** — `golang.org/x/time/rate` token bucket（默认 1 req/s, burst 20）挡在 basicAuth 前；`/var/log/ko/dashboard-audit.log`（mode 0600，append-only）记录所有请求（200 / 401 / 429 / 500），字段：RFC3339Nano remote user method path status bytes dur；新增 `--rate-limit` / `--rate-burst` / `--audit-log` flag，审计失败降级到 `io.Discard` 不阻塞请求
- **`ko reset --purge` 强化清理** — 默认 reset 在原 `kubeadm reset` 基础上额外清理:`/etc/cni/net.d`、`/opt/cni/bin`、`/var/lib/cni`、CNI / Cilium / kube-ipvs0 / veth* 接口、所有 iptables 表（filter/nat/mangle/raw）、overlay + kubelet pod volume 挂载、`/etc/systemd/system/{etcd,ko-etcd-backup,containerd}.service`、`/var/lib/etcd`(per-member) / `/etc/etcd` / `/var/backups/etcd`、`/etc/containerd/config.toml`、`/etc/docker/daemon.json`、kubelet drop-in;`--purge` 进一步清空 `/var/lib/{containerd,docker,ko}`、`/root/.ko`、ctr k8s.io 容器;external etcd 模式自动先调 `UninstallExternalEtcd`
- **`Teardown.ResetAllWithConfig(cfg)`** — 新的入口,按 `cfg.Etcd.Mode` 自动决定是否先卸外部 etcd
- **`Teardown.Purge` 字段** — 控制 `--purge` 行为
- **`ko cluster restore`** — 从 snapshot 恢复 etcd,支持 stacked + external 两种模式; external 模式按 member 顺序 stop etcd → move data dir aside → scp snapshot → etcdctl snapshot restore → start,stacked 模式先停所有 master 的 kubelet 再按 master 顺序 restore 最后启回
- **`ko cluster backup`** 扩展支持 external etcd 模式(按 member 顺序各做一次 snapshot 并 scp 回本地)
- **`etcd.Service.InitialCluster()`** 导出,restore 时构造 `--initial-cluster`
- **S14 外部 etcd** — etcd 3.5.21 作为外部二进制 + systemd unit + mTLS 自签 PKI（ca / server / peer / client）+ 8 小时 systemd timer 备份（14 天滚动）+ dashboard `/api/etcd/{status,backups}` 端点
- **`ko etcd install|status|backup|uninstall` 命令族**
- **sealos 风格 `ko init --generate-config=PROFILE`** — profile: `single` / `ha` / `external-etcd`,带嵌入式 HCL 模板 + 注释
- **race-mode CI 修复** — `internal/cluster/local_exec.go` 缓冲区在 `cmd.Run()` 之后读取,`TestLocalExecutor_PropagatesExitError` 改用 `sh -c 'echo ... >&2; exit 1'`
- **Go 1.25.5 锁版本** — `go.mod` 1.26.4,CI/release workflow 用 1.25.5,`helm.sh/helm/v3` 锁 v3.21.0（v3.21.x 系列里最后一个不要求 Go 1.26 的版本）

### Added

#### S1 — 地基
- Go 1.22+ 单二进制 CLI（cobra），cmd 入口在 `cmd/ko/`
- 全局 flag：`--config`、`--verbose`、`--log-level`、`--offline`
- SSH executor（`internal/exec`），host:port / key file / password 都支持
- HCL 配置解析（`pkg/config`）：`cluster` / `nodes` / `ha` / `runtime` / `cni` / `tune` / `dashboard` 块
- 子命令骨架：`version` / `arch` / `doctor` / `init` / `node` / `tune` / `reset` / `cluster` / `pack` / `dashboard` / `completion`

#### S2 — 单机 init
- containerd v2 安装（`internal/containerd`），从 GitHub release 下载 + SHA256 校验
- docker CE 安装（`internal/docker`），apt + dnf 双支持
- kubeadm 安装 + `kubeadm init` 编排
- Cilium 安装（helm），`kubeProxyReplacement=strict`（无 kube-proxy）
- 集群信息落 `~/.ko/kube/admin.conf`

#### S3 — HA 多 master + 100 年证书
- kube-vip DaemonSet + VIP 绑定（HA 控制面）
- 多 master join 编排（自动 join 其余 master + 所有 worker）
- 100 年证书有效期（`--certificate-validity=876000h`）
- Flannel 兜底 CNI（`vxlan` / `host-gw`）
- `needsFlannel()` 自动降级（Cilium 起不来时切 Flannel）

#### S4 — 节点生命周期
- `ko node add/remove/list/label`
- 加 worker / 加 master（自动签 control-plane 证书）
- 删节点（drain + delete + reset）
- 远程 containerd / docker 安装（复用 S2 的 installer）

#### S5 — 主机调优
- sysctl 配置（写 `/etc/sysctl.d/99-ko.conf` + `sysctl --system`）
- 内核模块加载（`br_netfilter` / `overlay` 等）
- swap 关闭（systemd + /etc/fstab）
- profile 化（`production` / `dev` / `minimal`）
- `ko tune apply/show/reset`

#### S6 — 集群操作
- `ko reset`（带确认，全集群清理）
- `ko cluster info`（集群概览）
- `ko cluster certs`（每台 master 的证书剩余有效期）
- `ko cluster backup`（etcd snapshot）

#### S7 — 离线 OCI bundle
- `ko pack build --arch amd64|arm64`（单架构 bundle）
- `ko pack inspect <bundle>`（看 index / manifest / layer）
- OCI image layout tar.gz，自定义 mediaType 区分各 artifact
- upstream 下载：containerd / docker / helm chart / k8s 镜像
- 离线 init：`ko init --offline --bundle <path>`

#### S8 — Doctor 完整化
- kernel 版本检查（≥ 5.4）
- swap 状态检查
- 运行时（containerd / docker）检查
- 端口 6443 / 10250 / 10251 / 10252 检查
- 镜像加速器配置检查

#### S9 — Web Dashboard
- HTTP server + basic auth（`subtle.ConstantTimeCompare`，常量时间比较防 timing attack）
- recoverer 中间件（panic → 500，不掉连接）
- REST API：`/api/{cluster/info,nodes,nodes/add,nodes/remove,tune/apply,certs,healthz}`
- 默认 minimal HTML（带 `--static-dir` 可挂前端）
- `ko dashboard --user --password --static-dir`，密码走 `KO_DASHBOARD_PASSWORD` 环境变量

#### S10 — 多架构
- `ko pack build --arch all`（amd64 + arm64 同时打包，输出 `-multi.oci.tar.gz`）
- `image.BuildMulti`：per-arch manifest，相同 layer blob 按 digest 去重
- Makefile 双架构交叉编译（注入 version / commit / build-date ldflags）
- GitHub Actions CI：amd64 跑 race tests + arm64 qemu 冒烟
- Release workflow：tag 触发双架构二进制 + multi-arch bundle 上传 GitHub Release

### Notes

- 不做 App store / ClusterApp
- 不内置 registry 镜像加速器
- 不替代 kubectl
- 不做集群内升级（v0.0.1 范围外）
- 证书 100 年有效期是**强约束**——kubeadm 默认 1 年，ko 强制 `--certificate-validity=876000h`
- 默认 CNI 是 Cilium（kubeProxyReplacement=strict），要求 kernel ≥ 5.4；机器老就切 Flannel

[v0.0.1]: https://github.com/bilbilmyc/ko/releases/tag/v0.0.1
[v0.0.2]: https://github.com/bilbilmyc/ko/releases/tag/v0.0.2
[v0.0.3]: https://github.com/bilbilmyc/ko/releases/tag/v0.0.3