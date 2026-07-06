package cluster

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ko-build/ko/pkg/config"
)

// TestKubeletDropIn_ContainsRequiredFlags locks in the v0.0.5 contract:
// the drop-in must include the flags ko relies on for airgap + production
// hardening. A refactor that drops any of these silently breaks a cluster
// we have no nodes to re-test against, so we assert the literals.
func TestKubeletDropIn_ContainsRequiredFlags(t *testing.T) {
	content := KubeletDropIn("") // containerd default
	assert.Contains(t, content, "[Service]", "must be a systemd unit fragment")
	assert.Contains(t, content, "Environment=\"KUBELET_KUBEADM_ARGS=", "must override kubeadm's KUBELET_KUBEADM_ARGS")
	assert.Contains(t, content, "--image-pull-progress-deadline=30m", "airgap pulls need ≥30m; kubeadm default 1m aborts cilium pulls")
	assert.Contains(t, content, "--registry-qps=5", "must pin registry QPS for predictable backpressure")
	assert.Contains(t, content, "--registry-burst=10", "must pin registry burst to 10")
	assert.Contains(t, content, "--eviction-hard=memory.available<100Mi", "must set memory eviction threshold")
	assert.Contains(t, content, "nodefs.available<10%", "must set disk eviction threshold")
	assert.NotContains(t, content, "--container-runtime-endpoint",
		"containerd default must NOT pin the CRI socket — kubeadm auto-discovers /run/containerd/containerd.sock")
}

// TestKubeletDropIn_DockerRuntime_PinsCRIDockerd guards the k8s ≥ 1.24
// dockershim-removal fix: docker runtime kubelets must point at
// cri-dockerd's socket. Without this flag the kubelet hangs forever on
// the docker engine socket (which isn't a CRI endpoint anymore).
func TestKubeletDropIn_DockerRuntime_PinsCRIDockerd(t *testing.T) {
	content := KubeletDropIn("docker")
	assert.Contains(t, content, "--container-runtime-endpoint=unix:///run/cri-dockerd/cri-dockerd.sock",
		"docker runtime drop-in must pin cri-dockerd CRI socket (k8s ≥ 1.24 has no in-tree dockershim)")
	assert.NotContains(t, content, "unix:///var/run/docker.sock",
		"must not point at docker engine socket — it's not a CRI endpoint")
}

// TestWriteKubeletDropIn_WritesExpectedFile asserts the script (a) writes
// to the systemd drop-in dir, (b) embeds the drop-in content via heredoc,
// (c) daemon-reloads. We can't run the script in a unit test, but we can
// assert that a future refactor doesn't, say, write to /tmp by accident.
func TestWriteKubeletDropIn_WritesExpectedFile(t *testing.T) {
	// Clear any leftover cfg from other tests so WriteKubeletDropIn falls
	// back to the containerd default render path.
	SetClusterCfg(nil)
	defer SetClusterCfg(nil)

	exec := NewMockExecutor()
	defer exec.Close()
	exec.RunFn = func(_ context.Context, host, command string) Result {
		return Result{Host: host, Command: command, Stdout: []byte("ok")}
	}
	require.NoError(t, WriteKubeletDropIn(context.Background(), exec, "m1"))

	require.NotEmpty(t, exec.Calls)
	cmd := exec.Calls[0].Command
	assert.Contains(t, cmd, "/etc/systemd/system/kubelet.service.d", "must land in kubelet.service.d")
	assert.Contains(t, cmd, "20-ko-offline.conf", "filename must be stable — teardown script keys on it")
	assert.Contains(t, cmd, "mkdir -p /etc/systemd/system/kubelet.service.d", "must create the drop-in dir")
	assert.Contains(t, cmd, "cat >", "must atomically overwrite the drop-in")
	assert.Contains(t, cmd, "KO_KUBELET_EOF", "must use a heredoc to avoid shell quoting pitfalls")
	assert.Contains(t, cmd, "systemctl daemon-reload", "must reload systemd so the new Environment takes effect")
}

