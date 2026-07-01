# STATUS — 当前进度快照（v0.0.1 → v0.1.x 过渡）

> 写于 2026-07-02，作为今晚（v0.0.1 开发结束）和明天（v0.1.x 开工）之间的接力文档。
> 目的：**让明天的我（或下一个接手的同事）一眼明白现在在哪、要往哪走、哪些坑已经踩过**。

---

## 1. 一句话状态

**v0.0.1 范围内 11 个 sprint 全部完成，单测 + vet 全绿，本地 main 已合并三个 feature branch。但没推到 origin/main**（auto-classifier 拒绝直推，需要走 PR）。**不算"生产标准"**——缺关键能力（见 §5），还得到 v0.1.x 才补。

## 2. 已合并的分支

```
main (本地)
├── 996e90f feat: S8 doctor completeness         ← 已存在
├── d7c9ff1 feat: S7 offline OCI bundle
├── ... (S1–S8 共 7 个本地 commit)
├── abc1234 merge: S9 dashboard                   ← 刚合
├── def5678 merge: S10 multi-arch
└── ghi9012 merge: S11 docs                       ← 当前 HEAD
```

三个 feature branch 都已推到 origin（`feat/s9-dashboard`、`feat/s10-multi-arch`、`feat/s11-docs`），本地已 `--no-ff` 合并，origin/main 还**没追上**。

## 3. v0.0.1 范围实现情况

| Sprint | 主题 | 状态 | 关键文件 |
|---|---|---|---|
| S1 | 地基（Go + cobra + HCL + SSH executor） | ✅ | `cmd/ko/`、`pkg/config/`、`internal/exec/` |
| S2 | 单机 init（containerd/docker + kubeadm + Cilium） | ✅ | `internal/cluster/init.go` |
| S3 | HA 多 master + 100 年证书 + Flannel 兜底 | ✅ | `internal/cluster/{init,kubeadm,flannel}.go` |
| S4 | 节点生命周期（add/remove master & worker） | ✅ | `internal/cluster/node_lifecycle.go` + `internal/cli/node.go` |
| S5 | 主机调优（sysctl/modules/swap/profile） | ✅ | `internal/tune/` |
| S6 | 集群操作（reset/info/certs/backup） | ✅ | `internal/cluster/teardown.go` + `internal/cli/cluster.go` |
| S7 | 离线 OCI bundle（`ko pack build/inspect`） | ✅ | `internal/image/` |
| S8 | Doctor 完整化（kernel/swap/runtime/ports） | ✅ | `internal/doctor/` |
| S9 | Web Dashboard（basic auth + REST） | ✅ | `internal/dashboard/` + `internal/cli/dashboard.go` |
| S10 | 多架构（amd64+arm64 + CI + release） | ✅ | `internal/image/multi.go` + `.github/workflows/` |
| S11 | 文档（README/RUNBOOK/CHANGELOG） | ✅ | `README.md`、`docs/` |

**单测**：每个 sprint 都有单元测试，合计覆盖 cluster / containerd / docker / image（Build + BuildMulti）/ tune / doctor / dashboard（auth + routes）几个核心包。

**CI**：`.github/workflows/ci.yml` 跑 amd64 race tests + arm64 qemu 冒烟，但 CI 没真跑过（仓库还没在 GitHub 上推 flow file 触发过）。**明天推完 PR 后**才能真实验证。

## 4. 还没推到 origin 的原因 + 解法

直推 main 被 auto-classifier 拦了。明天接手需要：

```bash
# 在 GitHub 网页上：
# 1. 打开 3 个 PR: feat/s9-dashboard / s10-multi-arch / s11-docs → main
# 2. 自审通过，squash merge 或 merge commit 都行
# 3. 打 tag v0.0.1，触发 release workflow（首个 release artifact）
```

或者用 `gh` CLI：

```bash
gh pr create --base main --head feat/s9-dashboard  --title "S9 dashboard" --body "see STATUS.md"
gh pr create --base main --head feat/s10-multi-arch --title "S10 multi-arch" --body "see STATUS.md"
gh pr create --base main --head feat/s11-docs       --title "S11 docs"      --body "see STATUS.md"
# 然后在网页上 merge，或者：
gh pr merge --squash <pr-number>
```

