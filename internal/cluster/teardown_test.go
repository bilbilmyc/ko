package cluster

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseCertLine_OK(t *testing.T) {
	line := "FILE=/etc/kubernetes/pki/ca.crt|notAfter=Jul  1 12:34:56 2126 GMT|subject=CN=k8s"
	info := parseCertLine("m1", line)
	if assert.NotNil(t, info) {
		assert.Equal(t, "/etc/kubernetes/pki/ca.crt", info.Path)
		assert.Equal(t, "m1", info.Host)
		assert.Equal(t, "CN=k8s", info.Subject)
		assert.Equal(t, 2126, info.NotAfter.Year())
	}
}

func TestParseCertLine_Bad(t *testing.T) {
	assert.Nil(t, parseCertLine("m1", ""))
	assert.Nil(t, parseCertLine("m1", "junk line"))
}

func TestTeardown_ResetAll_Order(t *testing.T) {
	var order []string
	mock := NewMockExecutor()
	defer mock.Close()
	mock.RunFn = func(_ context.Context, h, _ string) Result {
		order = append(order, h)
		return Result{Host: h, Command: "ok"}
	}
	t0 := NewTeardown(mock)
	t0.CRI = "unix:///run/containerd/containerd.sock"
	err := t0.ResetAll(context.Background(),
		[]string{"m1", "m2"}, []string{"w1", "w2"})
	assert.NoError(t, err)
	// workers come first, then masters
	assert.Equal(t, []string{"w1", "w2", "m1", "m2"}, order)
}

func TestParseCertLine_TimeIn100Years(t *testing.T) {
	line := "FILE=/etc/kubernetes/pki/ca.crt|notAfter=Jul  1 12:34:56 2126 GMT|subject=CN=k"
	info := parseCertLine("m1", line)
	if assert.NotNil(t, info) {
		assert.Equal(t, 2126, info.NotAfter.Year())
	}
}

// --- RestoreStackedEtcd tests ---

func TestTeardown_RestoreStackedEtcd_RejectsBadInputs(t *testing.T) {
	mock := NewMockExecutor()
	defer mock.Close()
	td := NewTeardown(mock)

	err := td.RestoreStackedEtcd(context.Background(), RestoreStackedOpts{
		SnapshotPath: "",
		Masters:      []string{"10.0.0.11"},
	})
	assert.Error(t, err, "empty snapshot must be rejected")

	tmp := t.TempDir()
	err = td.RestoreStackedEtcd(context.Background(), RestoreStackedOpts{
		SnapshotPath: tmp + "/missing.db",
		Masters:      []string{"10.0.0.11"},
	})
	assert.Error(t, err, "missing snapshot must be rejected")

	require.NoError(t, os.WriteFile(tmp+"/snap.db", []byte("snap"), 0o600))
	err = td.RestoreStackedEtcd(context.Background(), RestoreStackedOpts{
		SnapshotPath: tmp + "/snap.db",
		Masters:      nil,
	})
	assert.Error(t, err, "empty masters must be rejected")
}

func TestTeardown_RestoreStackedEtcd_OrderAndCommands(t *testing.T) {
	tmp := t.TempDir()
	snap := tmp + "/snap.db"
	require.NoError(t, os.WriteFile(snap, []byte("snap"), 0o600))

	var calls []string
	mock := NewMockExecutor()
	defer mock.Close()
	mock.RunFn = func(_ context.Context, host, command string) Result {
		// Capture just enough to verify the lifecycle.
		switch {
		case strings.HasPrefix(command, "hostname -s"):
			calls = append(calls, host+":hostname")
			return Result{Stdout: []byte("master-" + strings.TrimPrefix(host, "10.0.0."))}
		case strings.HasPrefix(command, "systemctl stop kubelet"):
			calls = append(calls, host+":stop-kubelet")
		case strings.Contains(command, "etcdctl snapshot restore"):
			calls = append(calls, host+":restore")
		case strings.HasPrefix(command, "systemctl start kubelet"):
			calls = append(calls, host+":start-kubelet")
		case strings.HasPrefix(command, "curl -sk"):
			// apiserver probe: report healthy on first call only.
			if host == "10.0.0.11" {
				return Result{Stdout: []byte("ok")}
			}
			return Result{Stdout: []byte("")}
		}
		return Result{}
	}

	td := NewTeardown(mock)
	err := td.RestoreStackedEtcd(context.Background(), RestoreStackedOpts{
		SnapshotPath: snap,
		Masters:      []string{"10.0.0.11", "10.0.0.12"},
	})
	require.NoError(t, err)

	// Expected sequence:
	//   1. hostname lookup for both masters
	//   2. stop kubelet on both
	//   3. restore on master-1, then master-2
	//   4. start kubelet on both
	//   5. apiserver probe on master-1
	want := []string{
		"10.0.0.11:hostname",
		"10.0.0.12:hostname",
		"10.0.0.11:stop-kubelet",
		"10.0.0.12:stop-kubelet",
		"10.0.0.11:restore",
		"10.0.0.12:restore",
		"10.0.0.11:start-kubelet",
		"10.0.0.12:start-kubelet",
	}
	assert.Equal(t, want, calls, "lifecycle order")
}

