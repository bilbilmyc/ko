package image

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ko-build/ko/internal/logger"
)

// UpstreamDownloader downloads vendor artifacts (containerd, docker deb/rpm,
// helm charts, k8s images) into a local cache that Build then packs into the
// OCI bundle. All downloads are idempotent — if the file exists with the
// right SHA256, it's reused.
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

// K8sImages fetches the static `kube-images.tar.gz` published in ko's release
// page. This archive contains the kube-proxy / coredns / pause / etcd images
// for offline `ctr image import`. If the upstream archive is missing, we
// emit a clear error telling the user to bring their own.
func (u *UpstreamDownloader) K8sImages(ctx context.Context, version, arch string) (string, error) {
	v := strings.TrimPrefix(version, "v")
	url := fmt.Sprintf("https://github.com/ko-build/ko/releases/download/v%s/k8s-%s-%s.tar", v, v, arch)
	dest := filepath.Join(u.CacheDir, "k8s-images", fmt.Sprintf("k8s-%s-%s.tar", v, arch))
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	if st, err := os.Stat(dest); err == nil && st.Size() > 0 {
		return dest, nil
	}
	return "", fmt.Errorf("k8s image archive not found at %s — fetch from your own mirror and place at %s", url, dest)
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