// TestWriteKubeletDropIn_DockerRuntime_PinsCRIDockerd wires the per-host
// config so WriteKubeletDropIn sees Runtime="docker" and renders the
// cri-dockerd endpoint in the script body. This is the offline-add path:
// the bundle install happens once, but kubelet has to know to talk to
// the CRI shim.
func TestWriteKubeletDropIn_DockerRuntime_PinsCRIDockerd(t *testing.T) {
	cfg := &fakeCfgLite{runtime: "docker"}
	SetClusterCfg(cfg)
	defer SetClusterCfg(nil)

	exec := NewMockExecutor()
	defer exec.Close()
	exec.RunFn = func(_ context.Context, host, command string) Result {
		return Result{Host: host, Command: command, Stdout: []byte("ok")}
	}
	require.NoError(t, WriteKubeletDropIn(context.Background(), exec, "w1"))

	require.NotEmpty(t, exec.Calls)
	cmd := exec.Calls[0].Command
	assert.Contains(t, cmd, "--container-runtime-endpoint=unix:///run/cri-dockerd/cri-dockerd.sock",
		"docker-runtime kubelet drop-in must pin cri-dockerd CRI socket")
}

// fakeCfgLite is a minimal adapter so kubelet_test can satisfy the
// LookupNodeOverride contract without dragging the full *config.File in.
// (We don't actually need the full hcl-validated config to render a
// drop-in — only the per-host runtime.)
type fakeCfgLite struct{ runtime string }

func (f *fakeCfgLite) LookupNodeOverride(host string) *config.NodesOverrideBlock {
	return &config.NodesOverrideBlock{Host: host, Runtime: f.runtime}
}

// TestWriteKubeletDropIn_PropagatesFailure confirms a non-zero exit on the
// remote host surfaces as an error so init aborts instead of continuing
// with kubelet running on stale config.
func TestWriteKubeletDropIn_PropagatesFailure(t *testing.T) {
	SetClusterCfg(nil)
	defer SetClusterCfg(nil)

	exec := NewMockExecutor()
	defer exec.Close()
	exec.RunFn = func(_ context.Context, host, command string) Result {
		return Result{
			Host:    host,
			Command: command,
			Err:     errors.New("simulated remote failure"),
		}
	}
	err := WriteKubeletDropIn(context.Background(), exec, "m1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "write kubelet drop-in on m1")
}

// TestWriteKubeletDropInAll_StopsOnFirstFailure asserts the helper is
// all-or-nothing: a half-applied drop-in set is worse than aborting.
func TestWriteKubeletDropInAll_StopsOnFirstFailure(t *testing.T) {
	SetClusterCfg(nil)
	defer SetClusterCfg(nil)

	exec := NewMockExecutor()
	defer exec.Close()
	calls := 0
	exec.RunFn = func(_ context.Context, host, command string) Result {
		calls++
		if calls == 2 {
			return Result{Host: host, Command: command, Err: errors.New("simulated remote failure")}
		}
		return Result{Host: host, Command: command, Stdout: []byte("ok")}
	}
	err := writeKubeletDropInAll(context.Background(), exec, []string{"m1", "m2", "m3"})
	require.Error(t, err)
	// We should have attempted m1 (ok) + m2 (err) and stopped — m3 never
	// gets the drop-in. A future refactor that loops with `continue`
	// instead of returning would silently leave m3 unprotected.
	assert.Equal(t, 2, calls, "must stop on first failure")
}

// TestKubeletDropIn_HeredocSafeQuotes guards against a refactor that
// embeds raw double-quotes inside the heredoc — kubeadm's KUBELET_KUBEADM_ARGS
// is itself a quoted Environment value, and a stray quote would break the
// unit. The current literal must NOT contain unescaped double-quotes
// inside the KUBELET_KUBEADM_ARGS=… body.
func TestKubeletDropIn_HeredocSafeQuotes(t *testing.T) {
	content := KubeletDropIn("")
	// The body between KUBELET_KUBEADM_ARGS= and the trailing quote must
	// contain no raw double-quote characters (shell would break).
	start := strings.Index(content, "KUBELET_KUBEADM_ARGS=")
	require.GreaterOrEqual(t, start, 0)
	body := content[start+len("KUBELET_KUBEADM_ARGS="):]
	end := strings.Index(body, "\"")
	require.GreaterOrEqual(t, end, 0, "missing closing quote on KUBELET_KUBEADM_ARGS")
	body = body[:end]
	assert.NotContains(t, body, "\"", "KUBELET_KUBEADM_ARGS body must not contain double-quotes")
}