func TestTeardown_RestoreStackedEtcd_RestoreCommandShape(t *testing.T) {
	tmp := t.TempDir()
	snap := tmp + "/snap.db"
	require.NoError(t, os.WriteFile(snap, []byte("snap"), 0o600))

	var restoreCmds []string
	mock := NewMockExecutor()
	defer mock.Close()
	mock.RunFn = func(_ context.Context, host, command string) Result {
		if host == "10.0.0.11" && strings.HasPrefix(command, "hostname -s") {
			return Result{Stdout: []byte("m1")}
		}
		if host == "10.0.0.12" && strings.HasPrefix(command, "hostname -s") {
			return Result{Stdout: []byte("m2")}
		}
		if strings.Contains(command, "etcdctl snapshot restore") {
			restoreCmds = append(restoreCmds, command)
		}
		if host == "10.0.0.11" && strings.HasPrefix(command, "curl -sk") {
			return Result{Stdout: []byte("ok")}
		}
		return Result{}
	}

	td := NewTeardown(mock)
	require.NoError(t, td.RestoreStackedEtcd(context.Background(), RestoreStackedOpts{
		SnapshotPath: snap,
		Masters:      []string{"10.0.0.11", "10.0.0.12"},
	}))

	require.Len(t, restoreCmds, 2, "one restore command per master")
	// Both must reference the same --initial-cluster (so the freshly-restored
	// nodes agree on cluster membership when they come up).
	for _, cmd := range restoreCmds {
		assert.Contains(t, cmd, "--initial-cluster=m1=https://10.0.0.11:2380,m2=https://10.0.0.12:2380")
		assert.Contains(t, cmd, "--data-dir=/var/lib/etcd")
		assert.Contains(t, cmd, "chmod 0700 /var/lib/etcd")
	}
	// Each master must use its OWN --name and --initial-advertise-peer-urls.
	assert.Contains(t, restoreCmds[0], "--name=m1")
	assert.Contains(t, restoreCmds[0], "--initial-advertise-peer-urls=https://10.0.0.11:2380")
	assert.Contains(t, restoreCmds[1], "--name=m2")
	assert.Contains(t, restoreCmds[1], "--initial-advertise-peer-urls=https://10.0.0.12:2380")
}

func TestTeardown_RestoreStackedEtcd_ApiserverTimeoutFails(t *testing.T) {
	tmp := t.TempDir()
	snap := tmp + "/snap.db"
	require.NoError(t, os.WriteFile(snap, []byte("snap"), 0o600))

	mock := NewMockExecutor()
	defer mock.Close()
	mock.RunFn = func(_ context.Context, host, command string) Result {
		if strings.HasPrefix(command, "hostname -s") {
			return Result{Stdout: []byte("m1")}
		}
		// /healthz never comes back.
		return Result{}
	}

	td := NewTeardown(mock)
	td.RestoreAPITimeout = 500 * time.Millisecond
	err := td.RestoreStackedEtcd(context.Background(), RestoreStackedOpts{
		SnapshotPath: snap,
		Masters:      []string{"10.0.0.11"},
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "apiserver")
}