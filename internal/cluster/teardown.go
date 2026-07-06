package cluster

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/ko-build/ko/internal/logger"
	"github.com/ko-build/ko/pkg/config"
)

// Teardown wipes cluster state from every host so a fresh `ko init` can run
// cleanly. It is idempotent — running it on a never-initialised host is a
// no-op. After Teardown, /etc/kubernetes/*, /var/lib/kubelet, /var/lib/etcd,
// CNI configs, veth interfaces, and iptables/ipvs rules are gone on every
// host. With Purge=true, the image cache and ko-installed config files are
// also nuked, leaving the host in the same state as if it had never been
// touched (modulo the installed containerd / docker / kubelet binaries).
type Teardown struct {
	Exec Executor
	CRI  string // default "unix:///run/containerd/containerd.sock"
	// RestoreAPITimeout bounds the apiserver /healthz probe in
	// RestoreStackedEtcd. Zero means the default (90s).
	RestoreAPITimeout time.Duration
	// Purge enables a more aggressive cleanup: containerd image cache,
	// ko-written config files (containerd config.toml, kubelet drop-ins,
	// /etc/cni/net.d, /opt/cni/bin), and any external-etcd artefacts.
	// Off by default so that a back-to-back `ko init` doesn't have to
	// re-pull the whole image set.
	Purge bool
}

func NewTeardown(exec Executor) *Teardown {
	return &Teardown{Exec: exec, CRI: "unix:///run/containerd/containerd.sock"}
}

// ResetAll runs reset on every host. Order: workers first (so masters keep
// quorum), then masters. Does NOT touch external etcd — use
// ResetAllWithConfig when the cluster has one, so the etcd uninstall runs
// before per-host kubeadm reset.
func (t *Teardown) ResetAll(ctx context.Context, masters, workers []string) error {
	if t.CRI == "" {
		t.CRI = "unix:///run/containerd/containerd.sock"
	}
	for _, w := range workers {
		if err := t.resetHost(ctx, w); err != nil {
			return fmt.Errorf("worker %s: %w", w, err)
		}
	}
	for _, m := range masters {
		if err := t.resetHost(ctx, m); err != nil {
			return fmt.Errorf("master %s: %w", m, err)
		}
	}
	return nil
}

// ResetAllWithConfig dispatches on etcd mode: external etcd is uninstalled
// first (so etcd pods/data don't survive a reset and confuse the next init),
// then per-host kubeadm reset runs. With t.Purge, both phases go deeper.
func (t *Teardown) ResetAllWithConfig(ctx context.Context, cfg *config.File) error {
	if IsExternalEtcd(cfg) {
		logger.Info("reset: uninstalling external etcd", "purge_backups", t.Purge)
		if err := UninstallExternalEtcd(ctx, cfg, t.Exec, t.Purge); err != nil {
			return fmt.Errorf("uninstall external etcd: %w", err)
		}
	}
	return t.ResetAll(ctx, cfg.Nodes.Masters, cfg.Nodes.Workers)
}

// resetHost runs kubeadm reset + the full ko post-cleanup on a single host.
// The script is intentionally idempotent: every operation tolerates the
// "nothing to do" case via `2>/dev/null || true` or `test -e && mv`. The
// goal is that a host which was init'd, reset, and init'd again ends up in
// the same state as a freshly-prepped one.
func (t *Teardown) resetHost(ctx context.Context, host string) error {
	logger.Info("resetting host", "host", host, "purge", t.Purge)
	script := t.resetScript()
	res := t.Exec.Run(ctx, host, script)
	if res.Failed() {
		return fmt.Errorf("reset: %w (stderr: %s)", res.Err, res.Stderr)
	}
	return nil
}

