package cluster

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ko-build/ko/pkg/config"
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

// --- resetScript tests ---

func TestResetScript_Default_HasCoreCleanup(t *testing.T) {
	td := NewTeardown(NewMockExecutor())
	td.CRI = "unix:///run/containerd/containerd.sock"
	script := td.resetScript()
	for _, want := range []string{
		"set -euo pipefail",
		"systemctl stop kubelet",
		"systemctl stop containerd",
		"systemctl stop etcd",
		"kubeadm reset --force",
		"--cri-socket=unix:///run/containerd/containerd.sock",
		"rm -rf /etc/kubernetes",
		"rm -rf /var/lib/kubelet",
		"rm -rf /var/lib/etcd",
		"rm -rf /etc/cni/net.d",
		"rm -rf /opt/cni/bin",
		"rm -rf /var/lib/cni",
		"iptables --flush",
		"iptables -t nat    --flush",
		"iptables -t mangle --flush",
		"ipvsadm --clear",
		`ip link delete "$iface"`,
		"flannel.1",
		"vxlan.calico",
		"rm -f  /etc/containerd/config.toml",
		"rm -f  /etc/docker/daemon.json",
		"rm -rf /etc/systemd/system/kubelet.service.d",
		"rm -f  /etc/systemd/system/etcd.service",
		"rm -f  /etc/systemd/system/ko-etcd-backup.service",
		"rm -f  /etc/systemd/system/ko-etcd-backup.timer",
		"rm -rf /etc/etcd",
		"rm -rf /var/backups/etcd",
	} {
		assert.Contains(t, script, want, "missing cleanup: %q", want)
	}
}

func TestResetScript_Default_DoesNotPurgeImageCache(t *testing.T) {
	td := NewTeardown(NewMockExecutor())
	td.Purge = false
	script := td.resetScript()
	assert.Contains(t, script, `if [ "false" = "true" ]`,
		"purge branch must be guarded by the bool so it stays a no-op by default")
	// The image-cache cleanup lines DO appear in the script (inside the
	// `if`), but they're unreachable at runtime. Verify the guard:
	assert.Contains(t, script, `if [ "false" = "true" ]`)
	assert.NotContains(t, script, `if [ "true" = "true" ]`,
		"default reset must not enable the purge branch")
}

func TestResetScript_Purge_NukesImageCache(t *testing.T) {
	td := NewTeardown(NewMockExecutor())
	td.Purge = true
	script := td.resetScript()
	assert.Contains(t, script, `if [ "true" = "true" ]`,
		"purge branch must activate when Purge=true")
	assert.Contains(t, script, "rm -rf /var/lib/containerd")
	assert.Contains(t, script, "rm -rf /var/lib/docker")
	assert.Contains(t, script, "ctr -n k8s.io containers delete --force --all")
	assert.Contains(t, script, "rm -rf /var/lib/ko")
	assert.Contains(t, script, "rm -rf /root/.ko")
}

func TestResetScript_HasVethWildcardCleanup(t *testing.T) {
	td := NewTeardown(NewMockExecutor())
	script := td.resetScript()
	assert.Contains(t, script, "/sys/class/net/veth*/ifindex",
		"veth* cleanup must use the /sys wildcard, not just the named interfaces")
}

func TestResetScript_HasStaleMountCleanup(t *testing.T) {
	td := NewTeardown(NewMockExecutor())
	script := td.resetScript()
	assert.Contains(t, script, "mount -t fuse-overlayfs,overlay",
		"must lazy-unmount overlayfs mounts (containerd snapshotter)")
	assert.Contains(t, script, "/proc/self/mounts",
		"kubelet pod volume mounts must be sourced from /proc/self/mounts")
}

// --- ResetAllWithConfig tests ---

func TestResetAllWithConfig_Stacked_SkipsExternalEtcdUninstall(t *testing.T) {
	mock := NewMockExecutor()
	defer mock.Close()
	mock.RunFn = func(_ context.Context, _, _ string) Result { return Result{} }
	td := NewTeardown(mock)
	// Stacked mode: no external etcd uninstall — just per-host reset.
	err := td.ResetAllWithConfig(context.Background(), stackedCfg())
	require.NoError(t, err)
	// No etcd.service stop should appear (stacked mode: etcd runs as a
	// static pod under kubelet, no systemd unit to stop).
	var stopped []string
	for _, c := range mock.Calls {
		if strings.HasPrefix(c.Command, "systemctl stop") {
			stopped = append(stopped, c.Command)
		}
	}
	for _, s := range stopped {
		assert.NotContains(t, s, "systemctl stop etcd", "stacked mode: no etcd.service systemd unit to stop")
	}
}

func TestResetAllWithConfig_External_UninstallsEtcdFirst(t *testing.T) {
	mock := NewMockExecutor()
	defer mock.Close()
	mock.RunFn = func(_ context.Context, _, _ string) Result { return Result{} }
	td := NewTeardown(mock)
	err := td.ResetAllWithConfig(context.Background(), externalEtcdCfg())
	require.NoError(t, err)
	// We should see etcd.service stop calls (one per member) before
	// kubeadm reset runs. The uninstall is invoked first.
	var etcdStops, kubeadmResets int
	var firstEtcdStop, firstKubeadm int = -1, -1
	for i, c := range mock.Calls {
		if strings.Contains(c.Command, "systemctl disable --now etcd") {
			etcdStops++
			if firstEtcdStop < 0 {
				firstEtcdStop = i
			}
		}
		if strings.Contains(c.Command, "kubeadm reset --force") {
			kubeadmResets++
			if firstKubeadm < 0 {
				firstKubeadm = i
			}
		}
	}
	assert.Equal(t, 2, etcdStops, "one etcd.service disable per member (config has 2 etcd members)")
	assert.Equal(t, 2, kubeadmResets, "one kubeadm reset per master (config has 2 masters)")
	assert.True(t, firstEtcdStop < firstKubeadm,
		"external-etcd uninstall must run before kubeadm reset (got firstEtcdStop=%d firstKubeadm=%d)",
		firstEtcdStop, firstKubeadm)
}

// --- helpers ---

func stackedCfg() *config.File {
	return &config.File{
		Cluster: config.ClusterBlock{Name: "test", Version: "1.30.0"},
		Nodes: config.NodesBlock{
			Masters: []string{"10.0.0.11", "10.0.0.12"},
		},
		Etcd: config.EtcdBlock{Mode: "stacked"},
	}
}

func externalEtcdCfg() *config.File {
	return &config.File{
		Cluster: config.ClusterBlock{Name: "test", Version: "1.30.0"},
		Nodes: config.NodesBlock{
			Masters: []string{"10.0.0.11", "10.0.0.12"},
		},
		Etcd: config.EtcdBlock{
			Mode:      "external",
			Endpoints: []string{"https://10.0.0.31:2379"},
			Members: []config.EtcdMemberBlock{
				{Name: "etcd-1", Host: "10.0.0.31"},
				{Name: "etcd-2", Host: "10.0.0.32"},
			},
		},
	}
}