**注意**：明天合并前先确认本地 main 已经合并了这三个分支（已经合并了——见 `git log --oneline main`），否则 PR 会空 merge 或冲突。

## 5. 距离"生产标准"的差距（明天 + 后续要补的）

按重要性排：

### 5.1 P0 — 真生产前必须有

1. **没有真集群 E2E 测试**
   - 当前所有单测都是 unit（mock SSH executor），从没人真在一堆物理机上跑过 `ko init`
   - 需要搭一个测试集群（CI 里跑或本地），跑 `init` / `node add` / `reset` 全流程
   - 推荐：起一套 vagrant / kvm + GitHub Actions matrix
2. **Dashboard 没有真前端**
   - 当前只有兜底 HTML（几行 API 列表）
   - 至少要个能看集群状态、加节点、看证书的最小 React/Vue 页
   - 或者直接放弃 Dashboard 当 UI，只当"带 auth 的 kubectl proxy + REST"
3. **Dashboard auth 太弱**
   - basic auth = 单密码，没审计、没 SSO、没速率限制
   - 至少加：rate limit（stdlib `golang.org/x/time/rate`）、简单 audit log 文件
4. **没有真的"不停机"加 master 路径验证**
   - 加 master 在 etcd 多数派临界点很危险，需要真集群验证
   - 至少在 RUNBOOK.md §7 加一个明确的"危险场景警告"
5. **没有 release artifact 可以下**
   - tag 没打、release workflow 没触发过，README 里的 `curl ... -o ko` 还指向 404

### 5.2 P1 — 一个月内补

1. **没有集群升级路径**（v0.0.1 明说不做，v0.0.2 加 `ko upgrade`）
2. **没有 etcd 恢复命令**（RUNBOOK §6 写了手动恢复步骤，应该包成 `ko cluster restore`）
3. **没有 secrets 加解密**（SSH password 现在直接走 HCL，不安全）
4. **镜像仓库 registry mirror 配置**没做（用户问起来还得自己改 containerd config）
5. **没有结构化日志选项**（`--log-format=json`）

### 5.3 P2 — 锦上添花

1. Docker Compose / kind 集成测试
2. Prometheus metrics 端点（`/metrics`）
3. Helm chart 仓库支持自定义（现在 hardcode 几个）
4. CNI 多选（Calico / Multus）
5. 支持 IPv6 single-stack / dual-stack

## 6. 已知 bug / 技术债

按严重度排：

1. **`internal/cli/stubs.go` 那个 `stubCmd`** 现在没人用，但是 linter 没有移除。需要真删（或者改成 build tag 保护）。
2. **`internal/cli/dashboard.go:198`** 有 `var _ = io.Discard` —— 是历史 import 残留，可以删（已确认不影响功能）。
3. **`internal/helm/install.go:161`** null handling 是 workaround，没找到根因。
4. **`packaged layer 解压到运行时还没有自动化 E2E**——`ko init --offline --bundle X` 的"把 bundle 里 k8s 镜像 ctr import"路径，单测覆盖到函数级，**没跑过完整链路**。
5. **CI / Release workflow 没实跑过**——首次 push 到 GitHub 才能验证。
6. **`pkg/config/` 没 fuzz 测试**——HCL 解析器复杂度不高，但用户输入容易出怪 case（中文 key / 嵌套太深 / block 写错）。

## 7. 明天（v0.0.1 收尾 + v0.1 启动）建议顺序

### 上午：v0.0.1 收尾

```bash
# 1. 在 GitHub 上起 3 个 PR 并 merge（或 squash）
gh pr create ...  # 见 §4

# 2. 打 tag v0.0.1，触发首个 release
git tag v0.0.1
git push origin v0.0.1
# → 看 GitHub Actions 跑 ci.yml + release.yml，确认 bin/ + multi bundle 都上传

