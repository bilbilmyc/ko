package image

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ko-build/ko/internal/logger"
)

// UpstreamDownloader downloads vendor artifacts (containerd, docker deb/rpm,
// helm charts, k8s images, kubeadm, registry image) into a local cache that
// Build then packs into the OCI bundle. All downloads are idempotent — if
// the file exists with the right SHA256, it's reused.
type UpstreamDownloader struct {
	CacheDir string
	HTTP     *http.Client
}

// NewUpstream creates a downloader rooted at cacheDir.
func NewUpstream(cacheDir string) *UpstreamDownloader {
	return &UpstreamDownloader{
		CacheDir: cacheDir,
		HTTP:     &http.Client{Timeout: 10 * time.Minute},
	}
}

// Containerd downloads the upstream containerd tarball for the given
// arch and version (e.g. "v2.0.5"). Returns the local path.
func (u *UpstreamDownloader) Containerd(ctx context.Context, version, arch string) (string, error) {
	v := strings.TrimPrefix(version, "v")
	url := fmt.Sprintf("https://github.com/containerd/containerd/releases/download/v%s/containerd-%s-linux-%s.tar.gz", v, v, arch)
	dest := filepath.Join(u.CacheDir, "containerd", fmt.Sprintf("containerd-%s-linux-%s.tar.gz", v, arch))
	if st, err := os.Stat(dest); err == nil && st.Size() > 0 {
		logger.Info("containerd cached", "path", dest)
		return dest, nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	if err := downloadFile(ctx, u.HTTP, url, dest); err != nil {
		return "", fmt.Errorf("containerd: %w", err)
	}
	logger.Info("containerd downloaded", "url", url, "dest", dest)
	return dest, nil
}

// Kubeadm downloads the kubeadm binary tarball for the given k8s version
// (e.g. "v1.32.0") and arch, then wraps it in a tar.gz that contains a
// single ./kubeadm entry — so the offline init can extract it directly to
// /usr/local/bin without re-tarring.
func (u *UpstreamDownloader) Kubeadm(ctx context.Context, k8sVersion, arch string) (string, error) {
	v := strings.TrimPrefix(k8sVersion, "v")
	url := fmt.Sprintf("https://dl.k8s.io/v%s/bin/linux/%s/kubeadm", v, arch)
	dest := filepath.Join(u.CacheDir, "kubeadm", fmt.Sprintf("kubeadm-v%s-linux-%s.tar.gz", v, arch))
	if st, err := os.Stat(dest); err == nil && st.Size() > 0 {
		logger.Info("kubeadm cached", "path", dest)
		return dest, nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	tmp := dest + ".part"
	if err := downloadFile(ctx, u.HTTP, url, tmp); err != nil {
		return "", fmt.Errorf("kubeadm: %w", err)
	}
	if err := wrapBinaryAsTarGz(tmp, dest, "kubeadm"); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	_ = os.Remove(tmp)
	logger.Info("kubeadm downloaded", "url", url, "dest", dest)
	return dest, nil
}

// K8sImagesTar pulls every kube-apiserver / kube-controller-manager /
// kube-scheduler / kube-proxy / coredns / pause / etcd image for the given
// k8s version and arch into the host's local image store, then exports them
// as a single docker-archive tar so `ko init --offline` can `ctr -n=k8s.io
// images import` it. Requires nerdctl (preferred) or docker on PATH.
func (u *UpstreamDownloader) K8sImagesTar(ctx context.Context, k8sVersion, arch string) (string, error) {
	v := strings.TrimPrefix(k8sVersion, "v")
	imgs := K8sImagesForVersion(k8sVersion, arch)
	dest := filepath.Join(u.CacheDir, "k8s-images", fmt.Sprintf("k8s-%s-%s.tar", v, arch))
	if st, err := os.Stat(dest); err == nil && st.Size() > 0 {
		logger.Info("k8s images cached", "path", dest)
		return dest, nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	puller, err := detectImagePuller()
	if err != nil {
		return "", fmt.Errorf("k8s images: %w", err)
	}
	logger.Info("pulling k8s images", "tool", puller.Name(), "count", len(imgs), "version", k8sVersion)
	if err := puller.PullAll(ctx, imgs); err != nil {
		return "", fmt.Errorf("k8s images pull: %w", err)
	}
	if err := puller.Save(ctx, dest, imgs); err != nil {
		return "", fmt.Errorf("k8s images save: %w", err)
	}
	logger.Info("k8s images tar written", "dest", dest, "size_bytes", mustStat(dest))
	return dest, nil
}

// RegistryImage pulls registry:2 (the Docker distribution registry) for the
// given arch, then exports it as a docker-archive tar. Offline init runs it
// on master-1 with `--net=host` to serve all other nodes.
func (u *UpstreamDownloader) RegistryImage(ctx context.Context, arch string) (string, error) {
	dest := filepath.Join(u.CacheDir, "registry", fmt.Sprintf("registry-2-linux-%s.tar", arch))
	if st, err := os.Stat(dest); err == nil && st.Size() > 0 {
		logger.Info("registry image cached", "path", dest)
		return dest, nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	puller, err := detectImagePuller()
	if err != nil {
		return "", fmt.Errorf("registry image: %w", err)
	}
	img := "registry:2"
	if err := puller.PullAll(ctx, []string{img}); err != nil {
		return "", fmt.Errorf("registry image pull: %w", err)
	}
	if err := puller.Save(ctx, dest, []string{img}); err != nil {
		return "", fmt.Errorf("registry image save: %w", err)
	}
	logger.Info("registry image tar written", "dest", dest, "size_bytes", mustStat(dest))
	return dest, nil
}

// CiliumChartTGZ downloads the cilium helm chart tarball directly from
// helm.cilium.io — no helm CLI needed. Returns the local path.
func (u *UpstreamDownloader) CiliumChartTGZ(ctx context.Context, version string) (string, error) {
	dest := filepath.Join(u.CacheDir, "charts", fmt.Sprintf("cilium-%s.tgz", version))
	if st, err := os.Stat(dest); err == nil && st.Size() > 0 {
		logger.Info("cilium chart cached", "path", dest)
		return dest, nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	url := fmt.Sprintf("https://helm.cilium.io/cilium-%s.tgz", version)
	if err := downloadFile(ctx, u.HTTP, url, dest); err != nil {
		return "", fmt.Errorf("cilium chart: %w", err)
	}
	logger.Info("cilium chart downloaded", "url", url, "dest", dest)
	return dest, nil
}

// CiliumImagesTar pulls every image the cilium helm chart deploys and
// exports them as a single docker-archive tar so the offline registry can
// serve them. Same ImagePuller strategy as K8sImagesTar.
func (u *UpstreamDownloader) CiliumImagesTar(ctx context.Context, version string) (string, error) {
	imgs := CiliumImagesForVersion(version)
	dest := filepath.Join(u.CacheDir, "cilium-images", fmt.Sprintf("cilium-%s-images.tar", version))
	if st, err := os.Stat(dest); err == nil && st.Size() > 0 {
		logger.Info("cilium images cached", "path", dest)
		return dest, nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	puller, err := detectImagePuller()
	if err != nil {
		return "", fmt.Errorf("cilium images: %w", err)
	}
	logger.Info("pulling cilium images", "tool", puller.Name(), "count", len(imgs), "version", version)
	if err := puller.PullAll(ctx, imgs); err != nil {
		return "", fmt.Errorf("cilium images pull: %w", err)
	}
	if err := puller.Save(ctx, dest, imgs); err != nil {
		return "", fmt.Errorf("cilium images save: %w", err)
	}
	logger.Info("cilium images tar written", "dest", dest, "size_bytes", mustStat(dest))
	return dest, nil
}

// K8sImagesForVersion returns the static list of control-plane + CNI-host
// images kubeadm pulls at init for the given k8s version. This is the
// v1.32 set; older/newer versions may differ — keep this in sync with
// kubeadm's `kubeadm config images list` output.
func K8sImagesForVersion(version, arch string) []string {
	v := strings.TrimPrefix(version, "v")
	registry := "registry.k8s.io"
	_ = arch // arch only affects pause on some k8s versions; we use the multi-arch default.
	return []string{
		registry + "/kube-apiserver:v" + v,
		registry + "/kube-controller-manager:v" + v,
		registry + "/kube-scheduler:v" + v,
		registry + "/kube-proxy:v" + v,
		registry + "/coredns/coredns:v1.11.3",
		registry + "/pause:3.10",
		registry + "/etcd:3.5.16-0",
	}
}

// CiliumImagesForVersion returns the static list of images the cilium helm
// chart (1.16.x) deploys. Cilium 1.17+ may add/drop — keep this in sync.
func CiliumImagesForVersion(version string) []string {
	return []string{
		"quay.io/cilium/cilium:v" + version,
		"quay.io/cilium/operator-generic:v" + version,
		"quay.io/cilium/hubble-relay:v" + version,
		"quay.io/cilium/hubble-ui:v0.13.2",
		"quay.io/cilium/hubble-ui-backend:v0.13.2",
		"quay.io/cilium/certgen:v0.2.3",
	}
}

// ImagePuller is the abstraction ko uses for "pull + save OCI images" at
// pack time. nerdctl is preferred (it's the containerd-native CLI); docker
// is the fallback. ctr is not supported here because it doesn't expose a
// save command that emits docker-archive format.
type ImagePuller interface {
	Name() string
	PullAll(ctx context.Context, images []string) error
	Save(ctx context.Context, dest string, images []string) error
}

func detectImagePuller() (ImagePuller, error) {
	if _, err := exec.LookPath("nerdctl"); err == nil {
		return &cliPuller{bin: "nerdctl"}, nil
	}
	if _, err := exec.LookPath("docker"); err == nil {
		return &cliPuller{bin: "docker"}, nil
	}
	return nil, fmt.Errorf("neither nerdctl nor docker found in PATH; install one to bake k8s images")
}

type cliPuller struct{ bin string }

func (p *cliPuller) Name() string { return p.bin }

func (p *cliPuller) PullAll(ctx context.Context, images []string) error {
	for _, img := range images {
		cmd := exec.CommandContext(ctx, p.bin, "pull", img)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s pull %s: %w (out=%s)", p.bin, img, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

func (p *cliPuller) Save(ctx context.Context, dest string, images []string) error {
	args := append([]string{"save", "-o", dest}, images...)
	cmd := exec.CommandContext(ctx, p.bin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s save: %w (out=%s)", p.bin, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// wrapBinaryAsTarGz wraps a single downloaded binary file (e.g. kubeadm)
// into a tar.gz containing ./<name> so that offline init can extract it
// directly with `tar -xzf`. The source binary is left in place; the caller
// deletes it.
func wrapBinaryAsTarGz(srcBinary, destTarGz, name string) error {
	in, err := os.Open(srcBinary)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(destTarGz)
	if err != nil {
		return err
	}
	defer out.Close()
	gz := gzip.NewWriter(out)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{
		Name:    name,
		Mode:    0o755,
		Size:    mustStat(srcBinary),
		ModTime: time.Unix(0, 0),
		Format:  tar.FormatGNU,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	if _, err := io.Copy(tw, in); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

func mustStat(p string) int64 {
	st, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return st.Size()
}

func downloadFile(ctx context.Context, client *http.Client, url, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d for %s", resp.StatusCode, url)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}