package etcd

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"text/template"

	"github.com/ko-build/ko/internal/exec"
	"github.com/ko-build/ko/internal/logger"
)

// Service is the etcd systemd manager. One instance per cluster; methods
// take a per-host view (the unit file content varies per member).
type Service struct {
	Exec        exec.Executor
	TarballPath string       // local path to etcd-v*-linux-*.tar.gz (from Download)
	Version     string       // "v3.5.21"
	Cluster     ClusterConfig
	BinaryPath  string // remote path to install etcd binary (default /usr/local/bin/etcd)
	DataRoot    string // remote data root (default /var/lib/etcd)
}

// Executor is the subset of exec.Executor this package needs. Lets us
// unit-test with a mock without depending on the cluster package.
type Executor = exec.Executor

// ClusterConfig drives the unit-file rendering. The Members slice is the
// authoritative cluster topology — InitialCluster is computed from it.
type ClusterConfig struct {
	Members       []Member
	ClusterToken  string
	PKIDir        string // default /etc/etcd/pki
	InitialState  string // "new" (default) or "existing"
	ExtraArgs     []string
}

type Member struct {
	Name string
	Host string
	// Optional overrides — if empty, defaults are derived from Host.
	ListenClientURLs  []string
	AdvertiseClientURL string
	ListenPeerURLs    []string
	InitialPeerURLs   string
	DataDir           string
}

// NewService returns a Service with sensible defaults filled in for any
// blank fields. ClusterToken defaults to "ko-etcd-cluster" so all
// `ko` deployments on the same network don't accidentally cluster.
func NewService(ex exec.Executor, tarball, version string, cc ClusterConfig) *Service {
	if cc.ClusterToken == "" {
		cc.ClusterToken = "ko-etcd-cluster"
	}
	if cc.InitialState == "" {
		cc.InitialState = "new"
	}
	if cc.PKIDir == "" {
		cc.PKIDir = "/etc/etcd/pki"
	}
	for i := range cc.Members {
		m := &cc.Members[i]
		if len(m.ListenClientURLs) == 0 {
			m.ListenClientURLs = []string{
				fmt.Sprintf("https://%s:2379", m.Host),
				"https://127.0.0.1:2379",
			}
		}
		if m.AdvertiseClientURL == "" {
			m.AdvertiseClientURL = fmt.Sprintf("https://%s:2379", m.Host)
		}
		if len(m.ListenPeerURLs) == 0 {
			m.ListenPeerURLs = []string{fmt.Sprintf("https://%s:2380", m.Host)}
		}
		if m.InitialPeerURLs == "" {
			m.InitialPeerURLs = fmt.Sprintf("https://%s:2380", m.Host)
		}
		if m.DataDir == "" {
			m.DataDir = "/var/lib/etcd/" + m.Name
		}
	}
	return &Service{
		Exec:        ex,
		TarballPath: tarball,
		Version:     version,
		Cluster:     cc,
		BinaryPath:  "/usr/local/bin/etcd",
		DataRoot:    "/var/lib/etcd",
	}
}

// initialClusterList builds the --initial-cluster string. Members are
// sorted by name to keep the list deterministic.
func (s *Service) initialClusterList() string {
	members := append([]Member(nil), s.Cluster.Members...)
	sort.Slice(members, func(i, j int) bool { return members[i].Name < members[j].Name })
	var b bytes.Buffer
	for i, m := range members {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%s=%s", m.Name, m.InitialPeerURLs)
	}
	return b.String()
}

