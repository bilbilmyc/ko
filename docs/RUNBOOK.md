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

### 5.3 不要暴露到公网

Dashboard 没有审计日志，没有速率限制，basic auth 是唯一防线。**只**监听 127.0.0.1，或放在内网 nginx 后。

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

ko v0.0.1 不自动 restore（手动用 `etcdctl snapshot restore`）。恢复步骤：

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
# ko 会 ssh 到所有节点跑 kubeadm reset + 清 /etc/kubernetes + 清 /var/lib/etcd
```

reset 是不可逆的——执行前确认 etcd 已经备份。

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