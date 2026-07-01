package docker

import (
	"context"
	"fmt"
	"os"
	"strings"

	execx "github.com/ko-build/ko/internal/exec"
	"github.com/ko-build/ko/internal/logger"
)

type Installer struct {
	Exec    execx.Executor
	Version string
	Channel string // "stable" | "test" | "nightly"
}

func NewInstaller(exec execx.Executor, version, channel string) *Installer {
	if version == "" {
		version = defaultVersion
	}
	if channel == "" {
		channel = "stable"
	}
	return &Installer{Exec: exec, Version: version, Channel: channel}
}

// Install performs the docker-ce install. Online: uses the official docker repo
// (apt/yum). Offline: requires docker-ce deb/rpm to be pre-staged on the host
// (e.g. via ko pack; the actual install script handles that branch).
func (i *Installer) Install(ctx context.Context, host string) error {
	os := i.detectOS(ctx, host)
	switch os {
	case "ubuntu", "debian":
		return i.installDebian(ctx, host)
	case "centos", "rhel", "rocky", "almalinux", "opencloudos", "openEuler", "kylin":
		return i.installRPM(ctx, host)
	case "":
		return fmt.Errorf("could not detect OS family on host %s", host)
	default:
		return fmt.Errorf("unsupported OS family %q for docker install", os)
	}
}

func (i *Installer) detectOS(ctx context.Context, host string) string {
	res := i.Exec.Run(ctx, host, "cat /etc/os-release 2>/dev/null | grep ^ID= | head -1")
	if res.Failed() {
		return ""
	}
	line := strings.TrimSpace(string(res.Stdout))
	line = strings.TrimPrefix(line, "ID=")
	line = strings.Trim(line, `"`)
	return strings.ToLower(line)
}

func (i *Installer) installDebian(ctx context.Context, host string) error {
	logger.Info("installing docker-ce on debian-family host", "host", host, "version", i.Version)
	script := fmt.Sprintf(`set -euo pipefail
if ! command -v docker >/dev/null 2>&1 || ! docker --version | grep -q "%s"; then
  apt-get update
  apt-get install -y ca-certificates curl gnupg
  install -m 0755 -d /etc/apt/keyrings
  curl -fsSL https://download.docker.com/linux/$(. /etc/os-release && echo "$ID")/gpg | gpg --dearmor -o /etc/apt/keyrings/docker.gpg
  chmod a+r /etc/apt/keyrings/docker.gpg
  echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/$(. /etc/os-release && echo "$ID") $(. /etc/os-release && echo "$VERSION_CODENAME") %s" > /etc/apt/sources.list.d/docker.list
  apt-get update
  apt-get install -y docker-ce=%s docker-ce-cli=%s containerd.io docker-buildx-plugin docker-compose-plugin
fi
systemctl enable docker
systemctl restart docker
systemctl is-active docker
`, i.Version, i.Channel, i.Version, i.Version)
	res := i.Exec.Run(ctx, host, script)
	if res.Failed() {
		return fmt.Errorf("apt install docker-ce: %w", res.Err)
	}
	return nil
}

func (i *Installer) installRPM(ctx context.Context, host string) error {
	logger.Info("installing docker-ce on rpm-family host", "host", host, "version", i.Version)
	script := fmt.Sprintf(`set -euo pipefail
if ! command -v docker >/dev/null 2>&1 || ! docker --version | grep -q "%[1]s"; then
  dnf -y install dnf-plugins-core
  dnf config-manager --add-repo https://download.docker.com/linux/centos/docker-ce.repo
  dnf -y install docker-ce-%[1]s docker-ce-cli-%[1]s containerd.io docker-buildx-plugin docker-compose-plugin
fi
systemctl enable docker
systemctl restart docker
systemctl is-active docker
`, i.Version)
	res := i.Exec.Run(ctx, host, script)
	if res.Failed() {
		return fmt.Errorf("dnf install docker-ce: %w", res.Err)
	}
	return nil
}

// WriteDaemon writes the docker daemon.json (idempotent) and reloads docker.
func (i *Installer) WriteDaemon(ctx context.Context, host, daemonJSON string) error {
	if err := i.Exec.Scp(ctx, host, writeTemp(daemonJSON, "daemon.json"), "/etc/docker/daemon.json"); err != nil {
		// scp may fail on first install before /etc/docker exists; fall back to heredoc
		cmd := fmt.Sprintf("mkdir -p /etc/docker && cat > /etc/docker/daemon.json <<'KO_DAEMON_EOF'\n%s\nKO_DAEMON_EOF", daemonJSON)
		res := i.Exec.Run(ctx, host, cmd)
		if res.Failed() {
			return res.Err
		}
	}
	res := i.Exec.Run(ctx, host, "systemctl restart docker")
	if res.Failed() {
		return fmt.Errorf("restart docker: %w", res.Err)
	}
	return nil
}

func writeTemp(content, name string) string {
	dir, _ := os.MkdirTemp("", "ko-docker-")
	path := dir + "/" + name
	_ = os.WriteFile(path, []byte(content), 0o644)
	return path
}