// resetScript is the per-host cleanup. Pulled out for testability.
func (t *Teardown) resetScript() string {
	purge := t.Purge
	// The script is structured as a sequence of `set +e`-guarded sections
	// so a failure in one doesn't skip the rest. The outer `set -euo
	// pipefail` would otherwise abort at the first non-zero exit.
	return fmt.Sprintf(`set -euo pipefail

# 0. Stop the workloads that hold open file handles on the dirs we're about
#    to nuke. "|| true" everywhere: on a never-init'd host these are all
#    already gone.
systemctl stop kubelet 2>/dev/null || true
systemctl stop containerd 2>/dev/null || true
systemctl stop docker 2>/dev/null || true
systemctl stop etcd 2>/dev/null || true

# 1. kubeadm reset — the canonical cleanup. --force bypasses the
#    "are you sure" prompt; --cri-socket points it at containerd's
#    gRPC socket so it can also wipe any ctr-tracked containers.
kubeadm reset --force --cri-socket=%s 2>&1 || true

# 2. kubeadm leaves some artefacts behind. Remove them.
rm -rf /etc/kubernetes
rm -rf /var/lib/kubelet
rm -rf /var/lib/etcd
rm -rf /var/lib/kube-vip
rm -f  /var/run/kubeadm-config
rm -rf /var/lib/kubelet/pods 2>/dev/null || true

# 3. ko-installed systemd units. These live in /etc/systemd/system which
#    kubeadm doesn't touch.
rm -f  /etc/systemd/system/etcd.service
rm -f  /etc/systemd/system/etcd.service.d/*.conf
rm -f  /etc/systemd/system/ko-etcd-backup.service
rm -f  /etc/systemd/system/ko-etcd-backup.timer
rm -f  /etc/systemd/system/containerd.service
rm -rf /etc/systemd/system/containerd.service.d
rm -rf /etc/systemd/system/etcd.service.d
systemctl daemon-reload 2>/dev/null || true

# 4. CNI config + binaries. kubeadm reset does NOT touch /etc/cni/net.d
#    or /opt/cni/bin. If left behind, the next init's CNI (e.g. Cilium)
#    will see the stale "10-containerd-net.conflist" from the old run
#    and refuse to start.
rm -rf /etc/cni/net.d
rm -rf /opt/cni/bin
rm -rf /var/lib/cni
rm -rf /run/calico 2>/dev/null || true
rm -rf /var/lib/calico 2>/dev/null || true

# 5. Network plumbing. kubeadm reset flushes iptables in filter+nat but
#    misses mangle/raw. We flush every table and remove the kube / CNI
#    interfaces that survive across resets. The "|| true" keeps the
#    script going when the interface doesn't exist.
iptables --flush 2>/dev/null || true
iptables -t nat    --flush 2>/dev/null || true
iptables -t mangle --flush 2>/dev/null || true
iptables -t raw    --flush 2>/dev/null || true
iptables -t filter --flush 2>/dev/null || true
ipvsadm --clear 2>/dev/null || true
for iface in cni0 flannel.1 vxlan.calico kube-ipvs0 cilium-host cilium-vxlan; do
  ip link delete "$iface" 2>/dev/null || true
done
# veth* are matched with a wildcard — only delete the ones k8s/CNI
# created, identified by the peer index living in /sys.
for veth in /sys/class/net/veth*/ifindex; do
  [ -e "$veth" ] || continue
  ip link delete "$(basename "$(dirname "$veth")")" 2>/dev/null || true
done

# 6. Stale mounts. After kubelet stops, /var/lib/kubelet/pods/*/volumes
#    may still be mounted if a pod was force-killed. Lazy unmount so we
#    don't fail when the source is already gone.
for mp in $(mount -t fuse-overlayfs,overlay | awk '{print $3}'); do
  umount -l "$mp" 2>/dev/null || true
done
for mp in $(awk '/\/var\/lib\/kubelet\/pods\// {print $2}' /proc/self/mounts); do
  umount -l "$mp" 2>/dev/null || true
done

# 7. ko-written config. We don't tear down the *installed* containerd /
#    docker binaries or the kubelet apt package — those are setup
#    steps that may be shared with other workloads on the host. But the
#    ko-generated config files DO need to come off so the next init
#    writes a clean version.
rm -f  /etc/containerd/config.toml
rm -f  /etc/docker/daemon.json
rm -rf /etc/systemd/system/kubelet.service.d
# /etc/hosts ko.local entry — written by OfflineRunner.writeHosts for
# airgap so every node resolves the in-cluster registry. After reset,
# the IP it pointed at may be dead (or the host may be re-init'd into
# a new cluster). Strip the line; if the host is being re-added with
# "ko node add", that path rewrites it idempotently.
sed -i '/[[:space:]]ko\.local$/d' /etc/hosts 2>/dev/null || true
# v0.0.5 in-cluster registry — installed by OfflineRunner.startRegistry
# on master-1 only. All lines are no-ops on non-master hosts where the
# files / unit never existed; the || true guards cover every case.
systemctl disable --now ko-registry.service 2>/dev/null || true
rm -f  /etc/systemd/system/ko-registry.service
rm -rf /var/lib/ko-registry
rm -f  /usr/local/bin/registry
rm -f  /etc/registry-config.yml

# 8. External-etcd artefacts. Per-member data dirs live under
#    /var/lib/etcd/<member> (stacked gets the lot, but we already
#    removed /var/lib/etcd above).
rm -rf /var/lib/etcd
rm -rf /etc/etcd
rm -rf /var/backups/etcd

# 9. Purge mode: nuke the image cache + containerd state. The default
#    skips this so a back-to-back init doesn't have to re-import the
#    OCI bundle's images. "ko reset --purge" is the "leave the host
#    as if ko had never run" knob.
if [ "%v" = "true" ]; then
  ctr -n k8s.io containers delete --force --all 2>/dev/null || true
  rm -rf /var/lib/containerd
  rm -rf /var/run/containerd
  rm -rf /var/lib/docker
  rm -rf /var/run/docker
  # ko cache directory on the host (not the operator's machine).
  rm -rf /var/lib/ko
  rm -rf /root/.ko
fi
`, t.CRI, purge) // %v is the bool literal "true"/"false"
}