// RenderUnit returns the systemd unit file for one member.
func (s *Service) RenderUnit(m Member) (string, error) {
	tpl := `[Unit]
Description=etcd (external, managed by ko)
Documentation=https://etcd.io/docs
After=network-online.target local-fs.target
Wants=network-online.target

[Service]
Type=notify
ExecStart={{.Binary}} \
  --name={{.Name}} \
  --data-dir={{.DataDir}} \
  --listen-client-urls={{.ListenClientURLs}} \
  --advertise-client-urls={{.AdvertiseClientURL}} \
  --listen-peer-urls={{.ListenPeerURLs}} \
  --initial-advertise-peer-urls={{.InitialPeerURL}} \
  --initial-cluster={{.InitialCluster}} \
  --initial-cluster-token={{.Token}} \
  --initial-cluster-state={{.State}} \
  --trusted-ca-file={{.PKIDir}}/ca.crt \
  --cert-file={{.PKIDir}}/server.crt \
  --key-file={{.PKIDir}}/server.key \
  --peer-trusted-ca-file={{.PKIDir}}/ca.crt \
  --peer-cert-file={{.PKIDir}}/peer.crt \
  --peer-key-file={{.PKIDir}}/peer.key
Restart=on-failure
RestartSec=5
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
`
	data := map[string]string{
		"Binary":             s.BinaryPath,
		"Name":               m.Name,
		"DataDir":            m.DataDir,
		"ListenClientURLs":   joinCSV(m.ListenClientURLs),
		"AdvertiseClientURL": m.AdvertiseClientURL,
		"ListenPeerURLs":     joinCSV(m.ListenPeerURLs),
		"InitialPeerURL":     m.InitialPeerURLs,
		"InitialCluster":     s.initialClusterList(),
		"Token":              s.Cluster.ClusterToken,
		"State":              s.Cluster.InitialState,
		"PKIDir":             s.Cluster.PKIDir,
	}
	t, err := template.New("etcd-unit").Parse(tpl)
	if err != nil {
		return "", fmt.Errorf("parse unit template: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("render unit: %w", err)
	}
	return buf.String(), nil
}

// Install lays down the binary + PKI + unit on a single host and starts
// the service. Idempotent: if the binary already exists at the target
// version, the upload is skipped.
func (s *Service) Install(ctx context.Context, member Member, paths *CertPaths) error {
	host := member.Host
	logger.Info("etcd install: starting", "host", host, "name", member.Name)

	// 1. Push binaries (scp the tarball, then extract on the host).
	if err := s.installBinary(ctx, host); err != nil {
		return fmt.Errorf("install binary: %w", err)
	}

	// 2. Lay out the PKI on the host.
	if err := s.installPKI(ctx, host, paths); err != nil {
		return fmt.Errorf("install pki: %w", err)
	}

	// 3. Create data dir.
	res := s.Exec.Run(ctx, host, fmt.Sprintf("mkdir -p %s", member.DataDir))
	if res.Failed() {
		return fmt.Errorf("mkdir data-dir: %w", res.Err)
	}

	// 4. Write the unit file.
	unit, err := s.RenderUnit(member)
	if err != nil {
		return err
	}
	tmp, err := writeTempFile("etcd.service", unit)
	if err != nil {
		return fmt.Errorf("stage unit: %w", err)
	}
	defer os.Remove(tmp)
	if err := s.Exec.Scp(ctx, host, tmp, "/etc/systemd/system/etcd.service"); err != nil {
		return fmt.Errorf("scp unit: %w", err)
	}

	// 5. Reload + enable + start.
	cmds := []string{
		"systemctl daemon-reload",
		"systemctl enable etcd.service",
		"systemctl restart etcd.service || systemctl start etcd.service",
	}
	for _, c := range cmds {
		r := s.Exec.Run(ctx, host, c)
		if r.Failed() {
			return fmt.Errorf("%q: %w", c, r.Err)
		}
	}
	logger.Info("etcd install: done", "host", host, "name", member.Name)
	return nil
}

// installBinary scps the local tarball, extracts etcd + etcdctl, and
// installs them into /usr/local/bin.
func (s *Service) installBinary(ctx context.Context, host string) error {
	// Detect existing version
	res := s.Exec.Run(ctx, host, fmt.Sprintf("%s --version 2>/dev/null | head -1", s.BinaryPath))
	if !res.Failed() && bytes.Contains(res.Stdout, []byte(s.Version)) {
		logger.Info("etcd binary already installed at target version", "host", host, "version", s.Version)
		return nil
	}

	remoteTar := "/tmp/ko-etcd.tar.gz"
	if err := s.Exec.Scp(ctx, host, s.TarballPath, remoteTar); err != nil {
		return fmt.Errorf("scp tarball: %w", err)
	}
	script := fmt.Sprintf(`set -euo pipefail
cd /tmp
tar -xzf %s
install -m 0755 etcd-v*-linux-*/etcd    /usr/local/bin/etcd
install -m 0755 etcd-v*-linux-*/etcdctl /usr/local/bin/etcdctl
rm -rf etcd-v*-linux-*
rm -f %s
/usr/local/bin/etcd --version | head -1
`, remoteTar, remoteTar)
	r := s.Exec.Run(ctx, host, script)
	if r.Failed() {
		return fmt.Errorf("extract + install: %w", r.Err)
	}
	return nil
}

// installPKI rsyncs the certs onto the host under PKIDir.
func (s *Service) installPKI(ctx context.Context, host string, paths *CertPaths) error {
	mkdir := fmt.Sprintf("mkdir -p %s", s.Cluster.PKIDir)
	if r := s.Exec.Run(ctx, host, mkdir); r.Failed() {
		return fmt.Errorf("mkdir pki: %w", r.Err)
	}
	dir := s.Cluster.PKIDir
	pairs := []struct{ src, dst string }{
		{paths.CA, dir + "/ca.crt"},
		{paths.CAKey, dir + "/ca.key"},
		{paths.Server, dir + "/server.crt"},
		{filepath.Join(paths.Dir, "server.key"), dir + "/server.key"},
		{paths.Peer, dir + "/peer.crt"},
		{filepath.Join(paths.Dir, "peer.key"), dir + "/peer.key"},
		{paths.Client, dir + "/client.crt"},
		{filepath.Join(paths.Dir, "client.key"), dir + "/client.key"},
	}
	for _, p := range pairs {
		if err := s.Exec.Scp(ctx, host, p.src, p.dst); err != nil {
			return fmt.Errorf("scp %s -> %s: %w", p.src, p.dst, err)
		}
	}
	// Tighten perms
	perm := fmt.Sprintf("chmod 0644 %s/*.crt && chmod 0600 %s/*.key", dir, dir)
	if r := s.Exec.Run(ctx, host, perm); r.Failed() {
		return fmt.Errorf("chmod pki: %w", r.Err)
	}
	return nil
}

// Uninstall stops + disables the unit and removes PKI/data. Leaves
// /usr/local/bin/etcd in place (caller can `apt purge` or rm manually).
func (s *Service) Uninstall(ctx context.Context, member Member) error {
	host := member.Host
	cmds := []string{
		"systemctl disable --now etcd.service 2>/dev/null || true",
		fmt.Sprintf("rm -f /etc/systemd/system/etcd.service /etc/systemd/system/etcd.service.d/*.conf"),
		fmt.Sprintf("rm -rf %s", s.Cluster.PKIDir),
		fmt.Sprintf("rm -rf %s", member.DataDir),
		"systemctl daemon-reload",
		"systemctl reset-failed etcd.service 2>/dev/null || true",
	}
	for _, c := range cmds {
		r := s.Exec.Run(ctx, host, c)
		if r.Failed() {
			return fmt.Errorf("%q: %w", c, r.Err)
		}
	}
	return nil
}

// Status reports the service state on a single member.
type MemberStatus struct {
	Host      string
	Name      string
	Active    string // "active" | "inactive" | "failed"
	EndpointHealth string // "healthy" | "unhealthy" | "unknown"
	Leader    bool
}

// Status queries the systemd state of etcd.service on each member and
// reports back. It also probes the client endpoint via /health.
func (s *Service) Status(ctx context.Context, members []Member) ([]MemberStatus, error) {
	var out []MemberStatus
	for _, m := range members {
		st := MemberStatus{Host: m.Host, Name: m.Name, Active: "unknown", EndpointHealth: "unknown"}
		r := s.Exec.Run(ctx, m.Host, "systemctl is-active etcd.service 2>/dev/null || true")
		if !r.Failed() {
			st.Active = string(bytes.TrimSpace(r.Stdout))
		}
		// /health is a JSON endpoint — curl exits 0 on healthy, non-zero otherwise
		r = s.Exec.Run(ctx, m.Host,
			fmt.Sprintf("curl -sk --max-time 3 --cacert %s/ca.crt --cert %s/client.crt --key %s/client.key https://%s:2379/health 2>/dev/null",
				s.Cluster.PKIDir, s.Cluster.PKIDir, s.Cluster.PKIDir, m.Host))
		if !r.Failed() && bytes.Contains(r.Stdout, []byte(`"health":"true"`)) {
			st.EndpointHealth = "healthy"
		} else {
			st.EndpointHealth = "unhealthy"
		}
		out = append(out, st)
	}
	return out, nil
}

// joinCSV joins strings with commas (no quoting — used for URL lists).
func joinCSV(s []string) string {
	var b bytes.Buffer
	for i, v := range s {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(v)
	}
	return b.String()
}

// writeTempFile atomically writes content to a temp file under /tmp and
// returns the path. Caller is responsible for Remove.
func writeTempFile(name, content string) (string, error) {
	f, err := os.CreateTemp("", "ko-"+name+"-*.tmp")
	if err != nil {
		return "", err
	}
	path := f.Name()
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}
