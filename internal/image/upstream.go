package image

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// UpstreamDownloader is a read-only view of the project's vendor/
// directory. It returns local paths to pre-downloaded assets (binaries
// + image archives + helm charts) for the OCI bundle layer pack. Zero
// network — everything is in vendor/ after `ko vendor fetch`.
//
// Layout (must match scripts/fetch-vendor.sh + vendor-versions.env):
//
//	vendor/containerd/<ver>/{linux-amd64,linux-arm64}.tar.gz
//	vendor/kubeadm/<ver>/{linux-amd64,linux-arm64}.tar.gz
//	vendor/kubelet/<ver>/{linux-amd64,linux-arm64}.tar.gz
//	vendor/cri-dockerd/<ver>/{linux-amd64,linux-arm64}.tar.gz
//	vendor/registry/v<ver>/{linux-amd64,linux-arm64}.tar.gz
//	vendor/docker/static/{x86_64,aarch64}/docker-<ver>.tgz
//	vendor/docker/deb/{amd64,arm64}/docker-ce_*.deb
//	vendor/docker/rpm/{x86_64,aarch64}/docker-ce-*.rpm
//	vendor/images/k8s-<ver>-{amd64,arm64}.tar
//	vendor/images/cilium-v<ver>.tar
//	vendor/images/prometheus-stack-v<ver>.tar
//	vendor/charts/cilium-<ver>.tgz
//	vendor/charts/kube-prometheus-stack-<ver>.tgz
type UpstreamDownloader struct {
	// VendorDir is the root of the vendored asset tree. Defaults to
	// ./vendor (overridable via KO_VENDOR_DIR for test sandboxes).
	VendorDir string
}

// NewUpstream creates a downloader rooted at vendorDir.
func NewUpstream(vendorDir string) *UpstreamDownloader {
	return &UpstreamDownloader{VendorDir: vendorDir}
}

// requireAsset returns the local path to a vendored asset after asserting
// it exists and is at least minSize bytes. The minimum-size check is a
// cheap defence against half-written files (e.g. fetch-vendor.sh crashed
// mid-download) — the build would otherwise ship a truncated layer.
func (u *UpstreamDownloader) requireAsset(relPath string, minSize int64) (string, error) {
	full := filepath.Join(u.VendorDir, relPath)
	st, err := os.Stat(full)
	if err != nil {
		return "", fmt.Errorf("vendor asset %s missing: %w (run `ko vendor fetch` to populate)", relPath, err)
	}
	if st.Size() < minSize {
		return "", fmt.Errorf("vendor asset %s only %d bytes (min %d) — re-run `ko vendor fetch`",
			relPath, st.Size(), minSize)
	}
	return full, nil
}

// Containerd returns the path to the vendored containerd tarball for the
// given version (e.g. "v2.1.0") and arch ("amd64" | "arm64").
func (u *UpstreamDownloader) Containerd(version, arch string) (string, error) {
	return u.requireAsset(
		filepath.Join("containerd", version, "linux-"+arch+".tar.gz"),
		10_000_000, // ~10 MB minimum — containerd tarball is ~30 MB.
	)
}

// Kubeadm returns the path to the vendored kubeadm tarball (a single-
// entry tar.gz wrapping ./kubeadm) for the given k8s version and arch.
func (u *UpstreamDownloader) Kubeadm(k8sVersion, arch string) (string, error) {
	return u.requireAsset(
		filepath.Join("kubeadm", k8sVersion, "linux-"+arch+".tar.gz"),
		40_000_000, // ~40 MB minimum — kubeadm is ~50 MB compressed.
	)
}

// Kubelet returns the path to the vendored kubelet tarball (wrapping
// ./kubelet) for the given k8s version and arch. Needed for offline
// kubelet install — the target node has no package manager access.
func (u *UpstreamDownloader) Kubelet(k8sVersion, arch string) (string, error) {
	return u.requireAsset(
		filepath.Join("kubelet", k8sVersion, "linux-"+arch+".tar.gz"),
		80_000_000, // ~80 MB minimum — kubelet is ~100 MB compressed.
	)
}

// CRIDockerd returns the path to the vendored cri-dockerd tarball
// (Mirantis upstream, single ./cri-dockerd entry) for the given version
// and arch. Needed for `Runtime=Default=docker` on k8s >= 1.24, which
// removed dockershim.
func (u *UpstreamDownloader) CRIDockerd(version, arch string) (string, error) {
	return u.requireAsset(
		filepath.Join("cri-dockerd", version, "linux-"+arch+".tar.gz"),
		10_000_000,
	)
}

// RegistryBinary returns the path to the vendored distribution/registry
// static Go binary (wrapped in a tar.gz with ./registry entry) for the
// given version and arch.
func (u *UpstreamDownloader) RegistryBinary(version, arch string) (string, error) {
	return u.requireAsset(
		filepath.Join("registry", "v"+strings.TrimPrefix(version, "v"), "linux-"+arch+".tar.gz"),
		5_000_000,
	)
}

