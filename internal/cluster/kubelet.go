package cluster

import (
	"context"
	"fmt"
	"strings"

	"github.com/ko-build/ko/pkg/config"
)

// kubeletDropInPath is the systemd drop-in ko writes to override the kubelet
// flags kubeadm ships. Drop-ins live under /etc/systemd/system/kubelet.service.d/
// (per the kubeadm convention) and override Environment=KUBELET_KUBEADM_ARGS.
const kubeletDropInPath = "/etc/systemd/system/kubelet.service.d/20-ko-offline.conf"

// criDockerdEndpoint is the CRI socket cri-dockerd (Mirantis) listens on
// once it's running. Used as --container-runtime-endpoint for kubelet
// when the per-node runtime is docker — k8s ≥ 1.24 dropped the in-tree
// dockershim, so kubelet can't talk to dockerd directly anymore.
const criDockerdEndpoint = "unix:///run/cri-dockerd/cri-dockerd.sock"

// KubeletDropIn returns the contents of the ko kubelet drop-in for the
// given runtime. Pass runtime="" to get the containerd default; "docker"
// appends --container-runtime-endpoint pointing at cri-dockerd.
//
// v0.0.5: kubelet in an airgap cluster has very different needs from the
// kubeadm defaults:
//   - --image-pull-progress-deadline=30m — kubeadm defaults to 1m. In an
//     airgap, a large image (e.g. cilium 200M) can take 30-60s to push/pull
//     from the local registry; the 1m default aborts mid-pull and the pod
//     enters ImagePullBackOff, even though the image is reachable.
//   - --registry-qps=5 / --registry-burst=10 — kubelet's default QPS is 5,
//     burst 10, so this is technically a no-op. We set them explicitly so
//     the operator can grep /etc/systemd/system/kubelet.service.d/ and see
//     the policy we picked.
//   - --eviction-hard=memory.available<100Mi,nodefs.available<10% — kubeadm
//     doesn't set eviction thresholds. In production, without an eviction
//     threshold, a node that runs out of disk enters CrashLoopBackOff for
//     every pod instead of evicting them cleanly. We pick conservative
//     numbers that won't false-trigger on the typical 100G+ data disk.
//
// We render the drop-in as a heredoc'd Environment line so the existing
// resetScript can remove it with `rm -rf /etc/systemd/system/kubelet.service.d`
// (idempotent — works whether or not the drop-in was ever written).
func KubeletDropIn(runtime string) string {
	args := "--image-pull-progress-deadline=30m" +
		" --registry-qps=5" +
		" --registry-burst=10" +
		" --eviction-hard=memory.available<100Mi,nodefs.available<10%"
	// v0.0.5+: docker runtime → kubelet talks to cri-dockerd, not dockerd.
	// k8s ≥ 1.24 removed the in-tree dockershim, so without this kubelet
	// can't see docker containers at all.
	if runtime == "docker" {
		args += " --container-runtime-endpoint=" + criDockerdEndpoint
	}
	return "[Service]\n" +
		"Environment=\"KUBELET_KUBEADM_ARGS=" + args + "\"\n"
}

// WriteKubeletDropIn installs the ko kubelet drop-in on a single host. It
// is idempotent: re-running overwrites the file and re-enables kubelet,
// which is what we want when an operator upgrades the airgap (re-runs
// `ko init --offline` on the same host).
//
// The drop-in lands at /etc/systemd/system/kubelet.service.d/20-ko-offline.conf.
// We `systemctl daemon-reload` so kubelet picks up the new Environment on
// the next restart (kubeadm init / join will do that restart on its own).
func WriteKubeletDropIn(ctx context.Context, exec Executor, host string) error {
	runtime := ""
	if cfg := currentRuntimeForHost(host); cfg != "" {
		runtime = cfg
	}
	return writeKubeletDropInWithRuntime(ctx, exec, host, runtime)
}

// writeKubeletDropInWithRuntime is the runtime-aware variant — called
// directly by PrepareHostFromBundle so the docker-runtime path picks up
// the cri-dockerd endpoint flag without leaking global state through
// WriteKubeletDropIn's signature.
func writeKubeletDropInWithRuntime(ctx context.Context, exec Executor, host, runtime string) error {
	content := KubeletDropIn(runtime)
	script := fmt.Sprintf(`set -euo pipefail
mkdir -p /etc/systemd/system/kubelet.service.d
cat > %s <<'KO_KUBELET_EOF'
%sKO_KUBELET_EOF
systemctl daemon-reload
`, kubeletDropInPath, content)
	res := exec.Run(ctx, host, script)
	if res.Failed() {
		return fmt.Errorf("write kubelet drop-in on %s: %w (stderr=%s)", host, res.Err, string(res.Stderr))
	}
	return nil
}

// writeKubeletDropInAll applies the drop-in to every host in `hosts`.
// Failures on the first host stop the loop — kubelet drop-in is required
// for airgap clusters to function, so partial application is worse than
// rolling back the whole init.
func writeKubeletDropInAll(ctx context.Context, exec Executor, hosts []string) error {
	for _, h := range hosts {
		if err := WriteKubeletDropIn(ctx, exec, h); err != nil {
			return err
		}
	}
	return nil
}

// currentRuntimeForHost is a thin shim around the cfg that lets WriteKubeletDropIn
// stay free of cfg plumbing while still rendering the right drop-in per node.
// Returns "" when no docker override is set, so the kubelet drop-in matches
// the default containerd CRI.
func currentRuntimeForHost(host string) string {
	// We can't import the full Cfg without making WriteKubeletDropIn take
	// a *config.File; instead we lean on a package-level var that the
	// caller (NodeLifecycle / OfflineRunner) is expected to set before
	// any per-host drop-in is written. This is set by
	// OfflineRunner.Run / PrepareHostFromBundle via SetClusterCfg.
	c := clusterCfg
	if c == nil {
		return ""
	}
	override := c.LookupNodeOverride(host)
	if override != nil && override.Runtime == "docker" {
		return "docker"
	}
	return ""
}

// clusterCfg is the package-level handle into the config so legacy
// callers of WriteKubeletDropIn (which takes only host/exec) still get a
// runtime-aware drop-in. Set by SetClusterCfg from the CLI / NodeLifecycle
// before any per-node drop-in write.
var clusterCfg nodeOverrideLookup

// nodeOverrideLookup is the minimal surface kubelet.go needs from the
// cluster config to render a per-runtime drop-in. *config.File satisfies
// it via LookupNodeOverride; tests can inject a fake without pulling the
// hcl-validated config struct.
type nodeOverrideLookup interface {
	LookupNodeOverride(host string) *config.NodesOverrideBlock
}

// SetClusterCfg registers the cluster config that WriteKubeletDropIn
// consults to render a per-runtime drop-in. Pass nil to clear.
func SetClusterCfg(c nodeOverrideLookup) {
	clusterCfg = c
}

// assertKubeletDropInPath is a self-test that the path constant is what we
// expect — a refactor that silently renames the drop-in would silently
// leave the old file behind after `ko reset`, which would confuse the next
// `ko init --offline` re-run.
func assertKubeletDropInPath() {
	if !strings.HasPrefix(kubeletDropInPath, "/etc/systemd/system/kubelet.service.d/") {
		panic("kubelet drop-in path drifted from the systemd convention: " + kubeletDropInPath)
	}
}

func init() { assertKubeletDropInPath() }