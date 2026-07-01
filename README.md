# ko

Kubernetes 集群生命周期管理工具，对标 [sealos](https://github.com/labring/sealos)，**离线优先**。

> 当前状态：**v0.0.1 开发中**（首个可用版）。详见 [`docs/SPEC.md`](docs/SPEC.md) 和 [`docs/PLAN.md`](docs/PLAN.md)。

## 特点

- **离线优先**：所有组件打成一个 OCI 大镜像包，断网环境也能装
- **多架构**：amd64 + arm64
- **双运行时**：上游 containerd v2 + docker 可切
- **CNI 双轨**：Cilium（默认，kube-proxy 替换）+ Flannel（兜底）
- **HA 自带**：kube-vip 自动管 VIP
- **Web Dashboard**：集群安装 / 节点管理 / 主机调优 / journald 实时日志
- **主机调优**：sysctl / 内核模块 / swap / systemd 一键优化
- **证书 100 年**：内部 CA 自签，集群所有证书 100 年有效期

## 快速开始（开发版）

```bash
make build
./bin/ko version
./bin/ko doctor
```

## 文档

- [`docs/SPEC.md`](docs/SPEC.md) — 规格说明（v0.0.1 已冻结）
- [`docs/PLAN.md`](docs/PLAN.md) — 实施计划（11 个垂直切片）
- [`docs/CHANGELOG.md`](docs/CHANGELOG.md) — 变更日志

## 命令

```
ko version                         工具版本
ko arch                            当前二进制 arch
ko doctor [--config cluster.hcl]   预检
ko init --config cluster.hcl       初始化集群
ko node add/remove/list/label      节点管理
ko tune apply/show/reset           主机调优
ko reset                           释放集群
ko cluster info/certs/backup       集群操作
ko pack build/push/inspect         离线包制作
ko dashboard                       Web Dashboard
ko completion <bash|zsh|fish>      补全脚本
```

## 路线图

| 版本 | 目标 |
|---|---|
| v0.0.1 | 首个可用版：init / node add-remove / tune / reset / dashboard + 离线大镜像包 + amd64+arm64 |
| v0.1.x | HA 外部 etcd / 切换到用户魔改 containerd / eBPF 自动检测 / SSO |
| v0.2+ | 看 v0.0.1 反馈决定 |

## License

TBD
