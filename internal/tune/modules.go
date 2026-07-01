package tune

import (
	"context"
	"fmt"
	"strings"

	"github.com/ko-build/ko/internal/exec"
)

// ApplyModules runs modprobe for each module and writes the list to
// /etc/modules-load.d/99-ko.conf so the modules load on boot.
func ApplyModules(ctx context.Context, ex exec.Executor, host string, modules []string) error {
	for _, m := range modules {
		res := ex.Run(ctx, host, fmt.Sprintf("modprobe %s 2>/dev/null || true", m))
		if res.Failed() {
			return fmt.Errorf("modprobe %s: %w", m, res.Err)
		}
	}
	var b strings.Builder
	for _, m := range modules {
		b.WriteString(m)
		b.WriteByte('\n')
	}
	body := b.String()
	cmd := fmt.Sprintf("mkdir -p /etc/modules-load.d && cat > %s <<'KO_MODULES_EOF'\n%sKO_MODULES_EOF\n", ModulesPath, body)
	res := ex.Run(ctx, host, cmd)
	if res.Failed() {
		return fmt.Errorf("write %s: %w", ModulesPath, res.Err)
	}
	return nil
}

// LoadedModules reports the set of kernel modules currently loaded.
func LoadedModules(ctx context.Context, ex exec.Executor, host string) ([]string, error) {
	res := ex.Run(ctx, host, "lsmod | awk 'NR>1 {print $1}'")
	if res.Failed() {
		return nil, fmt.Errorf("lsmod: %w", res.Err)
	}
	out := string(res.Stdout)
	var mods []string
	for _, line := range splitLines(out) {
		if line = trimSpace(line); line != "" {
			mods = append(mods, line)
		}
	}
	return mods, nil
}

// ResetModules removes the ko modules-load file (does not unload modules).
func ResetModules(ctx context.Context, ex exec.Executor, host string) error {
	res := ex.Run(ctx, host, fmt.Sprintf("rm -f %s", ModulesPath))
	if res.Failed() {
		return fmt.Errorf("reset modules: %w", res.Err)
	}
	return nil
}