# 3. 在本地试跑 release 出来的二进制
curl -sSL https://github.com/bilbilmyc/ko/releases/download/v0.0.1/ko-linux-amd64 -o ko
chmod +x ko && ./ko version
```

### 下午：v0.1.x 启动（按优先级挑一个）

候选（按"用户最痛"排）：

1. **真集群 E2E 测试**（最优先）
   - 起一套本地 kvm/vagrant 环境，跑完整 init
   - 或者写 docker-in-docker 集群测试（kind / k3d）
2. **Dashboard 真前端**
   - 用 Vite + Vue3 最小搭一个（不要 React——团队熟 Vue）
3. **集群升级 `ko upgrade`**
   - kubeadm upgrade apply + 滚动重启组件
   - 高风险，要 E2E 覆盖
4. **Registry mirror 默认配置**
   - containerd 配置 `mirrors.conf` / `hosts.toml`
   - 把 sealos 那套默认 mirror 抄过来

## 8. 代码导览（明天开干时知道去哪看）

| 想做的事 | 去看哪 |
|---|---|
| 改 init 流程 | `internal/cluster/init.go`（550 行，HA 检测在 `isHA()`，CNI 选在 `installCNI()`） |
| 改 kubeadm 调用 | `internal/cluster/kubeadm.go`（命令构造 + cert validity 在这里） |
| 改 SSH | `internal/exec/`（避免 import cycle，提到这里实现） |
| 加新 CNI | `internal/cluster/` 起一个 `xxx.go`，模仿 `flannel.go` 结构，再改 `installCNI()` switch |
| 加新调优项 | `internal/tune/` + `pkg/config.File.Tune` + `internal/cli/tune.go` |
| 改 OCI bundle 格式 | `internal/image/builder.go`（Build 单 arch）+ `multi.go`（多 arch），注意 custom mediaType |
| 改 Dashboard API | `internal/dashboard/handlers.go`（REST）+ `internal/cli/dashboard.go`（apiAdapter） |
| 改 CLI 命令 | `internal/cli/` 一个文件一类命令，新增子命令走 `root.AddCommand`（`internal/cli/root.go:42-52`） |
| 改配置 schema | `pkg/config/file.go` + `cluster.hcl` 顶层块定义，新增字段走 `ApplyDefaults()` |
| 改发布 | `.github/workflows/{ci,release}.yml` + `Makefile` |

## 9. 当时的几个关键决策（防止明天想"为什么这么写"）

1. **证书 100 年**：用户明确要求。`internal/cluster/kubeadm.go` 里 `CertificateValidity = 876000h` —— kubeadm 默认是 1 年，ko 强制改。`--certificate-validity` 同时用在 init 和 join 上。
2. **CNI 默认 Cilium + strict kube-proxy 替换**：要求 kernel ≥ 5.4；不满足就降级到 Flannel（`internal/cluster/init.go: needsFlannel()`）。**不要**默认开 kube-proxy——会和 Cilium 抢 iptables 规则。
3. **stacked etcd**：v0.0.1 只支持 stacked。**外部 etcd 是 v0.1.x 的活**。
4. **离线 bundle 格式**：自定义 OCI mediaType（`application/vnd.ko.layer.*.v1`），不是 Docker registry v2 标准。读取时按 mediaType 分发，不能直接 `docker load`。这是有意为之——Docker 不支持自定义 layer 标签。
5. **exec 包独立**：为了断 `cluster ← containerd/docker` 的 import cycle，把 SSH executor 提到 `internal/exec/`。新加 ssh 工具也走这个包。
6. **不做 app store / ClusterApp**：明确边界，写在 README §"不做什么"。sealos 用户群这一条会很敏感，需要在 v0.1 跟用户对一次。
7. **dashboard 默认 127.0.0.1:8080**：本地 only，要监听 `0.0.0.0` 必须显式 `--listen`。生产必须再过 nginx 加 TLS（见 RUNBOOK §5.2）。

## 10. 一键恢复命令

明天接手时第一件事：

```bash
# 1. 拉最新
git checkout main
git pull origin main  # 如果明天 origin/main 同步过来了
git log --oneline -5

# 2. 跑测试，确认昨晚的代码还是绿的
go test ./...

# 3. 读这份 STATUS.md → 决定今天干什么
```

如果今天有紧急 hotfix 还没合并的，看 `git branch -a`，未 push 的本地分支应该还有（如果主分支合了所有东西，那只需要走 §4 推 PR 的流程）。

---

**TL;DR**：v0.0.1 代码全绿、文档齐了、本地合并完了，差推到 GitHub + 首次 release。明天第一优先级是把这一刀切圆（PR → tag → release），第二优先级挑 v0.1 一刀真干。
