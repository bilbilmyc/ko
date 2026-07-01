// Package tune applies host-level tuning (sysctl, kernel modules, swap, etc.)
// across the nodes of a cluster. Every operation is idempotent and writes to a
// `ko-*.conf` file under /etc so ko's footprint is easy to audit and reset.
package tune

import (
	"context"
	"fmt"
	"strings"

	"github.com/ko-build/ko/internal/exec"
)

// SysctlPath is where ko writes its sysctl overrides on each host.
const SysctlPath = "/etc/sysctl.d/99-ko.conf"

// ModulesPath is where ko writes its module-load list.
const ModulesPath = "/etc/modules-load.d/99-ko.conf"

// SwapOff disables swap and comments out any swap entries in /etc/fstab.
const SwapOffScript = `set -euo pipefail
if [ "$(swapon -v 2>/dev/null | wc -l)" -gt 0 ]; then
  swapoff -a
fi
# Comment out remaining swap entries in fstab
if [ -f /etc/fstab ]; then
  sed -i.bak 's/^\([^#].*[[:space:]]swap[[:space:]].*$\)/# \1/' /etc/fstab
fi
`

// ApplySysctl writes the given key/value pairs to /etc/sysctl.d/99-ko.conf and
// runs `sysctl --system` to reload.
func ApplySysctl(ctx context.Context, ex exec.Executor, host string, kv map[string]string) error {
	body := renderSysctl(kv)
	cmd := fmt.Sprintf("mkdir -p /etc/sysctl.d && cat > %s <<'KO_SYSCTL_EOF'\n%s\nKO_SYSCTL_EOF\nsysctl --system", SysctlPath, body)
	res := ex.Run(ctx, host, cmd)
	if res.Failed() {
		return fmt.Errorf("write %s: %w", SysctlPath, res.Err)
	}
	return nil
}

// CurrentSysctl reads the current effective sysctl values via `sysctl -n`.
func CurrentSysctl(ctx context.Context, ex exec.Executor, host string, keys []string) (map[string]string, error) {
	keyList := strings.Join(keys, " ")
	res := ex.Run(ctx, host, fmt.Sprintf("sysctl -n %s 2>/dev/null", keyList))
	if res.Failed() {
		return nil, fmt.Errorf("sysctl -n: %w", res.Err)
	}
	lines := strings.Split(strings.TrimRight(string(res.Stdout), "\n"), "\n")
	out := map[string]string{}
	for i, k := range keys {
		if i < len(lines) {
			out[k] = strings.TrimSpace(lines[i])
		}
	}
	return out, nil
}

// ResetSysctl removes the ko sysctl file and reloads.
func ResetSysctl(ctx context.Context, ex exec.Executor, host string) error {
	res := ex.Run(ctx, host, fmt.Sprintf("rm -f %s && sysctl --system", SysctlPath))
	if res.Failed() {
		return fmt.Errorf("reset sysctl: %w", res.Err)
	}
	return nil
}

func renderSysctl(kv map[string]string) string {
	var b strings.Builder
	for k, v := range kv {
		fmt.Fprintf(&b, "%s = %s\n", k, v)
	}
	return b.String()
}