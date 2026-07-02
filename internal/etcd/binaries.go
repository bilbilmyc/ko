package etcd

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// 默认版本和镜像源。我们不引入 spreadsheet-style 配置文件 — 版本钉死在
// 一个 const 里,用户要升级时编辑 `bump.go` 后跑 `go test` 验证完整性。

// DefaultVersion is the etcd release we ship. 3.5.x is the LTS line.
const DefaultVersion = "3.5.21"

// DefaultBaseURL is the upstream release tarball root. Pinned to GitHub
// releases so the URL is reproducible — no CDN indirection.
const DefaultBaseURL = "https://github.com/etcd-io/etcd/releases/download"

// sha256Sums is a curated table of known sha256s for the tarball at
// v{version} per arch. We require this entry to be present before any
// install can proceed. Add new lines here when bumping DefaultVersion.
var sha256Sums = map[string]map[string]string{
	"v3.5.21": {
		"amd64": "adddda4b06718e68671ffabff2f8cee48488ba61ad82900e639d108f2148501c",
		"arm64": "95bf6918623a097c0385b96f139d90248614485e781ec9bee4768dbb6c79c53f",
	},
}

// TarballName is "etcd-v3.5.21-linux-amd64.tar.gz" — `version` MUST
// already include the leading "v" (matches the upstream tarball layout).
func TarballName(version, goos, arch string) string {
	return fmt.Sprintf("etcd-%s-%s-%s.tar.gz", version, goos, arch)
}

// DownloadURL for the chosen version + arch. goos is `linux` (ko only
// runs against Linux nodes); arch is `amd64` or `arm64`. version
// accepts both "v3.5.21" and "3.5.21".
func DownloadURL(version, arch string) string {
	v := strings.TrimPrefix(version, "v")
	return fmt.Sprintf("%s/v%s/%s", DefaultBaseURL, v, TarballName("v"+v, "linux", arch))
}

// BinaryPaths returns the absolute paths of the etcd and etcdctl binaries
// after extraction. Layout inside the tarball is etcd-v<ver>-linux-<arch>/.
func BinaryPaths(extractDir string) (etcdBin, etcdctlBin string) {
	root := extractDir
	if entries, err := os.ReadDir(root); err == nil {
		for _, e := range entries {
			if e.IsDir() && strings.HasPrefix(e.Name(), "etcd-v") {
				root = filepath.Join(root, e.Name())
				break
			}
		}
	}
	return filepath.Join(root, "etcd"), filepath.Join(root, "etcdctl")
}

// VerifyChecksum returns nil if the tarball at path matches the pinned
// sha256 for version+arch. If either is unknown we refuse the install.
func VerifyChecksum(path, version, arch string) error {
	byArch, ok := sha256Sums[version]
	if !ok {
		return fmt.Errorf("unknown etcd version %q (no pinned sha256 — refusing to install)", version)
	}
	want, ok := byArch[arch]
	if !ok {
		return fmt.Errorf("unknown etcd %q arch %q (no pinned sha256)", version, arch)
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %q: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("hash %q: %w", path, err)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != want {
		return fmt.Errorf("sha256 mismatch for %q: got %s, want %s", path, got, want)
	}
	return nil
}

// DownloadOptions tunes Download behavior.
type DownloadOptions struct {
	URL      string        // explicit tarball URL; auto-derived when empty
	Dir      string        // destination directory
	Version  string        // e.g. "v3.5.21"
	Arch     string        // amd64 / arm64
	Timeout  time.Duration // http timeout
	Insecure bool          // skip TLS verify (lab use only)
}

// Download fetches the tarball into Dir/<TarballName> and verifies its
// checksum. Returns the local path on success.
func Download(opts DownloadOptions) (string, error) {
	if opts.Version == "" {
		opts.Version = "v" + strings.TrimPrefix(DefaultVersion, "v")
	}
	if opts.Arch == "" {
		opts.Arch = runtime.GOARCH
	}
	if opts.URL == "" {
		opts.URL = DownloadURL(opts.Version, opts.Arch)
	}
	if opts.Timeout == 0 {
		opts.Timeout = 5 * time.Minute
	}
	if opts.Dir == "" {
		opts.Dir = filepath.Join(os.TempDir(), "ko-etcd")
	}
	if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %q: %w", opts.Dir, err)
	}
	tarball := filepath.Join(opts.Dir, TarballName(opts.Version, "linux", opts.Arch))
	if _, err := os.Stat(tarball); err == nil {
		// already downloaded — re-verify before reusing
		if err := VerifyChecksum(tarball, opts.Version, opts.Arch); err == nil {
			return tarball, nil
		}
		_ = os.Remove(tarball)
	}

	client := &http.Client{Timeout: opts.Timeout}
	if opts.Insecure {
		client.Transport = insecureTransport()
	}
	resp, err := client.Get(opts.URL)
	if err != nil {
		return "", fmt.Errorf("download %q: %w", opts.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("download %q: http %d", opts.URL, resp.StatusCode)
	}
	out, err := os.OpenFile(tarball+".part", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return "", fmt.Errorf("create tmp: %w", err)
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		_ = out.Close()
		_ = os.Remove(tarball+".part")
		return "", fmt.Errorf("write tmp: %w", err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tarball+".part")
		return "", fmt.Errorf("close tmp: %w", err)
	}
	if err := os.Rename(tarball+".part", tarball); err != nil {
		return "", fmt.Errorf("rename tmp: %w", err)
	}
	if err := VerifyChecksum(tarball, opts.Version, opts.Arch); err != nil {
		_ = os.Remove(tarball)
		return "", err
	}
	return tarball, nil
}
