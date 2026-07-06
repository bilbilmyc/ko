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
	logger.Info("installing docker-ce on debian-family host", "host", host, "version", i.Version, "channel", i.Channel)
	// If Version is empty (the v0.0.5 default — track latest), skip the
	// `=VERSION` pin so apt installs whatever the channel carries. Pinning
	// to an empty string would be a syntax error.
	pkgPin := ""
	if i.Version != "" {
		pkgPin = fmt.Sprintf("=%s", i.Version)
	}
	script := fmt.Sprintf(`set -euo pipefail
if ! command -v docker >/dev/null 2>&1 || ! docker --version | grep -q "%s"; then
  apt-get update
  apt-get install -y ca-certificates curl gnupg
  install -m 0755 -d /etc/apt/keyrings
  curl -fsSL https://download.docker.com/linux/$(. /etc/os-release && echo "$ID")/gpg | gpg --dearmor -o /etc/apt/keyrings/docker.gpg
  chmod a+r /etc/apt/keyrings/docker.gpg
  echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/$(. /etc/os-release && echo "$ID") $(. /etc/os-release && echo "$VERSION_CODENAME") %s" > /etc/apt/sources.list.d/docker.list
  apt-get update
  apt-get install -y docker-ce%s docker-ce-cli%s containerd.io docker-buildx-plugin docker-compose-plugin
fi
systemctl enable docker
systemctl restart docker
systemctl is-active docker
`, i.Version, i.Channel, pkgPin, pkgPin)
	res := i.Exec.Run(ctx, host, script)
	if res.Failed() {
		return fmt.Errorf("apt install docker-ce: %w", res.Err)
	}
	return nil
}

func (i *Installer) installRPM(ctx context.Context, host string) error {
	logger.Info("installing docker-ce on rpm-family host", "host", host, "version", i.Version, "channel", i.Channel)
	// dnf install with no version suffix installs the latest matching the
	// enabled repo, so empty Version means "track channel latest".
	ceVersion := i.Version
	cliVersion := i.Version
	if i.Version != "" {
		ceVersion = "docker-ce-" + i.Version
		cliVersion = "docker-ce-cli-" + i.Version
	} else {
		ceVersion = "docker-ce"
		cliVersion = "docker-ce-cli"
	}
	script := fmt.Sprintf(`set -euo pipefail
if ! command -v docker >/dev/null 2>&1 || ! docker --version | grep -q "%[1]s"; then
  dnf -y install dnf-plugins-core
  dnf config-manager --add-repo https://download.docker.com/linux/centos/docker-ce.repo
  dnf -y install %[2]s %[3]s containerd.io docker-buildx-plugin docker-compose-plugin
fi
systemctl enable docker
systemctl restart docker
systemctl is-active docker
`, i.Version, ceVersion, cliVersion)
	res := i.Exec.Run(ctx, host, script)
	if res.Failed() {
		return fmt.Errorf("dnf install docker-ce: %w", res.Err)
	}
	return nil
}

// InstallOffline installs docker-ce from the bundle's pre-staged assets —
// no network access at install time. Used by `ko init --offline` and
// `ko node add --offline` for docker-runtime nodes.
//
// Layer contract:
//   - debLayer / rpmLayer: at least one of them is required. The bundle
//     builder puts the operator-dropped .deb/.rpm here; if the operator
//     didn't drop a package, the static tgz fallback path takes over
//     (extract dockerd + docker into /usr/local/bin).
//   - staticLayer: the download.docker.com static tarball
//     (dockerd + docker + containerd in one tgz). Used when no package
//     manager is reachable (locked-down airgap distros), or when the
//     operator wants to skip apt/dnf entirely.
//
// If both a package and the static tgz are present, the package wins —
// it's the operator's blessed install path (systemd unit, cgroup
// integration, the whole nine yards). Static is the fallback for the
// "no apt mirror, no internet" scenario.
func (i *Installer) InstallOffline(ctx context.Context, host, debLayer, rpmLayer, staticLayer string) error {
	if debLayer == "" && rpmLayer == "" && staticLayer == "" {
		return fmt.Errorf("InstallOffline: at least one of debLayer/rpmLayer/staticLayer must be non-empty")
	}
	os := i.detectOS(ctx, host)
	logger.Info("installing docker-ce offline", "host", host, "os", os, "version", i.Version)
	script := fmt.Sprintf(`set -euo pipefail
# Skip the install if the right version of docker is already present.
if command -v docker >/dev/null 2>&1 && docker --version | grep -q '%[1]s'; then
  echo "docker %[1]s already installed; skipping"
  exit 0
fi

%[2]s

systemctl enable docker
systemctl restart docker
systemctl is-active docker
`, i.Version, installOfflineBranch(os, debLayer, rpmLayer, staticLayer))
	res := i.Exec.Run(ctx, host, script)
	if res.Failed() {
		return fmt.Errorf("offline docker install on %s: %w (stderr=%s)", host, res.Err, string(res.Stderr))
	}
	return nil
}