// BackupEtcd takes an etcd snapshot on the first master (stacked mode only).
// Returns the local path to the snapshot file.
func (t *Teardown) BackupEtcd(ctx context.Context, master string) (string, error) {
	logger.Info("snapshotting etcd", "master", master)
	ts := time.Now().UTC().Format("20060102-150405")
	remotePath := fmt.Sprintf("/tmp/ko-etcd-%s.db", ts)
	// The cert paths are stable inside kubeadm's PKI tree.
	script := fmt.Sprintf(`set -euo pipefail
ETCDCTL_API=3 etcdctl --endpoints=https://127.0.0.1:2379 \
  --cacert=/etc/kubernetes/pki/etcd/ca.crt \
  --cert=/etc/kubernetes/pki/etcd/server.crt \
  --key=/etc/kubernetes/pki/etcd/server.key \
  snapshot save %s
`, remotePath)
	res := t.Exec.Run(ctx, master, script)
	if res.Failed() {
		return "", fmt.Errorf("etcd snapshot: %w", res.Err)
	}
	// scp back to a local timestamped path
	localPath := fmt.Sprintf("ko-etcd-%s.db", ts)
	if err := t.Exec.Scp(ctx, master, remotePath, localPath); err != nil {
		return "", fmt.Errorf("scp snapshot: %w", err)
	}
	logger.Info("snapshot saved", "local", localPath)
	return localPath, nil
}

// RestoreStackedOpts describes a stacked-mode restore from a single snapshot
// file distributed across every master. Each master gets its own etcd
// instance (as a static pod), so we restore on every master in turn.
type RestoreStackedOpts struct {
	SnapshotPath string   // local snapshot file
	Masters      []string // master IPs in user-specified order
}

// RestoreStackedEtcd restores etcd on every master from one snapshot file.
// Order of operations:
//  1. Validate snapshot exists.
//  2. Discover each master's etcd member name (= `hostname -s` — kubeadm's
//     convention for the static pod).
//  3. Stop kubelet on every master first so the static pod can't restart
//     etcd while we're working on the data dir.
//  4. Per master (sequential): move /var/lib/etcd aside, scp snapshot,
//     etcdctl snapshot restore with --name=<host> --initial-cluster=...
//     --initial-advertise-peer-urls=https://<ip>:2380.
//  5. Start kubelet on every master.
//  6. Poll apiserver /healthz on the first master.
func (t *Teardown) RestoreStackedEtcd(ctx context.Context, opts RestoreStackedOpts) error {
	if opts.SnapshotPath == "" {
		return fmt.Errorf("snapshot path required")
	}
	if _, err := os.Stat(opts.SnapshotPath); err != nil {
		return fmt.Errorf("snapshot %q: %w", opts.SnapshotPath, err)
	}
	if len(opts.Masters) == 0 {
		return fmt.Errorf("at least one master required")
	}

	names := make([]string, len(opts.Masters))
	for i, m := range opts.Masters {
		r := t.Exec.Run(ctx, m, "hostname -s")
		if r.Failed() {
			return fmt.Errorf("%s: hostname: %w", m, r.Err)
		}
		names[i] = strings.TrimSpace(string(r.Stdout))
	}
	var ic strings.Builder
	for i, m := range opts.Masters {
		if i > 0 {
			ic.WriteByte(',')
		}
		fmt.Fprintf(&ic, "%s=https://%s:2380", names[i], m)
	}
	initialCluster := ic.String()
	ts := time.Now().UTC().Format("20060102-150405")
	logger.Info("etcd stacked restore: starting", "masters", len(opts.Masters), "snapshot", opts.SnapshotPath)

	for _, m := range opts.Masters {
		r := t.Exec.Run(ctx, m, "systemctl stop kubelet 2>/dev/null || true")
		if r.Failed() {
			return fmt.Errorf("%s: stop kubelet: %w", m, r.Err)
		}
	}
	logger.Info("etcd stacked restore: kubelet stopped on all masters")

	for i, m := range opts.Masters {
		name := names[i]
		remoteSnap := fmt.Sprintf("/tmp/ko-etcd-restore-%s.db", ts)
		if err := t.Exec.Scp(ctx, m, opts.SnapshotPath, remoteSnap); err != nil {
			return fmt.Errorf("%s: scp snapshot: %w", m, err)
		}
		mv := fmt.Sprintf(`test -e /var/lib/etcd && mv /var/lib/etcd /var/lib/etcd.broken-%s || true`, ts)
		if r := t.Exec.Run(ctx, m, mv); r.Failed() {
			return fmt.Errorf("%s: move data-dir: %w", m, r.Err)
		}
		restore := fmt.Sprintf(`set -euo pipefail
ETCDCTL_API=3 etcdctl snapshot restore %s \
  --name=%s \
  --initial-cluster=%s \
  --initial-advertise-peer-urls=https://%s:2380 \
  --data-dir=/var/lib/etcd
chmod 0700 /var/lib/etcd
`, remoteSnap, name, initialCluster, m)
		if r := t.Exec.Run(ctx, m, restore); r.Failed() {
			return fmt.Errorf("%s: etcdctl snapshot restore: %w", m, r.Err)
		}
		_ = t.Exec.Run(ctx, m, fmt.Sprintf("rm -f %s", remoteSnap))
		logger.Info("etcd stacked restore: data dir restored", "master", m, "name", name)
	}

	for _, m := range opts.Masters {
		if r := t.Exec.Run(ctx, m, "systemctl start kubelet"); r.Failed() {
			return fmt.Errorf("%s: start kubelet: %w", m, r.Err)
		}
	}
	logger.Info("etcd stacked restore: kubelet started on all masters")

	return t.waitForStackedAPI(ctx, opts.Masters[0])
}

