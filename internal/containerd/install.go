// Package containerd installs the upstream containerd runtime on remote nodes.
package containerd

import (
	"context"
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

	execx "github.com/ko-build/ko/internal/exec"
	"github.com/ko-build/ko/internal/logger"
)

const (
	upstreamGitHub = "https://github.com/containerd/containerd/releases/download"
	defaultVersion = "v2.0.5"
	installPath    = "/usr/local"
	configDir      = "/etc/containerd"
)

// Installer installs upstream containerd onto a host via the given executor.
type Installer struct {
	Exec    execx.Executor
	Version string // e.g. "v2.0.5"
	Cache   string // local cache dir for downloaded tarballs
	Source  string // "upstream" | "vendor"
	Arch    string // host arch
}

func NewInstaller(exec execx.Executor, version, source, arch, cache string) *Installer {
	if version == "" {
		version = defaultVersion
	}
	if arch == "" {
		arch = runtime.GOARCH
	}
	if cache == "" {
		cache = defaultCacheDir()
	}
	return &Installer{
		Exec:    exec,
		Version: version,
		Source:  source,
		Arch:    arch,
		Cache:   cache,
	}
}

func defaultCacheDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".ko", "cache", "containerd")
	}
	return filepath.Join(os.TempDir(), "ko-cache", "containerd")
}

// Install runs the full upstream install on the given host. Idempotent: if
// `containerd --version` already matches, it skips download/extract and only
// ensures the config + service are in place.
func (i *Installer) Install(ctx context.Context, host, configTOML string) error {
	if i.Source != "upstream" {
		return fmt.Errorf("only upstream source is supported in S2 (got %q); vendor source lands in v0.1.x", i.Source)
	}

	if i.Arch != "amd64" && i.Arch != "arm64" {
		return fmt.Errorf("unsupported arch %q (supported: amd64, arm64)", i.Arch)
	}

	if err := i.checkExisting(ctx, host); err == nil {
		logger.Info("containerd already installed on host", "host", host, "version", i.Version)
	} else {
		tarball, err := i.fetchTarball(ctx)
		if err != nil {
			return fmt.Errorf("fetch tarball: %w", err)
		}
		if err := i.copyToHost(ctx, host, tarball); err != nil {
			return fmt.Errorf("copy to host: %w", err)
		}
		if err := i.extractOnHost(ctx, host); err != nil {
			return fmt.Errorf("extract on host: %w", err)
		}
	}

	if err := i.writeConfig(ctx, host, configTOML); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	if err := i.installSystemdUnit(ctx, host); err != nil {
		return fmt.Errorf("install systemd unit: %w", err)
	}
	if err := i.enableAndStart(ctx, host); err != nil {
		return fmt.Errorf("enable/start: %w", err)
	}
	return nil
}

func (i *Installer) checkExisting(ctx context.Context, host string) error {
	res := i.Exec.Run(ctx, host, "containerd --version")
	if res.Failed() {
		return res.Err
	}
	got := strings.TrimSpace(string(res.Stdout))
	if !strings.Contains(got, i.Version) {
		return fmt.Errorf("existing containerd version %q does not match required %q", got, i.Version)
	}
	return nil
}

func (i *Installer) fetchTarball(ctx context.Context) (string, error) {
	url := fmt.Sprintf("%s/%s/containerd-%s-linux-%s.tar.gz", upstreamGitHub, i.Version, strings.TrimPrefix(i.Version, "v"), i.Arch)
	checksumURL := url + ".sha256sum"
	if err := os.MkdirAll(i.Cache, 0o755); err != nil {
		return "", fmt.Errorf("create cache: %w", err)
	}
	dest := filepath.Join(i.Cache, filepath.Base(url))
	if st, err := os.Stat(dest); err == nil && st.Size() > 0 {
		logger.Info("using cached containerd tarball", "path", dest)
		return dest, nil
	}

	logger.Info("downloading containerd", "url", url)
	expected, err := fetchChecksum(ctx, checksumURL, filepath.Base(url))
	if err != nil {
		return "", fmt.Errorf("fetch checksum: %w", err)
	}
	if err := downloadTo(ctx, url, dest); err != nil {
		return "", fmt.Errorf("download: %w", err)
	}
	if expected != "" {
		actual, err := fileSHA256(dest)
		if err != nil {
			return "", fmt.Errorf("hash: %w", err)
		}
		if actual != expected {
			_ = os.Remove(dest)
			return "", fmt.Errorf("checksum mismatch: got %s, want %s", actual, expected)
		}
	}
	return dest, nil
}

