package tune

import (
	"context"
	"fmt"

	"github.com/ko-build/ko/internal/exec"
)

// ApplySwapOff runs `swapoff -a` and comments swap entries in /etc/fstab.
// Kubernetes requires swap to be off (kubelet refuses to start with swap on).
func ApplySwapOff(ctx context.Context, ex exec.Executor, host string) error {
	res := ex.Run(ctx, host, SwapOffScript)
	if res.Failed() {
		return fmt.Errorf("swapoff: %w", res.Err)
	}
	return nil
}

// SwapActive reports whether any swap is active on host.
func SwapActive(ctx context.Context, ex exec.Executor, host string) (bool, error) {
	res := ex.Run(ctx, host, "swapon --show --noheadings 2>/dev/null | wc -l")
	if res.Failed() {
		return false, fmt.Errorf("swapon: %w", res.Err)
	}
	return trimSpace(string(res.Stdout)) != "0", nil
}