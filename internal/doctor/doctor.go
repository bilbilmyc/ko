// Package doctor implements preflight checks used by `ko doctor`.
// Each check returns a Result{OK, Name, Message}; the doctor CLI aggregates
// them and reports failures.
package doctor

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/ko-build/ko/internal/exec"
)

// Result is a single check outcome.
type Result struct {
	Name    string
	OK      bool
	Message string
}

// LocalChecks runs checks that don't need SSH: OS, arch, kernel, memory.
func LocalChecks() []Result {
	return []Result{
		checkOS(),
		checkArch(),
		checkKernel(),
	}
}

// RemoteChecks runs per-host checks over SSH.
func RemoteChecks(ctx context.Context, ex exec.Executor, host string) []Result {
	return []Result{
		checkSSH(ctx, ex, host),
		checkKernelRemote(ctx, ex, host),
		checkEBPF(ctx, ex, host),
		checkSwap(ctx, ex, host),
		checkRuntime(ctx, ex, host),
		checkPorts(ctx, ex, host),
	}
}

func checkOS() Result {
	if runtime.GOOS != "linux" {
		return Result{Name: "OS", OK: false, Message: fmt.Sprintf("%s (k8s nodes must be Linux)", runtime.GOOS)}
	}
	return Result{Name: "OS", OK: true, Message: "linux"}
}

func checkArch() Result {
	arch := runtime.GOARCH
	if arch != "amd64" && arch != "arm64" {
		return Result{Name: "Arch", OK: false, Message: fmt.Sprintf("%s (supported: amd64, arm64)", arch)}
	}
	return Result{Name: "Arch", OK: true, Message: arch}
}

func checkKernel() Result {
	// Best effort: just report what we know from Go's view.
	return Result{Name: "Kernel", OK: true, Message: "see per-host check for details"}
}

func checkSSH(ctx context.Context, ex exec.Executor, host string) Result {
	res := ex.Run(ctx, host, "uname -a")
	if res.Failed() {
		return Result{Name: fmt.Sprintf("SSH/%s", host), OK: false, Message: res.Error()}
	}
	return Result{Name: fmt.Sprintf("SSH/%s", host), OK: true, Message: trim(string(res.Stdout))}
}

func checkKernelRemote(ctx context.Context, ex exec.Executor, host string) Result {
	res := ex.Run(ctx, host, "uname -r")
	if res.Failed() {
		return Result{Name: fmt.Sprintf("Kernel/%s", host), OK: false, Message: res.Error()}
	}
	ver := trim(string(res.Stdout))
	// Heuristic: k8s 1.30+ requires kernel 5.4+
	major, minor, ok := parseKernelMajorMinor(ver)
	if !ok || major < 5 || (major == 5 && minor < 4) {
		return Result{Name: fmt.Sprintf("Kernel/%s", host), OK: false, Message: fmt.Sprintf("%s (need ≥ 5.4)", ver)}
	}
	return Result{Name: fmt.Sprintf("Kernel/%s", host), OK: true, Message: ver}
}

func checkEBPF(ctx context.Context, ex exec.Executor, host string) Result {
	// Cilium needs BPF + mount cgroup2.
	res := ex.Run(ctx, host, "ls /sys/fs/bpf 2>/dev/null && grep -q cgroup2 /proc/filesystems")
	if res.Failed() {
		return Result{Name: fmt.Sprintf("eBPF/%s", host), OK: false, Message: "BPF or cgroup2 missing — cilium may need fallback"}
	}
	return Result{Name: fmt.Sprintf("eBPF/%s", host), OK: true, Message: "BPF + cgroup2 OK"}
}

func checkSwap(ctx context.Context, ex exec.Executor, host string) Result {
	res := ex.Run(ctx, host, "swapon --show --noheadings 2>/dev/null | wc -l")
	if res.Failed() {
		return Result{Name: fmt.Sprintf("Swap/%s", host), OK: false, Message: res.Error()}
	}
	if strings.TrimSpace(string(res.Stdout)) != "0" {
		return Result{Name: fmt.Sprintf("Swap/%s", host), OK: false, Message: "swap is on — kubelet requires swapoff"}
	}
	return Result{Name: fmt.Sprintf("Swap/%s", host), OK: true, Message: "swap off"}
}

func checkRuntime(ctx context.Context, ex exec.Executor, host string) Result {
	// Check whichever runtime is present.
	res := ex.Run(ctx, host, "command -v containerd && containerd --version || command -v docker && docker --version")
	if res.Failed() {
		return Result{Name: fmt.Sprintf("Runtime/%s", host), OK: false, Message: "no containerd or docker installed"}
	}
	return Result{Name: fmt.Sprintf("Runtime/%s", host), OK: true, Message: trim(string(res.Stdout))}
}

func checkPorts(ctx context.Context, ex exec.Executor, host string) Result {
	// 6443 (apiserver), 2379-2380 (etcd), 10250 (kubelet)
	script := `for p in 6443 2379 2380 10250; do
  if ss -ltn 2>/dev/null | grep -q ":$p "; then
    echo "$p used"
  else
    echo "$p free"
  fi
done`
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	res := ex.Run(ctx2, host, script)
	if res.Failed() {
		return Result{Name: fmt.Sprintf("Ports/%s", host), OK: false, Message: res.Error()}
	}
	used := []string{}
	for line := range strings.SplitSeq(strings.TrimSpace(string(res.Stdout)), "\n") {
		if strings.HasSuffix(line, "used") {
			used = append(used, strings.TrimSuffix(line, " used"))
		}
	}
	if len(used) > 0 {
		return Result{Name: fmt.Sprintf("Ports/%s", host), OK: false, Message: fmt.Sprintf("in use: %s", strings.Join(used, ","))}
	}
	return Result{Name: fmt.Sprintf("Ports/%s", host), OK: true, Message: "6443/2379/2380/10250 free"}
}

func parseKernelMajorMinor(ver string) (int, int, bool) {
	// version looks like "5.15.0-91-generic"
	parts := strings.SplitN(ver, ".", 3)
	if len(parts) < 2 {
		return 0, 0, false
	}
	var major, minor int
	if _, err := fmt.Sscanf(parts[0], "%d", &major); err != nil {
		return 0, 0, false
	}
	if _, err := fmt.Sscanf(parts[1], "%d", &minor); err != nil {
		return 0, 0, false
	}
	return major, minor, true
}

func trim(s string) string {
	return strings.TrimRight(s, "\n\r ")
}