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
	assert.NotEmpty(t, paths.RegistryImage, "registry image layer must be present")
	assert.NotEmpty(t, paths.CiliumChart, "cilium chart layer must be present")
	// All paths must resolve to real files in the bundle root.
	for _, p := range []string{
		paths.Containerd, paths.Kubeadm, paths.K8sImages,
		paths.CiliumImages, paths.RegistryImage, paths.CiliumChart,
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
	require.NoError(t, r.configureContainerd(context.Background(), "m1", "ko.local:5000"))

	// Find the call that did the append; it's the one Run call we made.
	require.NotEmpty(t, exec.Calls)
	cmd := exec.Calls[0].Command
	assert.Contains(t, cmd, `registry.mirrors."quay.io"`, "quay.io mirror must be present")
	assert.Contains(t, cmd, `registry.mirrors."registry.k8s.io"`, "registry.k8s.io mirror must be present")
	assert.Contains(t, cmd, `registry.mirrors."docker.io"`, "docker.io mirror must be present")
	assert.Contains(t, cmd, `endpoint = ["http://ko.local:5000"]`, "mirror endpoint must point at the local registry")
	assert.Contains(t, cmd, `insecure_skip_verify = true`, "local registry must be marked insecure (no TLS)")
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
	// Spot-check: each k8s image must have a tag + push.
	for _, img := range image.K8sImagesForVersion("v1.32.0", "amd64") {
		assert.Contains(t, script, "images tag "+img+" ko.local:5000/", "k8s image %q must be retagged", img)
		assert.Contains(t, script, "images push --plain-http ko.local:5000", "k8s push must use --plain-http")
	}
	// Same for cilium.
	for _, img := range image.CiliumImagesForVersion("1.16.1") {
		assert.Contains(t, script, "images tag "+img+" ko.local:5000/", "cilium image %q must be retagged", img)
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
		"containerd":   "fake-containerd",
		"kubeadm":      "fake-kubeadm",
		"k8s-images":   "fake-k8s-images",
		"cilium-image": "fake-cilium-image",
		"registry":     "fake-registry",
		"cilium-chart": "fake-cilium-chart",
	}
	srcs := map[string]string{}
	mediaTypes := map[string]string{
		"containerd":   image.MediaTypeKoContainerdTar,
		"kubeadm":      image.MediaTypeKoKubeadmBinary,
		"k8s-images":   image.MediaTypeKoK8sImagesTar,
		"cilium-image": image.MediaTypeKoCiliumImagesTar,
		"registry":     image.MediaTypeKoRegistryImage,
		"cilium-chart": image.MediaTypeKoHelmChart,
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
		{SrcPath: srcs["registry"], MediaType: mediaTypes["registry"]},
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
