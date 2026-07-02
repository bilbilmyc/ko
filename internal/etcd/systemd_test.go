package etcd

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ko-build/ko/internal/exec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockExecutor is a tiny test double that records commands and serves
// canned responses keyed by the command's first token.
type mockExecutor struct {
	RunFn      func(ctx context.Context, host, command string) exec.Result
	ScpFn      func(ctx context.Context, host, src, dst string) error
	closed     bool
	history    []string
	scpHistory []scpCall
}

type scpCall struct {
	Src string
	Dst string
}

func (m *mockExecutor) Run(_ context.Context, host, command string) exec.Result {
	m.history = append(m.history, command)
	if m.RunFn != nil {
		return m.RunFn(context.Background(), host, command)
	}
	return exec.Result{}
}

func (m *mockExecutor) Scp(_ context.Context, host, src, dst string) error {
	m.scpHistory = append(m.scpHistory, scpCall{Src: src, Dst: dst})
	if m.ScpFn != nil {
		return m.ScpFn(context.Background(), host, src, dst)
	}
	return nil
}

func (m *mockExecutor) Close() error { m.closed = true; return nil }

func TestService_RenderUnit_HasAllFlags(t *testing.T) {
	cc := ClusterConfig{
		Members: []Member{
			{Name: "etcd-1", Host: "10.0.0.31"},
			{Name: "etcd-2", Host: "10.0.0.32"},
			{Name: "etcd-3", Host: "10.0.0.33"},
		},
		ClusterToken: "tok",
		PKIDir:       "/etc/etcd/pki",
	}
	svc := NewService(&mockExecutor{}, "/tmp/etcd.tar.gz", "v3.5.21", cc)
	unit, err := svc.RenderUnit(cc.Members[0])
	require.NoError(t, err)
	for _, want := range []string{
		"--name=etcd-1",
		"--data-dir=/var/lib/etcd/etcd-1",
		"--listen-client-urls=https://10.0.0.31:2379,https://127.0.0.1:2379",
		"--advertise-client-urls=https://10.0.0.31:2379",
		"--listen-peer-urls=https://10.0.0.31:2380",
		"--initial-advertise-peer-urls=https://10.0.0.31:2380",
		"--initial-cluster=etcd-1=https://10.0.0.31:2380,etcd-2=https://10.0.0.32:2380,etcd-3=https://10.0.0.33:2380",
		"--initial-cluster-token=tok",
		"--initial-cluster-state=new",
		"/etc/etcd/pki/ca.crt",
		"Type=notify",
		"Restart=on-failure",
		"WantedBy=multi-user.target",
	} {
		assert.Contains(t, unit, want, "missing flag in unit: %q", want)
	}
}

func TestService_RenderUnit_InitialClusterSorted(t *testing.T) {
	// Members out of order — output must still be alphabetical so that
	// the unit file is reproducible across hosts.
	cc := ClusterConfig{
		Members: []Member{
			{Name: "etcd-c", Host: "10.0.0.33"},
			{Name: "etcd-a", Host: "10.0.0.31"},
			{Name: "etcd-b", Host: "10.0.0.32"},
		},
		PKIDir: "/etc/etcd/pki",
	}
	svc := NewService(&mockExecutor{}, "", "v3.5.21", cc)
	unit, err := svc.RenderUnit(cc.Members[0])
	require.NoError(t, err)
	assert.Contains(t, unit, "etcd-a=https", "initial-cluster should be sorted alphabetically")
	for _, m := range []string{"etcd-a=", "etcd-b=", "etcd-c="} {
		assert.Contains(t, unit, m)
	}
}

func TestService_Defaults(t *testing.T) {
	cc := ClusterConfig{
		Members: []Member{
			{Name: "etcd-1", Host: "10.0.0.31"},
		},
	}
	svc := NewService(&mockExecutor{}, "", "v3.5.21", cc)
	assert.Equal(t, "ko-etcd-cluster", svc.Cluster.ClusterToken)
	assert.Equal(t, "new", svc.Cluster.InitialState)
	assert.Equal(t, "/etc/etcd/pki", svc.Cluster.PKIDir)
	m := svc.Cluster.Members[0]
	assert.Equal(t, []string{"https://10.0.0.31:2379", "https://127.0.0.1:2379"}, m.ListenClientURLs)
	assert.Equal(t, "https://10.0.0.31:2380", m.InitialPeerURLs)
	assert.Equal(t, "/var/lib/etcd/etcd-1", m.DataDir)
}

func TestService_Status_ActiveAndHealth(t *testing.T) {
	m := &mockExecutor{
		RunFn: func(_ context.Context, host, command string) exec.Result {
			if strings.HasPrefix(command, "systemctl is-active") {
				if host == "10.0.0.31" {
					return exec.Result{Stdout: []byte("active\n")}
				}
				return exec.Result{Stdout: []byte("inactive\n")}
			}
			if strings.HasPrefix(command, "curl") {
				if host == "10.0.0.31" {
					return exec.Result{Stdout: []byte(`{"health":"true"}`)}
				}
				return exec.Result{Stdout: []byte(`{"health":"false"}`)}
			}
			return exec.Result{}
		},
	}
	cc := ClusterConfig{
		Members: []Member{
			{Name: "etcd-1", Host: "10.0.0.31"},
			{Name: "etcd-2", Host: "10.0.0.32"},
		},
		PKIDir: "/etc/etcd/pki",
	}
	svc := NewService(m, "", "v3.5.21", cc)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	statuses, err := svc.Status(ctx, cc.Members)
	require.NoError(t, err)
	require.Len(t, statuses, 2)
	assert.Equal(t, "active", statuses[0].Active)
	assert.Equal(t, "healthy", statuses[0].EndpointHealth)
	assert.Equal(t, "inactive", statuses[1].Active)
	assert.Equal(t, "unhealthy", statuses[1].EndpointHealth)
}

func TestService_Install_Idempotent_ReusesBinary(t *testing.T) {
	m := &mockExecutor{
		RunFn: func(_ context.Context, _, command string) exec.Result {
			if strings.HasPrefix(command, "/usr/local/bin/etcd --version") {
				// Service.Version is "v3.5.21" — match exactly so the
				// idempotency check passes.
				return exec.Result{Stdout: []byte("etcd Version: v3.5.21")}
			}
			return exec.Result{}
		},
	}
	cc := ClusterConfig{
		Members: []Member{{Name: "etcd-1", Host: "10.0.0.31"}},
		PKIDir:  "/etc/etcd/pki",
	}
	svc := NewService(m, "/tmp/etcd.tar.gz", "v3.5.21", cc)

	paths := &CertPaths{
		Dir:    "/tmp/pki",
		CA:     "/tmp/pki/ca.crt",
		CAKey:  "/tmp/pki/ca.key",
		Server: "/tmp/pki/server.crt",
		Peer:   "/tmp/pki/peer.crt",
		Client: "/tmp/pki/client.crt",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, svc.Install(ctx, cc.Members[0], paths))

	require.NotEmpty(t, m.history)
	assert.True(t, strings.HasPrefix(m.history[0], "/usr/local/bin/etcd --version"))
	for _, h := range m.history {
		assert.NotContains(t, h, "tar -xzf", "should not re-extract when version matches")
	}
}
