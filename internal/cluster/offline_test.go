package cluster

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ko-build/ko/internal/image"
	"github.com/ko-build/ko/pkg/config"
)

func TestOfflineRunner_RequiresBundle(t *testing.T) {
	exec := NewMockExecutor()
	defer exec.Close()
	r := &OfflineRunner{
		Cfg:     &config.File{Cluster: config.ClusterBlock{Name: "x"}},
		Exec:    exec,
		Bundle:  "",
		Master1: "m1",
	}
	err := r.Run(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Bundle is required")
}

func TestOfflineRunner_RejectsMissingBundleFile(t *testing.T) {
	exec := NewMockExecutor()
	defer exec.Close()
	r := &OfflineRunner{
		Cfg:     &config.File{Cluster: config.ClusterBlock{Name: "x"}},
		Exec:    exec,
		Bundle:  "/nonexistent/ko-v0.0.1-multi.oci.tar.gz",
		Master1: "m1",
	}
	err := r.Run(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bundle /nonexistent")
}

func TestOfflineRunner_LocalRegistry_Defaults(t *testing.T) {
	r := &OfflineRunner{}
	assert.Equal(t, "ko.local:5000", r.LocalRegistry())
}

func TestOfflineRunner_LocalRegistry_Override(t *testing.T) {
	r := &OfflineRunner{
		RegistryImage: "registry:3",
		RegistryPort:  6000,
		LocalHost:     "reg.internal",
	}
	assert.Equal(t, "reg.internal:6000", r.LocalRegistry())
}

func TestOfflineRunner_LocalHostOrDefault(t *testing.T) {
	assert.Equal(t, "ko.local", (&OfflineRunner{}).LocalHostOrDefault())
	assert.Equal(t, "reg.local", (&OfflineRunner{LocalHost: "reg.local"}).LocalHostOrDefault())
}

func TestOfflineRunner_CiliumVersion(t *testing.T) {
	// Pinned: must match pack.go's gatherLayers.
	assert.Equal(t, "1.16.1", (&OfflineRunner{}).CiliumVersion())
}

func TestOfflineRunner_IdentifyLayers_RequiresIndexJSON(t *testing.T) {
	tmp := t.TempDir()
	r := &OfflineRunner{}
	_, err := r.identifyLayers(tmp)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read index.json")
}

func TestOfflineRunner_IdentifyLayers_FindsAllMediaTypes(t *testing.T) {
	tmp := t.TempDir()
	bundle := buildMinimalBundleForTest(t, tmp)

	r := &OfflineRunner{}
	paths, err := r.identifyLayers(bundle)
	require.NoError(t, err)
	assert.NotEmpty(t, paths.Containerd, "containerd layer must be present")
	assert.NotEmpty(t, paths.Kubeadm, "kubeadm layer must be present")
	assert.NotEmpty(t, paths.K8sImages, "k8s images layer must be present")
	assert.NotEmpty(t, paths.CiliumImages, "cilium images layer must be present")
	assert.NotEmpty(t, paths.RegistryBinary, "registry binary layer must be present (v0.0.5+)")
	assert.NotEmpty(t, paths.CiliumChart, "cilium chart layer must be present")
	// All paths must resolve to real files in the bundle root.
	for _, p := range []string{
		paths.Containerd, paths.Kubeadm, paths.K8sImages,
		paths.CiliumImages, paths.RegistryBinary, paths.CiliumChart,
	} {
		_, err := os.Stat(p)
		assert.NoError(t, err, "layer path must exist on disk: %s", p)
	}
}

// TestOfflineRunner_ConfigureContainerd_WritesMirrorForAllUpstreams is an
// end-to-end check that the appended config snippet contains mirrors for
// every registry any of the bundled images come from.
func TestOfflineRunner_ConfigureContainerd_GeneratesExpectedMirrors(t *testing.T) {
	exec := NewMockExecutor()
	defer exec.Close()
	// Force every Run to succeed — the host doesn't exist, but we never
	// execute; we only inspect the recorded call.
	exec.RunFn = func(_ context.Context, host, command string) Result {
		return Result{Host: host, Command: command, Stdout: []byte("ok")}
	}
	r := &OfflineRunner{
		Cfg:          &config.File{Cluster: config.ClusterBlock{Name: "x"}},
		Exec:         exec,
		Master1:      "m1",
		RegistryPort: 5000,
	}
	require.NoError(t, r.ConfigureContainerd(context.Background(), "m1", "ko.local:5000"))

	// Find the call that did the append; it's the one Run call we made.
	require.NotEmpty(t, exec.Calls)
	cmd := exec.Calls[0].Command
	assert.Contains(t, cmd, `registry.mirrors."quay.io"`, "quay.io mirror must be present")
	assert.Contains(t, cmd, `registry.mirrors."registry.k8s.io"`, "registry.k8s.io mirror must be present")
	assert.Contains(t, cmd, `registry.mirrors."docker.io"`, "docker.io mirror must be present")
	assert.Contains(t, cmd, `endpoint = ["http://ko.local:5000"]`, "mirror endpoint must point at the local registry")
	assert.Contains(t, cmd, `insecure_skip_verify = true`, "local registry must be marked insecure (no TLS)")

	// v0.0.5: containerd tuning for offline airgap. The bash script must
	// inject max_concurrent_downloads=3, timeout="30s", and
	// disable_snapshot_annotations=false into the CRI plugin section.
	assert.Contains(t, cmd, "max_concurrent_downloads = 3", "containerd must cap concurrent downloads")
	assert.Contains(t, cmd, `timeout = "30s"`, "containerd must set pull timeout to 30s")
	assert.Contains(t, cmd, "disable_snapshot_annotations = false", "containerd must keep snapshot annotations for audit")
	assert.Contains(t, cmd, "systemctl restart containerd", "containerd must be restarted after config rewrite")
}

// TestOfflineRunner_StartRegistry_WritesHardenedSystemdUnit asserts the
// v0.0.5 in-cluster registry is installed as a static Go binary under
// systemd with resource limits and a security sandbox. The script is the
// contract — it must:
//   - extract the static registry binary from the bundle layer
//   - write /etc/registry-config.yml with the listening port
//   - install /etc/systemd/system/ko-registry.service with hardening
//     (User=65534, MemoryLimit=2G, CPUQuota=200%, ProtectSystem=strict,
//      SystemCallFilter=@system-service, ~@privileged)
//   - daemon-reload + enable + start the unit
//   - probe /v2/ for up to 30s and fall back to journalctl on failure
func TestOfflineRunner_StartRegistry_WritesHardenedSystemdUnit(t *testing.T) {
	exec := NewMockExecutor()
	defer exec.Close()
	exec.RunFn = func(_ context.Context, host, command string) Result {
		return Result{Host: host, Command: command, Stdout: []byte("ok")}
	}
	r := &OfflineRunner{
		Cfg:          &config.File{Cluster: config.ClusterBlock{Name: "x"}},
		Exec:         exec,
		Master1:      "m1",
		RegistryPort: 5000,
	}
	l := &layerPaths{
		Containerd:     "/var/lib/ko/bundle/containerd.tgz",
		RegistryBinary: "/var/lib/ko/bundle/registry-binary.tgz",
	}
	require.NoError(t, r.startRegistry(context.Background(), "m1", offlineLayout{}, l))

	require.NotEmpty(t, exec.Calls)
	script := exec.Calls[0].Command

	// Bundle extraction: tar the static binary into /usr/local/bin.
	assert.Contains(t, script, "tar -xzf /var/lib/ko/bundle/registry-binary.tgz -C /usr/local/bin", "must extract registry binary from bundle")
	assert.Contains(t, script, "/usr/local/bin/registry", "binary must land in /usr/local/bin")

	// Registry config: listener + storage dir + debug/metrics.
	assert.Contains(t, script, "/etc/registry-config.yml", "must write registry config")
	assert.Contains(t, script, "addr: :5000", "must listen on the configured port")
	assert.Contains(t, script, "/var/lib/ko-registry", "must use the ko-registry data dir")

	// Systemd hardening: the v0.0.5 contract — these are the rules a future
	// edit must not silently weaken.
	assert.Contains(t, script, "/etc/systemd/system/ko-registry.service", "must install a unit file")
	assert.Contains(t, script, "User=65534", "must drop to nobody (UID 65534)")
	assert.Contains(t, script, "MemoryLimit=2G", "must cap memory at 2G")
	assert.Contains(t, script, "CPUQuota=200%", "must cap CPU at 200%")
	assert.Contains(t, script, "NoNewPrivileges=true", "must disable setuid escalation")
	assert.Contains(t, script, "ProtectSystem=strict", "must lock down /usr and /etc")
	assert.Contains(t, script, "SystemCallFilter=@system-service", "must restrict syscalls")
	assert.Contains(t, script, "SystemCallFilter=~@privileged", "must deny privileged syscalls")
	assert.Contains(t, script, "ExecStart=/usr/local/bin/registry serve /etc/registry-config.yml", "must exec the registry binary with config")

	// Enable + start + readiness probe.
	assert.Contains(t, script, "systemctl enable --now ko-registry.service", "must enable+start the unit")
	assert.Contains(t, script, "http://127.0.0.1:5000/v2/", "must probe /v2/ for readiness")
	assert.Contains(t, script, "seq 1 30", "must wait up to 30s for readiness")
	assert.Contains(t, script, "journalctl -u ko-registry.service", "must include journalctl fallback in error path")
}

// TestOfflineRunner_PushImages_CoversAllK8sAndCiliumImages checks the
// generated retag+push script covers every image that the offline cluster
// will need (k8s 1.32 control plane + cilium 1.16 chart images).
func TestOfflineRunner_PushImages_AllImagesRetaggedAndPushed(t *testing.T) {
	exec := NewMockExecutor()
	defer exec.Close()
	exec.RunFn = func(_ context.Context, host, command string) Result {
		return Result{Host: host, Command: command, Stdout: []byte("ok")}
	}
	cfg := &config.File{Cluster: config.ClusterBlock{Name: "x", Version: "v1.32.0"}}
	r := &OfflineRunner{Cfg: cfg, Exec: exec, Master1: "m1"}
	o := r.layout()
	require.NoError(t, r.pushImages(context.Background(), "m1", o))

	require.NotEmpty(t, exec.Calls)
	script := exec.Calls[0].Command
	// v0.0.5: push is now wrapped in a push_with_retry() shell function,
	// dispatched in parallel via `&`, with a trailing `wait`. The literal
	// "images push --plain-http ko.local:5000" no longer appears — ko.local
	// is the per-image $target. Assert on the new skeleton instead.
	assert.Contains(t, script, "push_with_retry()", "script must define push_with_retry helper")
	assert.Contains(t, script, "images push --plain-http", "retry helper must push with --plain-http")
	assert.Contains(t, script, "CTR_RETRY_COUNT=3", "retry helper must use 3 retries")
	assert.Contains(t, script, "\nwait\n", "script must wait for all background pushes")
	// Spot-check: each k8s image must have a tag + push_with_retry & dispatch.
	for _, img := range image.K8sImagesForVersion("v1.32.0", "amd64") {
		assert.Contains(t, script, "images tag "+img+" ko.local:5000/", "k8s image %q must be retagged", img)
		assert.Contains(t, script, "push_with_retry "+img+" ko.local:5000/", "k8s image %q must be pushed via push_with_retry", img)
	}
	// Same for cilium.
	for _, img := range image.CiliumImagesForVersion("1.16.1") {
		assert.Contains(t, script, "images tag "+img+" ko.local:5000/", "cilium image %q must be retagged", img)
		assert.Contains(t, script, "push_with_retry "+img+" ko.local:5000/", "cilium image %q must be pushed via push_with_retry", img)
	}

	// v0.0.5: after the wait, every pushed image must be untagged + removed
	// from the local ctr cache (the in-cluster registry is now the source
	// of truth — keeping a local copy is pure duplication on airgap hardware
	// where master-1 disk is often <100G). The bundle tarball at
	// /tmp/ko-bundle.oci.tar.gz must also be removed.
	assert.Contains(t, script, "images unset", "after push, source tags must be unset to free the cache")
	assert.Contains(t, script, "images rm", "after push, the local image must be removed from ctr cache")
	assert.Contains(t, script, "rm -f /tmp/ko-bundle.oci.tar.gz", "the bundle tarball must be cleaned up after push")

	// Spot-check cleanup lines for one k8s + one cilium image — guard against
	// a refactor that adds the cleanup helpers but forgets to call them
	// per-image.
	for _, img := range image.K8sImagesForVersion("v1.32.0", "amd64") {
		assert.Contains(t, script, "images rm "+img, "k8s image %q must be removed from ctr cache", img)
	}
	for _, img := range image.CiliumImagesForVersion("1.16.1") {
		assert.Contains(t, script, "images rm "+img, "cilium image %q must be removed from ctr cache", img)
	}
}

// TestOfflineRunner_WriteHosts_AppendsOnceAndCoversAllNodes asserts that
// /etc/hosts on every master + worker gets a `ko.local` line, and that
// the line is only appended once on a re-run.
func TestOfflineRunner_WriteHosts_AppendsOnceAndCoversAllNodes(t *testing.T) {
	exec := NewMockExecutor()
	defer exec.Close()
	exec.RunFn = func(_ context.Context, host, command string) Result {
		return Result{Host: host, Command: command, Stdout: []byte("10.0.0.11")}
	}
	cfg := &config.File{
		Cluster: config.ClusterBlock{Name: "x"},
		Nodes: config.NodesBlock{
			Masters: []string{"10.0.0.11", "10.0.0.12"},
			Workers: []string{"10.0.0.21"},
		},
	}
	r := &OfflineRunner{Cfg: cfg, Exec: exec, Master1: "10.0.0.11"}
	o := r.layout()
	require.NoError(t, r.writeHosts(context.Background(), o))

	// First call resolves master-1 IP, then we expect one /etc/hosts call
	// per node (3 total).
	scpOrRun := 0
	for _, c := range exec.Calls {
		if c.Method == "Run" {
			scpOrRun++
			if strings.Contains(c.Command, "/etc/hosts") {
				assert.Contains(t, c.Command, "10.0.0.11 ko.local", "hosts line must include master-1 IP + ko.local")
				// Idempotency: must guard with grep -qF.
				assert.Contains(t, c.Command, "grep -qF", "must be idempotent on re-run")
			}
		}
	}
	// 1 IP-resolution call + 3 /etc/hosts calls = 4.
	assert.Equal(t, 4, scpOrRun)
}

// buildMinimalBundleForTest writes a complete OCI image-layout tar.gz into
// `tmp` that contains one layer per mediaType the offline runner cares
// about. Returned path is the unpacked bundle root.
func buildMinimalBundleForTest(t *testing.T, tmp string) string {
	t.Helper()
	out := filepath.Join(tmp, "bundle")
	require.NoError(t, os.MkdirAll(out, 0o755))

	// One tiny file per mediaType.
	files := map[string]string{
		"containerd":      "fake-containerd",
		"kubeadm":         "fake-kubeadm",
		"k8s-images":      "fake-k8s-images",
		"cilium-image":    "fake-cilium-image",
		"registry-binary": "fake-registry-binary",
		"cilium-chart":    "fake-cilium-chart",
	}
	srcs := map[string]string{}
	mediaTypes := map[string]string{
		"containerd":      image.MediaTypeKoContainerdTar,
		"kubeadm":         image.MediaTypeKoKubeadmBinary,
		"k8s-images":      image.MediaTypeKoK8sImagesTar,
		"cilium-image":    image.MediaTypeKoCiliumImagesTar,
		"registry-binary": image.MediaTypeKoRegistryBinary, // v0.0.5+: required, replaces RegistryImage
		"cilium-chart":    image.MediaTypeKoHelmChart,
	}
	for name, payload := range files {
		p := filepath.Join(tmp, name)
		require.NoError(t, os.WriteFile(p, []byte(payload), 0o644))
		srcs[name] = p
	}

	layers := []image.LayerSource{
		{SrcPath: srcs["containerd"], MediaType: mediaTypes["containerd"]},
		{SrcPath: srcs["kubeadm"], MediaType: mediaTypes["kubeadm"]},
		{SrcPath: srcs["k8s-images"], MediaType: mediaTypes["k8s-images"]},
		{SrcPath: srcs["cilium-image"], MediaType: mediaTypes["cilium-image"]},
		{SrcPath: srcs["registry-binary"], MediaType: mediaTypes["registry-binary"]},
		{SrcPath: srcs["cilium-chart"], MediaType: mediaTypes["cilium-chart"]},
	}
	tarPath, _, err := image.Build(image.BuildOpts{
		Arch:      "amd64",
		Version:   "v0.0.1",
		Layers:    layers,
		OutputDir: tmp,
	})
	require.NoError(t, err)

	// image.Build wrote `ko-v0.0.1-amd64.oci.tar.gz`; the offline runner
	// expects an already-extracted OCI layout directory. Extract here.
	require.NoError(t, extractTarGz(tarPath, out))
	return out
}

func extractTarGz(src, dest string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(dest, h.Name)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.Create(target)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				_ = out.Close()
				return err
			}
			_ = out.Close()
		default:
			// skip symlinks etc.
		}
	}
}
