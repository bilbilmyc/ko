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
)

// Teardown wipes cluster state from every host so a fresh `ko init` can run
// cleanly. It is idempotent — running it on a never-initialised host is a
// no-op. After Teardown, /etc/kubernetes/*, /var/lib/kubelet, /var/lib/etcd,
// and iptables/ipvs rules are gone on every host.
type Teardown struct {
	Exec Executor
	CRI  string // default "unix:///run/containerd/containerd.sock"
	// RestoreAPITimeout bounds the apiserver /healthz probe in
	// RestoreStackedEtcd. Zero means the default (90s).
	RestoreAPITimeout time.Duration
}

func NewTeardown(exec Executor) *Teardown {
	return &Teardown{Exec: exec, CRI: "unix:///run/containerd/containerd.sock"}
}

// ResetAll runs reset on every host. Order: workers first (so masters keep
// quorum), then masters.
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

// resetHost runs kubeadm reset + cleanup on a single host.
func (t *Teardown) resetHost(ctx context.Context, host string) error {
	logger.Info("resetting host", "host", host)
	script := fmt.Sprintf(`set -euo pipefail
kubeadm reset --force --cri-socket=%s 2>&1 || true
rm -rf /etc/kubernetes /var/lib/kubelet /var/lib/etcd /var/lib/kube-vip 2>/dev/null || true
iptables --flush 2>/dev/null || true
iptables -t nat --flush 2>/dev/null || true
ipvsadm --clear 2>/dev/null || true
rm -f /var/run/kubeadm-config || true
`, t.CRI)
	res := t.Exec.Run(ctx, host, script)
	if res.Failed() {
		return fmt.Errorf("reset: %w", res.Err)
	}
	return nil
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