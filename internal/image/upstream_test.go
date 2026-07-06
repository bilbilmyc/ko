package image

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Image-list tests: the static lists are consumed by future pushImages
// paths in internal/cluster/offline.go.

func TestK8sImagesForVersion_1_32(t *testing.T) {
	imgs := K8sImagesForVersion("v1.32.0", "amd64")
	// Every image the kubeadm default manifest needs at init time.
	assert.Contains(t, imgs, "registry.k8s.io/kube-apiserver:v1.32.0")
	assert.Contains(t, imgs, "registry.k8s.io/kube-controller-manager:v1.32.0")
	assert.Contains(t, imgs, "registry.k8s.io/kube-scheduler:v1.32.0")
	assert.Contains(t, imgs, "registry.k8s.io/kube-proxy:v1.32.0")
	assert.Contains(t, imgs, "registry.k8s.io/coredns/coredns:v1.11.3")
	assert.Contains(t, imgs, "registry.k8s.io/pause:3.10")
	assert.Contains(t, imgs, "registry.k8s.io/etcd:3.5.16-0")
}

func TestK8sImagesForVersion_StripsVPrefix(t *testing.T) {
	withV := K8sImagesForVersion("v1.32.0", "amd64")
	withoutV := K8sImagesForVersion("1.32.0", "amd64")
	assert.Equal(t, withV, withoutV, "v prefix must be optional")
}

func TestCiliumImagesForVersion_1_16(t *testing.T) {
	imgs := CiliumImagesForVersion("1.16.1")
	// Every image the cilium 1.16 chart deploys.
	assert.Contains(t, imgs, "quay.io/cilium/cilium:v1.16.1")
	assert.Contains(t, imgs, "quay.io/cilium/operator-generic:v1.16.1")
	assert.Contains(t, imgs, "quay.io/cilium/hubble-relay:v1.16.1")
	assert.Contains(t, imgs, "quay.io/cilium/hubble-ui:v0.13.2")
	assert.Contains(t, imgs, "quay.io/cilium/hubble-ui-backend:v0.13.2")
	assert.Contains(t, imgs, "quay.io/cilium/certgen:v0.2.3")
}

func TestCiliumImagesForVersion_VersionInTag(t *testing.T) {
	// A future bump must produce new tags, not silently reuse old ones.
	v117 := CiliumImagesForVersion("1.17.0")
	assert.Contains(t, v117, "quay.io/cilium/cilium:v1.17.0")
	assert.NotContains(t, v117, "v1.16.1")
}

func TestPrometheusImagesForVersion_75_6_1(t *testing.T) {
	imgs := PrometheusImagesForVersion("75.6.1")
	assert.Contains(t, imgs, "quay.io/prometheus-operator/prometheus-operator:v0.81.0")
	assert.Contains(t, imgs, "quay.io/prometheus/prometheus:v3.2.1")
	assert.Contains(t, imgs, "quay.io/prometheus/alertmanager:v0.28.1")
	assert.Contains(t, imgs, "quay.io/prometheus/node-exporter:v1.8.2")
	assert.Contains(t, imgs, "registry.k8s.io/kube-state-metrics/kube-state-metrics:v2.13.0")
	assert.Contains(t, imgs, "grafana/grafana:11.3.1")
}

// Vendor-asset tests: UpstreamDownloader is read-only — every method
// returns the local path under VendorDir for a vendored asset, or an
// error if the file is missing / below the minimum size.

func TestRequireAsset_OK(t *testing.T) {
	vendor := t.TempDir()
	target := filepath.Join(vendor, "x", "y.bin")
	require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o755))
	require.NoError(t, os.WriteFile(target, make([]byte, 4096), 0o644))

	u := NewUpstream(vendor)
	got, err := u.requireAsset("x/y.bin", 1024)
	require.NoError(t, err)
	assert.Equal(t, target, got)
}

func TestRequireAsset_Missing(t *testing.T) {
	u := NewUpstream(t.TempDir())
	_, err := u.requireAsset("nope.bin", 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing")
	assert.Contains(t, err.Error(), "ko vendor fetch")
}

func TestRequireAsset_Truncated(t *testing.T) {
	vendor := t.TempDir()
	target := filepath.Join(vendor, "tiny.bin")
	require.NoError(t, os.WriteFile(target, make([]byte, 100), 0o644))

	u := NewUpstream(vendor)
	_, err := u.requireAsset("tiny.bin", 1000)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "only 100 bytes")
}

func TestContainerd_PathLayout(t *testing.T) {
	vendor := t.TempDir()
	dst := filepath.Join(vendor, "containerd", "v2.1.0", "linux-amd64.tar.gz")
	require.NoError(t, os.MkdirAll(filepath.Dir(dst), 0o755))
	require.NoError(t, os.WriteFile(dst, make([]byte, 11_000_000), 0o644))

	u := NewUpstream(vendor)
	got, err := u.Containerd("v2.1.0", "amd64")
	require.NoError(t, err)
	assert.Equal(t, dst, got)
}