// K8sImagesTar returns the path to the vendored docker-archive tar
// containing every kubeadm-required image for the given k8s version and
// arch (apiserver, controller-manager, scheduler, proxy, coredns, pause,
// etcd).
func (u *UpstreamDownloader) K8sImagesTar(k8sVersion, arch string) (string, error) {
	return u.requireAsset(
		filepath.Join("images", "k8s-"+k8sVersion+"-"+arch+".tar"),
		100_000_000, // ~100 MB minimum — k8s image set is ~500 MB.
	)
}

// CiliumImagesTar returns the path to the vendored docker-archive tar
// containing every image the cilium helm chart deploys.
func (u *UpstreamDownloader) CiliumImagesTar(version string) (string, error) {
	return u.requireAsset(
		filepath.Join("images", "cilium-v"+strings.TrimPrefix(version, "v")+".tar"),
		100_000_000,
	)
}

// PrometheusImagesTar returns the path to the vendored docker-archive
// tar containing the kube-prometheus-stack image set (prometheus
// operator, prometheus, alertmanager, node-exporter, kube-state-metrics,
// grafana).
func (u *UpstreamDownloader) PrometheusImagesTar(stackVersion string) (string, error) {
	return u.requireAsset(
		filepath.Join("images", "prometheus-stack-v"+stackVersion+".tar"),
		500_000_000, // ~500 MB minimum — prometheus + grafana are heavy.
	)
}

// CiliumChartTGZ returns the path to the vendored cilium helm chart tgz.
func (u *UpstreamDownloader) CiliumChartTGZ(version string) (string, error) {
	return u.requireAsset(
		filepath.Join("charts", "cilium-"+version+".tgz"),
		5_000,
	)
}

// PrometheusChartTGZ returns the path to the vendored kube-prometheus-
// stack helm chart tgz.
func (u *UpstreamDownloader) PrometheusChartTGZ(stackVersion string) (string, error) {
	return u.requireAsset(
		filepath.Join("charts", "kube-prometheus-stack-"+stackVersion+".tgz"),
		5_000,
	)
}

// DockerStatic returns the path to the vendored static docker tgz
// (download.docker.com layout: dockerd + docker + containerd inside
// one tarball). Used for the manual "extract + run" install path on
// distros that don't have a working apt/dnf repo in the airgap env.
func (u *UpstreamDownloader) DockerStatic(version, arch string) (string, error) {
	// arch: amd64 -> x86_64, arm64 -> aarch64 (download.docker.com naming)
	dir := "x86_64"
	if arch == "arm64" {
		dir = "aarch64"
	}
	return u.requireAsset(
		filepath.Join("docker", "static", dir, "docker-"+version+".tgz"),
		20_000_000,
	)
}

// DockerDeb returns the path to the vendored docker-ce .deb for the
// given arch. The operator drops the file into vendor/docker/deb/<arch>/
// manually; fetch-vendor.sh does NOT auto-download .deb (apt index
// resolution is unreliable in airgap). Use a glob to find the file
// since the filename includes a release suffix we don't pin.
func (u *UpstreamDownloader) DockerDeb(arch string) (string, error) {
	dir := filepath.Join(u.VendorDir, "docker", "deb", arch)
	matches, err := filepath.Glob(filepath.Join(dir, "docker-ce_*.deb"))
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("no docker-ce_*.deb found in %s — drop the .deb manually before pack", dir)
	}
	st, err := os.Stat(matches[0])
	if err != nil {
		return "", err
	}
	if st.Size() < 1_000_000 {
		return "", fmt.Errorf("docker-ce .deb at %s only %d bytes — looks corrupt", matches[0], st.Size())
	}
	return matches[0], nil
}

// DockerRPM returns the path to the vendored docker-ce .rpm for the
// given arch. Same manual-drop semantics as DockerDeb.
func (u *UpstreamDownloader) DockerRPM(arch string) (string, error) {
	dir := filepath.Join(u.VendorDir, "docker", "rpm", archToRPMArch(arch))
	matches, err := filepath.Glob(filepath.Join(dir, "docker-ce-*.rpm"))
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("no docker-ce-*.rpm found in %s — drop the .rpm manually before pack", dir)
	}
	st, err := os.Stat(matches[0])
	if err != nil {
		return "", err
	}
	if st.Size() < 1_000_000 {
		return "", fmt.Errorf("docker-ce .rpm at %s only %d bytes — looks corrupt", matches[0], st.Size())
	}
	return matches[0], nil
}

// archToRPMArch maps ko's amd64/arm64 to download.docker.com's RPM
// directory naming (x86_64 / aarch64).
func archToRPMArch(arch string) string {
	switch arch {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	default:
		return arch
	}
}
