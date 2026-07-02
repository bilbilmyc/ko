# Changelog

ko 的所有重要变更都会记在这里。格式基于 [Keep a Changelog](https://keepachangelog.com/)，
版本遵循 [Semantic Versioning](https://semver.org/)。

## [Unreleased]

### Added

- **`ko cluster restore`** — 从 snapshot 恢复 etcd,支持 stacked + external 两种模式; external 模式按 member 顺序 stop etcd → move data dir aside → scp snapshot → etcdctl snapshot restore → start,stacked 模式先停所有 master 的 kubelet 再按 master 顺序 restore 最后启回
- **`ko cluster backup`** 扩展支持 external etcd 模式(按 member 顺序各做一次 snapshot 并 scp 回本地)
- **`etcd.Service.InitialCluster()`** 导出,restore 时构造 `--initial-cluster`
- **S14 外部 etcd** — etcd 3.5.21 作为外部二进制 + systemd unit + mTLS 自签 PKI（ca / server / peer / client）+ 8 小时 systemd timer 备份（14 天滚动）+ dashboard `/api/etcd/{status,backups}` 端点
- **`ko etcd install|status|backup|uninstall` 命令族**
- **sealos 风格 `ko init --generate-config=PROFILE`** — profile: `single` / `ha` / `external-etcd`,带嵌入式 HCL 模板 + 注释
- **race-mode CI 修复** — `internal/cluster/local_exec.go` 缓冲区在 `cmd.Run()` 之后读取,`TestLocalExecutor_PropagatesExitError` 改用 `sh -c 'echo ... >&2; exit 1'`
- **Go 1.25.5 锁版本** — `go.mod` 1.26.4,CI/release workflow 用 1.25.5,`helm.sh/helm/v3` 锁 v3.21.0（v3.21.x 系列里最后一个不要求 Go 1.26 的版本）

## [v0.0.1] — 2026-07

## [v0.0.1] — 2026-07

首个可用版本（v0.0.1）。范围：单机 init、HA 多 master、节点生命周期、主机调优、集群操作、离线 OCI bundle、Doctor 预检、Web Dashboard、多架构支持。

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