// installOfflineBranch renders the per-distro install branch for
// InstallOffline's embedded script. Pure function so it's unit-testable
// without a remote shell.
//
// Order of preference:
//  1. .deb (debian/ubuntu) / .rpm (rhel/centos) — operator-dropped
//     package, treated as authoritative when present.
//  2. static tgz — download.docker.com's `dockerd + docker +
//     containerd in one tarball`. Manual install to /usr/local/bin +
//     minimal systemd unit.
func installOfflineBranch(os, debLayer, rpmLayer, staticLayer string) string {
	switch os {
	case "ubuntu", "debian":
		if debLayer != "" {
			return fmt.Sprintf(`# Operator-staged .deb (apt/dnf unreachable in airgap)
dpkg -i --force-depends '%[1]s' || true
apt-get -f install -y || true
`, debLayer)
		}
		return installStaticBranch(staticLayer)
	case "centos", "rhel", "rocky", "almalinux", "opencloudos", "openEuler", "kylin":
		if rpmLayer != "" {
			// --nogpgcheck: airgap boxes don't have the docker gpg key
			// imported, and the operator dropped the rpm deliberately.
			// Documented in RUNBOOK §4.1 as the trust trade-off.
			return fmt.Sprintf(`dnf -y localinstall --nogpgcheck '%[1]s' || true
`, rpmLayer)
		}
		return installStaticBranch(staticLayer)
	default:
		// Unknown distro → fall back to static install (works everywhere
		// once systemd is present).
		return installStaticBranch(staticLayer)
	}
}

// installStaticBranch renders the manual "extract the static tgz +
// minimal systemd unit" path. Used when no operator package is
// available, which is the common case for locked-down airgap distros.
func installStaticBranch(staticLayer string) string {
	if staticLayer == "" {
		return `echo "no docker install source available (need deb/rpm/static tgz layer)" >&2
exit 1
`
	}
	return fmt.Sprintf(`# Manual install from the static tgz. The download.docker.com layout
# has docker/, dockerd, containerd under the tgz root — install them
# all into /usr/local/bin so the operator can call `+"`docker`"+` and
# kubelet can find dockerd when cri-dockerd asks for it.
mkdir -p /tmp/ko-docker-static
tar -xzf '%[1]s' -C /tmp/ko-docker-static
install -m 0755 /tmp/ko-docker-static/docker/* /usr/local/bin/ 2>/dev/null || true
install -m 0755 /tmp/ko-docker-static/dockerd /usr/local/bin/ 2>/dev/null || true
install -m 0755 /tmp/ko-docker-static/containerd /usr/local/bin/ 2>/dev/null || true
rm -rf /tmp/ko-docker-static

# Minimal systemd unit for the static install. The operator's dockerd
# configuration (registry mirrors, cgroup driver) is written separately
# by WriteDaemon.
cat > /etc/systemd/system/docker.service <<'KO_DOCKER_UNIT_EOF'
[Unit]
Description=Docker Application Container Engine (ko offline install)
Documentation=https://docs.docker.com
After=network-online.target containerd.service cri-dockerd.service
Wants=network-online.target

[Service]
Type=notify
ExecStart=/usr/local/bin/dockerd --host=unix:///var/run/docker.sock
ExecReload=/bin/kill -s HUP $MAINPID
Restart=always
RestartSec=5
LimitNOFILE=65536
LimitNPROC=infinity
LimitCORE=infinity

[Install]
WantedBy=multi-user.target
KO_DOCKER_UNIT_EOF
`, staticLayer)
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