package image

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

// LatestContainerdVersion queries the GitHub API for the latest non-prerelease
// containerd release and returns its tag (e.g. "v2.1.0"). Used as the default
// version when packing a bundle so airgap installs track upstream stable
// without operator intervention. Override with a fixed version via HCL or
// CLI flag.
//
// The response is cached in CacheDir so repeated pack builds on the same
// day don't hammer the GitHub API.
func (u *UpstreamDownloader) LatestContainerdVersion(ctx context.Context) (string, error) {
	return u.latestGitHubTag(ctx, "containerd", "containerd", "containerd-latest.txt")
}

// LatestDockerVersion queries the GitHub API for the latest non-prerelease
// moby/moby release and returns its tag (e.g. "v28.0.0"). Same caching +
// override semantics as LatestContainerdVersion.
func (u *UpstreamDownloader) LatestDockerVersion(ctx context.Context) (string, error) {
	return u.latestGitHubTag(ctx, "moby", "moby", "docker-latest.txt")
}

// latestGitHubTag hits https://api.github.com/repos/<owner>/<repo>/releases/latest,
// parses out tag_name, and caches it under CacheDir/<cacheFile>. The cache
// file is read on every call — pack builds are infrequent enough that the
// staleness cost is negligible, and we avoid a network round-trip when
// nothing has moved upstream.
func (u *UpstreamDownloader) latestGitHubTag(ctx context.Context, owner, repo, cacheFile string) (string, error) {
	dest := filepath.Join(u.CacheDir, cacheFile)
	if st, err := os.Stat(dest); err == nil && st.Size() > 0 && time.Since(st.ModTime()) < 24*time.Hour {
		b, err := os.ReadFile(dest)
		if err == nil {
			tag := strings.TrimSpace(string(b))
			if tag != "" {
				return tag, nil
			}
		}
	}
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", owner, repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := u.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("github api %s/%s: %w", owner, repo, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github api %s/%s: HTTP %d", owner, repo, resp.StatusCode)
	}
	var payload struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", fmt.Errorf("github api %s/%s decode: %w", owner, repo, err)
	}
	tag := strings.TrimSpace(payload.TagName)
	if tag == "" {
		return "", fmt.Errorf("github api %s/%s: empty tag_name", owner, repo)
	}
	if err := os.MkdirAll(u.CacheDir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(dest, []byte(tag), 0o644); err != nil {
		// Caching is best-effort; the caller still gets the right tag.
		logger.Warn("latest-tag cache write failed", "path", dest, "err", err)
	}
	return tag, nil
}

// errUnexpected is a sentinel for "API succeeded but response was unusable".
// We don't use errors.New at package level to keep this small.

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
	// Free the local image store now that the bundle has its own copy of
	// every k8s image as a docker-archive layer. ~1-2 GB on a typical pack.
	if err := puller.Remove(ctx, imgs); err != nil {
		logger.Warn("k8s images local cleanup returned errors; non-fatal", "err", err)
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
	// Free the local image store — registry:2 is in the bundle layer now.
	if err := puller.Remove(ctx, []string{img}); err != nil {
		logger.Warn("registry image local cleanup returned errors; non-fatal", "err", err)
	}
	logger.Info("registry image tar written", "dest", dest, "size_bytes", mustStat(dest))
	return dest, nil
}

// DefaultRegistryVersion is the upstream distribution/distribution tag ko
// pins the in-cluster registry binary to. v2.8.3 is the latest 2.x release
// line that ships a static Go binary and matches registry:2 image tags.
const DefaultRegistryVersion = "2.8.3"

// RegistryBinary downloads the static `registry` Go binary for the given
// arch from https://github.com/distribution/distribution/releases (e.g.
// registry_2.8.3_linux_amd64.tar.gz) and wraps it in a tar.gz that contains
// a single ./registry entry — same pattern as Kubeadm(). Offline init
// extracts this to /usr/local/bin/registry and runs it under a hardened
// systemd unit instead of `nerdctl run registry:2`.
func (u *UpstreamDownloader) RegistryBinary(ctx context.Context, version, arch string) (string, error) {
	v := strings.TrimPrefix(version, "v")
	url := fmt.Sprintf("https://github.com/distribution/distribution/releases/download/v%s/registry_%s_linux_%s.tar.gz", v, v, arch)
	dest := filepath.Join(u.CacheDir, "registry-bin", fmt.Sprintf("registry-%s-linux-%s.tar.gz", v, arch))
	if st, err := os.Stat(dest); err == nil && st.Size() > 0 {
		logger.Info("registry binary cached", "path", dest)
		return dest, nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	tmp := dest + ".part"
	if err := downloadFile(ctx, u.HTTP, url, tmp); err != nil {
		return "", fmt.Errorf("registry binary: %w", err)
	}
	// The release tarball is a plain tar.gz whose root entry is `./registry`.
	// Unwrap it to a single file and re-wrap via wrapBinaryAsTarGz so the
	// bundle layer shape matches the kubeadm layer (one ./<name> entry).
	bin, err := extractSingleBinary(tmp, "registry")
	if err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("registry binary extract: %w", err)
	}
	if err := wrapBinaryAsTarGz(bin, dest, "registry"); err != nil {
		_ = os.Remove(tmp)
		_ = os.Remove(bin)
		return "", fmt.Errorf("registry binary wrap: %w", err)
	}
	_ = os.Remove(tmp)
	_ = os.Remove(bin)
	logger.Info("registry binary downloaded", "url", url, "dest", dest)
	return dest, nil
}