func TestKubeadm_Kubelet_StripOrKeepV(t *testing.T) {
	vendor := t.TempDir()
	// kubeadm lives under kubeadm/<k8sVersion>/linux-<arch>.tar.gz — same
	// convention for kubelet. We expect callers to pass KubeVersion which
	// already includes the leading v.
	dst := filepath.Join(vendor, "kubeadm", "v1.32.0", "linux-amd64.tar.gz")
	require.NoError(t, os.MkdirAll(filepath.Dir(dst), 0o755))
	require.NoError(t, os.WriteFile(dst, make([]byte, 41_000_000), 0o644))
	u := NewUpstream(vendor)
	got, err := u.Kubeadm("v1.32.0", "amd64")
	require.NoError(t, err)
	assert.Equal(t, dst, got)
}

func TestRegistryBinary_StripsVPrefix(t *testing.T) {
	vendor := t.TempDir()
	dst := filepath.Join(vendor, "registry", "v2.8.3", "linux-amd64.tar.gz")
	require.NoError(t, os.MkdirAll(filepath.Dir(dst), 0o755))
	require.NoError(t, os.WriteFile(dst, make([]byte, 5_500_000), 0o644))

	u := NewUpstream(vendor)
	got1, err := u.RegistryBinary("v2.8.3", "amd64")
	require.NoError(t, err)
	got2, err := u.RegistryBinary("2.8.3", "amd64")
	require.NoError(t, err)
	assert.Equal(t, got1, got2, "v-prefix must be optional")
	assert.Equal(t, dst, got1)
}

func TestDockerStatic_ArchDirectory(t *testing.T) {
	vendor := t.TempDir()
	dst := filepath.Join(vendor, "docker", "static", "aarch64", "docker-28.0.0.tgz")
	require.NoError(t, os.MkdirAll(filepath.Dir(dst), 0o755))
	require.NoError(t, os.WriteFile(dst, make([]byte, 21_000_000), 0o644))

	u := NewUpstream(vendor)
	got, err := u.DockerStatic("28.0.0", "arm64")
	require.NoError(t, err)
	assert.Equal(t, dst, got, "arm64 must resolve to aarch64/")
}

func TestDockerDeb_GlobHitsFile(t *testing.T) {
	vendor := t.TempDir()
	dir := filepath.Join(vendor, "docker", "deb", "amd64")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	dst := filepath.Join(dir, "docker-ce_28.0.0-1_amd64.deb")
	require.NoError(t, os.WriteFile(dst, make([]byte, 2_000_000), 0o644))

	u := NewUpstream(vendor)
	got, err := u.DockerDeb("amd64")
	require.NoError(t, err)
	assert.Equal(t, dst, got)
}

func TestDockerDeb_GlobMissIsError(t *testing.T) {
	u := NewUpstream(t.TempDir())
	_, err := u.DockerDeb("amd64")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "docker-ce_*.deb")
}

func TestDockerRPM_ArchMapping(t *testing.T) {
	vendor := t.TempDir()
	dir := filepath.Join(vendor, "docker", "rpm", "aarch64")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	dst := filepath.Join(dir, "docker-ce-28.0.0-1.aarch64.rpm")
	require.NoError(t, os.WriteFile(dst, make([]byte, 2_000_000), 0o644))

	u := NewUpstream(vendor)
	got, err := u.DockerRPM("arm64")
	require.NoError(t, err)
	assert.Equal(t, dst, got, "arm64 must resolve to aarch64/ for RPM layout")
}

func TestK8sImagesTar_PathLayout(t *testing.T) {
	vendor := t.TempDir()
	dst := filepath.Join(vendor, "images", "k8s-v1.32.0-amd64.tar")
	require.NoError(t, os.MkdirAll(filepath.Dir(dst), 0o755))
	require.NoError(t, os.WriteFile(dst, make([]byte, 110_000_000), 0o644))

	u := NewUpstream(vendor)
	got, err := u.K8sImagesTar("v1.32.0", "amd64")
	require.NoError(t, err)
	assert.Equal(t, dst, got)
}

func TestCiliumImagesTar_StripsVPrefix(t *testing.T) {
	vendor := t.TempDir()
	dst := filepath.Join(vendor, "images", "cilium-v1.16.1.tar")
	require.NoError(t, os.MkdirAll(filepath.Dir(dst), 0o755))
	require.NoError(t, os.WriteFile(dst, make([]byte, 110_000_000), 0o644))

	u := NewUpstream(vendor)
	got1, err := u.CiliumImagesTar("v1.16.1")
	require.NoError(t, err)
	got2, err := u.CiliumImagesTar("1.16.1")
	require.NoError(t, err)
	assert.Equal(t, got1, got2)
	assert.Equal(t, dst, got1)
}

func TestHelmCharts_PathLayout(t *testing.T) {
	vendor := t.TempDir()

	c := filepath.Join(vendor, "charts", "cilium-1.16.1.tgz")
	require.NoError(t, os.MkdirAll(filepath.Dir(c), 0o755))
	require.NoError(t, os.WriteFile(c, make([]byte, 8000), 0o644))

	p := filepath.Join(vendor, "charts", "kube-prometheus-stack-75.6.1.tgz")
	require.NoError(t, os.WriteFile(p, make([]byte, 8000), 0o644))

	u := NewUpstream(vendor)
	gotC, err := u.CiliumChartTGZ("1.16.1")
	require.NoError(t, err)
	assert.Equal(t, c, gotC)

	gotP, err := u.PrometheusChartTGZ("75.6.1")
	require.NoError(t, err)
	assert.Equal(t, p, gotP)
}