func (i *Installer) copyToHost(ctx context.Context, host, src string) error {
	dst := "/tmp/" + filepath.Base(src)
	if err := i.Exec.Scp(ctx, host, src, dst); err != nil {
		return err
	}
	return nil
}

func (i *Installer) extractOnHost(ctx context.Context, host string) error {
	cmd := fmt.Sprintf("tar -C %s -xzf /tmp/containerd-*.tar.gz && rm -f /tmp/containerd-*.tar.gz", installPath)
	res := i.Exec.Run(ctx, host, cmd)
	if res.Failed() {
		return res.Err
	}
	return nil
}

func (i *Installer) writeConfig(ctx context.Context, host, configTOML string) error {
	if configTOML == "" {
		return fmt.Errorf("configTOML is empty (load from vendor/containerd/config.toml)")
	}
	if err := i.Exec.Scp(ctx, host, writeTempConfig(configTOML), configDir+"/config.toml"); err != nil {
		// fallback: write via heredoc
		cmd := fmt.Sprintf("mkdir -p %s && cat > %s/config.toml <<'KO_CONFIG_EOF'\n%s\nKO_CONFIG_EOF",
			configDir, configDir, configTOML)
		res := i.Exec.Run(ctx, host, cmd)
		if res.Failed() {
			return res.Err
		}
	}
	return nil
}

func writeTempConfig(content string) string {
	dir, _ := os.MkdirTemp("", "ko-containerd-")
	path := filepath.Join(dir, "config.toml")
	_ = os.WriteFile(path, []byte(content), 0o644)
	return path
}

func (i *Installer) installSystemdUnit(ctx context.Context, host string) error {
	unit := systemdUnit()
	cmd := fmt.Sprintf("cat > /etc/systemd/system/containerd.service <<'KO_UNIT_EOF'\n%s\nKO_UNIT_EOF", unit)
	res := i.Exec.Run(ctx, host, cmd)
	if res.Failed() {
		return res.Err
	}
	return nil
}

func (i *Installer) enableAndStart(ctx context.Context, host string) error {
	cmds := []string{
		"systemctl daemon-reload",
		"systemctl enable containerd",
		"systemctl restart containerd",
	}
	for _, c := range cmds {
		res := i.Exec.Run(ctx, host, c)
		if res.Failed() {
			return fmt.Errorf("%s: %w", c, res.Err)
		}
	}
	// give systemd a moment
	time.Sleep(300 * time.Millisecond)
	res := i.Exec.Run(ctx, host, "systemctl is-active containerd")
	if res.Failed() {
		return fmt.Errorf("containerd not active: %w", res.Err)
	}
	return nil
}

func systemdUnit() string {
	return `[Unit]
Description=containerd container runtime
Documentation=https://containerd.io
After=network.target local-fs.target

[Service]
ExecStartPre=-/sbin/modprobe overlay
ExecStart=/usr/local/bin/containerd
Type=notify
Delegate=yes
KillMode=process
Restart=always
RestartSec=5
LimitNPROC=infinity
LimitCORE=infinity
LimitNOFILE=infinity
TasksMax=infinity
OOMScoreAdjust=-999

[Install]
WantedBy=multi-user.target
`
}

func fetchChecksum(ctx context.Context, url, targetFilename string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("checksum HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	for line := range strings.SplitSeq(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if fields[1] == targetFilename || strings.HasSuffix(fields[1], "/"+targetFilename) {
			return strings.ToLower(fields[0]), nil
		}
	}
	return "", nil
}

func downloadTo(ctx context.Context, url, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, resp.Body); err != nil {
		return err
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