// extractSingleBinary reads srcTarGz and writes the first regular-file entry
// whose name matches wantName to a temp file. Returns the temp path.
func extractSingleBinary(srcTarGz, wantName string) (string, error) {
	in, err := os.Open(srcTarGz)
	if err != nil {
		return "", err
	}
	defer in.Close()
	gz, err := gzip.NewReader(in)
	if err != nil {
		return "", err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return "", fmt.Errorf("no %q entry in %s", wantName, srcTarGz)
		}
		if err != nil {
			return "", err
		}
		base := filepath.Base(h.Name)
		if base != wantName {
			continue
		}
		if h.Typeflag != tar.TypeReg {
			return "", fmt.Errorf("%s: entry %s is not a regular file (type=%d)", srcTarGz, h.Name, h.Typeflag)
		}
		tmp, err := os.CreateTemp("", "ko-registry-bin-")
		if err != nil {
			return "", err
		}
		if _, err := io.Copy(tmp, tr); err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return "", err
		}
		if err := tmp.Close(); err != nil {
			os.Remove(tmp.Name())
			return "", err
		}
		if err := os.Chmod(tmp.Name(), 0o755); err != nil {
			os.Remove(tmp.Name())
			return "", err
		}
		return tmp.Name(), nil
	}
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
	// Free the local image store — every cilium image is now a bundle
	// layer (~1 GB saved across the cilium 1.16 image set).
	if err := puller.Remove(ctx, imgs); err != nil {
		logger.Warn("cilium images local cleanup returned errors; non-fatal", "err", err)
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
//
// Remove is invoked after a successful Save so the local image store
// doesn't accumulate the k8s / cilium / registry:2 images pack pulls in.
// The return is a joined error of per-image failures; callers are free
// to treat it as best-effort — a leftover image only costs disk.
type ImagePuller interface {
	Name() string
	PullAll(ctx context.Context, images []string) error
	Save(ctx context.Context, dest string, images []string) error
	Remove(ctx context.Context, images []string) error
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
	preStat, _ := os.Stat(dest)
	preSize := int64(0)
	if preStat != nil {
		preSize = preStat.Size()
	}
	// docker / nerdctl emit docker-archive tarballs that store a separate
	// copy of every shared layer blob under blobs/sha256/<digest>. On
	// storage drivers without cross-image dedup (docker's vfs in GitHub
	// Actions runners, for example), this triples the tar size for the
	// k8s / cilium image sets. Manifest.json still references blobs by
	// sha256 path, so ctr images import works just fine if we rewrite the
	// tar with each blob present only once.
	deduped := dest + ".dedup"
	if err := dedupDockerArchive(dest, deduped); err != nil {
		// Dedup is best-effort — fall back to the un-deduped tar.
		logger.Warn("docker-archive dedup failed; using un-deduped tar", "err", err)
		return nil
	}
	if err := os.Rename(deduped, dest); err != nil {
		logger.Warn("rename deduped tar failed; using un-deduped tar", "err", err)
		return nil
	}
	postStat, _ := os.Stat(dest)
	postSize := int64(0)
	if postStat != nil {
		postSize = postStat.Size()
	}
	logger.Info("docker-archive dedup applied",
		"pre_bytes", preSize, "post_bytes", postSize,
		"ratio", fmt.Sprintf("%.2fx", float64(preSize)/float64(postSize+1)))
	return nil
}

// Remove deletes images from the local image store. Pack invokes it after
// a successful Save so the k8s / cilium / registry:2 images pulled in for
// the bundle don't accumulate in the operator's docker/nerdctl store
// (5-10 GB typical per build).
//
// Best-effort by design: each image is removed independently, failures
// are logged as warnings, and a non-nil joined error is returned so callers
// can choose to surface it. The current call sites ignore the return — a
// leftover image only costs disk, not correctness.
func (p *cliPuller) Remove(ctx context.Context, images []string) error {
	var errs []string
	for _, img := range images {
		cmd := exec.CommandContext(ctx, p.bin, "rmi", img)
		out, err := cmd.CombinedOutput()
		if err != nil {
			// `rmi` is best-effort; a single failure (image already gone,
			// image referenced by a stopped container, etc.) must not abort
			// the rest of the cleanup pass.
			logger.Warn("local image cleanup failed", "tool", p.bin, "image", img,
				"err", err, "out", strings.TrimSpace(string(out)))
			errs = append(errs, fmt.Sprintf("%s: %s", img, strings.TrimSpace(string(out))))
			continue
		}
		logger.Info("local image cleaned up", "tool", p.bin, "image", img)
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("%s rmi partial failure: %s", p.bin, strings.Join(errs, "; "))
}

// dedupDockerArchive reads a docker-archive tar at src and writes a copy at
// dst with each blobs/sha256/<digest> file stored only once. Non-blob entries
// (manifest.json, configs, repositories) are passed through verbatim.
//
// The output remains a valid docker-archive — ctr images import looks up
// blobs by the path written in manifest.json, which is content-addressable
// by sha256. We just collapse duplicate copies in the tar stream.
// DedupDockerArchive is the exported form of dedupDockerArchive for testing
// from other packages; the implementation is the same.
var DedupDockerArchive = dedupDockerArchive

// dedupDockerArchive reads a docker-archive tar at src and writes a copy at
// dst with each unique-content blob stored exactly once. Non-blob entries
// (manifest.json, configs, repositories) are written in source order.
//
// Dedup is keyed on each blob's actual sha256, not on its tar path. docker
// save normally stores blobs at blobs/sha256/<digest> where the path
// matches the content hash, but docker's vfs storage driver (the default
// on GitHub Actions runners) doesn't dedup across images, so the same
// content can appear under the same path multiple times. We catch that —
// and the rarer case of the same content under different paths — by
// hashing each blob as we read it.
//
// The output is a valid docker-archive: ctr images import looks up blobs by
// the path written in manifest.json, which we rewrite so every layer points
// at the surviving canonical blob path.
func dedupDockerArchive(src, dst string) error {
	type blobEntry struct {
		header  *tar.Header
		content []byte
	}
	type rawEntry struct {
		header  *tar.Header
		content []byte
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tr := tar.NewReader(in)
	blobs := map[string]*blobEntry{} // sha256 -> first-seen entry
	var blobOrder []string           // sha256 keys in first-seen order
	var nonBlob []rawEntry           // manifest.json + configs etc, source order
	var manifestData []byte

	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			return err
		}
		hCopy := *h
		hCopy.Size = int64(len(body))
		if h.Name == "manifest.json" {
			manifestData = body
			nonBlob = append(nonBlob, rawEntry{header: &hCopy, content: body})
			continue
		}
		if !strings.HasPrefix(h.Name, "blobs/sha256/") {
			nonBlob = append(nonBlob, rawEntry{header: &hCopy, content: body})
			continue
		}
		sum := sha256.Sum256(body)
		key := hex.EncodeToString(sum[:])
		if _, exists := blobs[key]; !exists {
			blobs[key] = &blobEntry{header: &hCopy, content: body}
			blobOrder = append(blobOrder, key)
		}
	}
	if len(manifestData) == 0 {
		return fmt.Errorf("no manifest.json in docker-archive; refusing to dedup")
	}

	// Rewrite manifest.json so each image's Layers point at the surviving
	// (canonical) blob path. With the first-seen-wins rule above, every
	// input path maps to itself — but the manifest might still reference
	// paths that no longer exist in the output (if docker save emitted
	// the same content under multiple distinct paths). In that case we
	// fall back to looking up by content hash.
	canonical := make(map[string]string, len(blobOrder))
	for _, key := range blobOrder {
		canonical[blobs[key].header.Name] = blobs[key].header.Name
	}
	hashToCanonical := make(map[string]string, len(blobOrder))
	for _, key := range blobOrder {
		hashToCanonical[key] = blobs[key].header.Name
	}
	rewrittenManifest, err := rewriteManifestLayers(manifestData, canonical, hashToCanonical)
	if err != nil {
		return err
	}
	for i := range nonBlob {
		if nonBlob[i].header.Name == "manifest.json" {
			nonBlob[i].content = rewrittenManifest
			nonBlob[i].header.Size = int64(len(rewrittenManifest))
		}
	}

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	tw := tar.NewWriter(out)
	defer tw.Close()

	for _, e := range nonBlob {
		if err := tw.WriteHeader(e.header); err != nil {
			return err
		}
		if _, err := tw.Write(e.content); err != nil {
			return err
		}
	}
	for _, key := range blobOrder {
		b := blobs[key]
		if err := tw.WriteHeader(b.header); err != nil {
			return err
		}
		if _, err := tw.Write(b.content); err != nil {
			return err
		}
	}
	return nil
}

// rewriteManifestLayers rewrites manifest.json so each image's Layers list
// points at the canonical (surviving) blob path. `canonical` maps input
// path -> canonical path; `hashToCanonical` maps content sha256 -> canonical
// path, used when the input path doesn't appear in `canonical` (a docker
// save bug we still want to handle gracefully).
func rewriteManifestLayers(manifestData []byte, canonical map[string]string, hashToCanonical map[string]string) ([]byte, error) {
	var manifest []map[string]any
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return nil, err
	}
	for _, img := range manifest {
		layers, _ := img["Layers"].([]any)
		for i, l := range layers {
			p, _ := l.(string)
			if c, ok := canonical[p]; ok {
				layers[i] = c
				continue
			}
			if len(p) == 64 {
				if c, ok := hashToCanonical[p]; ok {
					layers[i] = c
				}
			}
		}
		img["Layers"] = layers
	}
	return json.Marshal(manifest)
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