package cluster

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ko-build/ko/pkg/config"
)

func TestKubectl_Builds(t *testing.T) {
	assert.Equal(t, "kubectl get nodes", kubectl("", "get nodes"))
	assert.Equal(t, "kubectl --kubeconfig=/tmp/admin.conf get nodes",
		kubectl("/tmp/admin.conf", "get nodes"))
}

func TestNodeLifecycle_Remove_BuildsCommands(t *testing.T) {
	mock := NewMockExecutor()
	defer mock.Close()
	n := &NodeLifecycle{
		Cfg: &config.File{
			Cluster: config.ClusterBlock{Version: "1.35.0"},
			Nodes:   config.NodesBlock{Masters: []string{"m1"}},
		},
		Exec:            mock,
		Kubeadm:         NewKubeadm(mock),
		KubeConfigPath:  "/tmp/admin.conf",
	}
	require.NoError(t, n.Remove(context.Background(), "w1", RemoveOptions{Force: false}))

	var drain, del, reset string
	for _, c := range mock.Calls {
		switch {
		case strings.Contains(c.Command, "drain"):
			drain = c.Command
		case strings.Contains(c.Command, "delete node"):
			del = c.Command
		case strings.Contains(c.Command, "kubeadm reset"):
			reset = c.Command
		}
	}
	assert.Contains(t, drain, "kubectl --kubeconfig=/tmp/admin.conf drain w1")
	assert.Contains(t, drain, "--ignore-daemonsets")
	assert.Contains(t, del, "delete node w1")
	assert.Contains(t, reset, "kubeadm reset")
	assert.Contains(t, reset, "--cri-socket=unix:///run/containerd/containerd.sock")
}

func TestNodeLifecycle_Remove_DockerCRI(t *testing.T) {
	mock := NewMockExecutor()
	defer mock.Close()
	n := &NodeLifecycle{
		Cfg: &config.File{
			Cluster: config.ClusterBlock{Version: "1.35.0"},
			Nodes:   config.NodesBlock{Masters: []string{"m1"}},
			NodesOverride: []config.NodesOverrideBlock{
				{Host: "w1", Runtime: "docker"},
			},
		},
		Exec:            mock,
		Kubeadm:         NewKubeadm(mock),
		KubeConfigPath:  "/tmp/admin.conf",
	}
	require.NoError(t, n.Remove(context.Background(), "w1", RemoveOptions{Force: true}))
	for _, c := range mock.Calls {
		if strings.Contains(c.Command, "kubeadm reset") {
			assert.Contains(t, c.Command, "unix:///run/cri-dockerd/cri-dockerd.sock")
			assert.NotContains(t, c.Command, "unix:///var/run/docker.sock",
				"docker runtime must use cri-dockerd socket, NOT docker engine socket (dockershim was removed in k8s ≥ 1.24)")
		}
	}
}

func TestNodeLifecycle_Label(t *testing.T) {
	mock := NewMockExecutor()
	defer mock.Close()
	n := &NodeLifecycle{
		Cfg:            &config.File{},
		Exec:           mock,
		KubeConfigPath: "/tmp/admin.conf",
	}
	require.NoError(t, n.Label(context.Background(), "w1", "role", "worker"))
	require.NotEmpty(t, mock.Calls)
	assert.Contains(t, mock.Calls[0].Command, "kubectl --kubeconfig=/tmp/admin.conf label node w1 role=worker --overwrite")
}

