package cluster

import (
	"context"
	"fmt"
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