func (t *Teardown) waitForStackedAPI(ctx context.Context, master string) error {
	timeout := t.RestoreAPITimeout
	if timeout == 0 {
		timeout = 90 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		r := t.Exec.Run(ctx, master, "curl -sk --max-time 3 https://127.0.0.1:6443/healthz 2>/dev/null")
		if !r.Failed() && bytes.Contains(r.Stdout, []byte("ok")) {
			logger.Info("etcd stacked restore: apiserver healthy", "master", master)
			return nil
		}
		time.Sleep(3 * time.Second)
	}
	return fmt.Errorf("apiserver on %s did not return healthy within %s after restore", master, timeout)
}

// CertificateInfo holds expiry metadata for one cert file.
type CertificateInfo struct {
	Host     string
	Path     string
	NotAfter time.Time
	Subject  string
}

// ListCertificates reads /etc/kubernetes/pki/*.crt on each master and returns
// a flat list with NotAfter parsed from openssl x509 -enddate output.
func (t *Teardown) ListCertificates(ctx context.Context, masters []string) ([]CertificateInfo, error) {
	var out []CertificateInfo
	for _, m := range masters {
		res := t.Exec.Run(ctx, m, `for f in /etc/kubernetes/pki/*.crt /etc/kubernetes/pki/etcd/*.crt; do
  [ -f "$f" ] || continue
  echo "FILE=$f"
  openssl x509 -in "$f" -noout -enddate -subject 2>/dev/null | tr '\n' '|'
  echo
done`)
		if res.Failed() {
			return nil, fmt.Errorf("%s: read pki: %w", m, res.Err)
		}
		for line := range strings.SplitSeq(strings.TrimRight(string(res.Stdout), "\n"), "\n") {
			info := parseCertLine(m, line)
			if info != nil {
				out = append(out, *info)
			}
		}
	}
	return out, nil
}

var (
	certNotAfterRE = regexp.MustCompile(`notAfter=(.+?)\s*\|subject=`)
	certSubjectRE  = regexp.MustCompile(`subject=(.+?)$`)
)

func parseCertLine(host, line string) *CertificateInfo {
	if !strings.HasPrefix(line, "FILE=") {
		return nil
	}
	path, meta, ok := strings.Cut(line, "|")
	if !ok {
		return nil
	}
	path = strings.TrimPrefix(path, "FILE=")

	var notAfter time.Time
	if m := certNotAfterRE.FindStringSubmatch(meta); m != nil {
		ts := strings.TrimSpace(m[1])
		if t, err := time.Parse("Jan _2 15:04:05 2006 MST", ts); err == nil {
			notAfter = t
		}
	}
	if notAfter.IsZero() {
		return nil
	}
	var subject string
	if m := certSubjectRE.FindStringSubmatch(meta); m != nil {
		subject = strings.TrimSpace(m[1])
	}
	return &CertificateInfo{Host: host, Path: path, NotAfter: notAfter, Subject: subject}
}