// TestNodeLifecycle_AddWorker_OfflinePath wires NodeLifecycle with an
// OfflineRunner and a fake bundle, then asserts AddWorker does the full
// airgap flow: identify layers locally → scp bundle → extract → install
// runtime from bundle → containerd mirror config → /etc/hosts ko.local
// entry → kubelet drop-in → kubeadm join with
// --image-repository=ko.local:5000.
//
// Without this guard, an offline `ko node add` would silently fall back
// to the online apt/dnf install path and try to pull k8s images from
// registry.k8s.io over the public internet — the very thing airgap is
// supposed to prevent.
func TestNodeLifecycle_AddWorker_OfflinePath(t *testing.T) {
	tmp := t.TempDir()
	_ = buildMinimalBundleForTest(t, tmp) // pre-populates tmp with files
	// Bundle must be a tar.gz path (OfflineRunner reads it via os.Stat
	// and identifyLayersFromTar extracts it locally). buildMinimalBundleForTest
	// returns the extracted dir; we need the tar.gz it produced.
	tarPath := filepath.Join(tmp, "ko-v0.0.1-amd64.oci.tar.gz")
	require.FileExists(t, tarPath)

	exec := NewMockExecutor()
	defer exec.Close()
	exec.RunFn = func(_ context.Context, host, command string) Result {
		return Result{Host: host, Command: command, Stdout: []byte("10.0.0.11")}
	}
	exec.ScpFn = func(_ context.Context, host, src, dst string) error {
		return nil
	}
	cfg := &config.File{
		Cluster: config.ClusterBlock{Name: "x", Version: "1.32.0"},
		Nodes:   config.NodesBlock{Masters: []string{"10.0.0.11"}},
	}
	r := &OfflineRunner{
		Cfg:     cfg,
		Exec:    exec,
		Bundle:  tarPath,
		Master1: "10.0.0.11",
	}
	n := &NodeLifecycle{
		Cfg:                   cfg,
		Exec:                  exec,
		Kubeadm:               NewKubeadm(exec),
		KubeConfigPath:        "/tmp/admin.conf",
		OfflineRunner:         r,
		LocalRegistryOverride: r.LocalRegistry(),
	}
	require.NoError(t, n.AddWorker(context.Background(), "10.0.0.21"))

	// Recorded call inspection. We don't care about order — just that each
	// required piece happened on the new host (10.0.0.21) — except for the
	// join token / hash lookup, which runs on master-1.
	var (
		bundleScp      bool
		bundleExtract  bool
		containerdCfg  bool
		hostsEntry     bool
		kubeletDropIn  bool
		kubeadmJoinCmd string
	)
	for _, c := range exec.Calls {
		switch c.Method {
		case "Scp":
			if c.Host == "10.0.0.21" && strings.HasSuffix(c.Dst, "/tmp/ko-bundle.oci.tar.gz") {
				bundleScp = true
			}
		case "Run":
			switch {
			case c.Host == "10.0.0.21" && strings.Contains(c.Command, "tar -xzf /tmp/ko-bundle.oci.tar.gz"):
				bundleExtract = true
			case c.Host == "10.0.0.21" && strings.Contains(c.Command, "registry.mirrors.\"docker.io\""):
				containerdCfg = true
			case c.Host == "10.0.0.21" && strings.Contains(c.Command, "/etc/hosts") && strings.Contains(c.Command, "ko.local"):
				hostsEntry = true
			case c.Host == "10.0.0.21" && strings.Contains(c.Command, "/etc/systemd/system/kubelet.service.d/20-ko-offline.conf"):
				kubeletDropIn = true
			case c.Host == "10.0.0.21" && strings.Contains(c.Command, "kubeadm join"):
				kubeadmJoinCmd = c.Command
			}
		}
	}
	assert.True(t, bundleScp, "bundle must be scp'd to the new host")
	assert.True(t, bundleExtract, "bundle must be extracted on the new host")
	assert.True(t, containerdCfg, "containerd must be configured with mirror → ko.local")
	assert.True(t, hostsEntry, "/etc/hosts must contain the ko.local entry")
	assert.True(t, kubeletDropIn, "kubelet drop-in must be applied")
	require.NotEmpty(t, kubeadmJoinCmd, "kubeadm join must run on the new host")
	assert.Contains(t, kubeadmJoinCmd, "--image-repository=ko.local:5000", "offline join must use the in-cluster registry")
}

// TestNodeLifecycle_AddWorker_OnlinePath_NoOfflineRunner confirms the
// happy path for the existing online init: no OfflineRunner, the
// `InstallContainerd` callback runs, and kubeadm join does NOT include
// --image-repository=ko.local:5000.
func TestNodeLifecycle_AddWorker_OnlinePath_NoOfflineRunner(t *testing.T) {
	mock := NewMockExecutor()
	defer mock.Close()
	installRan := false
	n := &NodeLifecycle{
		Cfg: &config.File{
			Cluster: config.ClusterBlock{Version: "1.32.0"},
			Nodes:   config.NodesBlock{Masters: []string{"m1"}},
		},
		Exec:           mock,
		Kubeadm:        NewKubeadm(mock),
		KubeConfigPath: "/tmp/admin.conf",
		InstallContainerd: func(_ context.Context, _ string) error {
			installRan = true
			return nil
		},
	}
	require.NoError(t, n.AddWorker(context.Background(), "w1"))
	assert.True(t, installRan, "online path must run InstallContainerd")
	for _, c := range mock.Calls {
		if c.Method == "Run" && strings.Contains(c.Command, "kubeadm join") {
			assert.NotContains(t, c.Command, "--image-repository=ko.local:5000",
				"online path must NOT inject in-cluster registry into kubeadm join")
		}
	}
}

// TestOfflineRunner_ResolveMaster1IP factors out the helper used by both
// writeHosts (init) and PrepareHostFromBundle (node add).
func TestOfflineRunner_ResolveMaster1IP(t *testing.T) {
	exec := NewMockExecutor()
	defer exec.Close()
	exec.RunFn = func(_ context.Context, host, command string) Result {
		return Result{Host: host, Command: command, Stdout: []byte("10.0.0.11\n10.0.0.12")}
	}
	r := &OfflineRunner{Exec: exec, Master1: "10.0.0.11"}
	ip, err := r.resolveMaster1IP(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "10.0.0.11", ip, "must take the first IP only — multi-NIC is out of scope for v0.0.1")
}