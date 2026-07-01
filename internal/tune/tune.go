package tune

import (
	"context"
	"fmt"
	"maps"

	"github.com/ko-build/ko/internal/exec"
	"github.com/ko-build/ko/internal/logger"
)

// Config describes what ko tune will apply / show / reset.
type Config struct {
	Profile     string
	SwapOff     bool
	Sysctl      map[string]string
	KernelModules []string
}

// Resolved merges the named profile with explicit overrides from Cfg.Tune
// block. Explicit keys win over profile keys.
func (c Config) Resolved() (*Profile, error) {
	p := LookupProfile(c.Profile)
	if p == nil {
		return nil, fmt.Errorf("unknown profile %q (available: production, dev, minimal)", c.Profile)
	}
	out := Profile{
		Name:    p.Name,
		Sysctl:  map[string]string{},
		Modules: append([]string(nil), p.Modules...),
	}
	maps.Copy(out.Sysctl, p.Sysctl)
	maps.Copy(out.Sysctl, c.Sysctl)
	out.Modules = append(out.Modules, c.KernelModules...)
	return &out, nil
}

// Apply runs swapoff (if requested), modules, then sysctl on each host.
func Apply(ctx context.Context, ex exec.Executor, hosts []string, cfg Config) error {
	p, err := cfg.Resolved()
	if err != nil {
		return err
	}
	for _, h := range hosts {
		logger.Info("tuning host", "host", h, "profile", p.Name)
		if cfg.SwapOff {
			if err := ApplySwapOff(ctx, ex, h); err != nil {
				return fmt.Errorf("%s: swapoff: %w", h, err)
			}
		}
		if len(p.Modules) > 0 {
			if err := ApplyModules(ctx, ex, h, p.Modules); err != nil {
				return fmt.Errorf("%s: modules: %w", h, err)
			}
		}
		if len(p.Sysctl) > 0 {
			if err := ApplySysctl(ctx, ex, h, p.Sysctl); err != nil {
				return fmt.Errorf("%s: sysctl: %w", h, err)
			}
		}
	}
	return nil
}

// Show returns a per-host map of current sysctl values.
func Show(ctx context.Context, ex exec.Executor, hosts []string, cfg Config) (map[string]map[string]string, error) {
	p, err := cfg.Resolved()
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(p.Sysctl))
	for k := range p.Sysctl {
		keys = append(keys, k)
	}
	out := map[string]map[string]string{}
	for _, h := range hosts {
		vals, err := CurrentSysctl(ctx, ex, h, keys)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", h, err)
		}
		out[h] = vals
	}
	return out, nil
}

// Reset removes the ko-managed sysctl + modules files from each host.
func Reset(ctx context.Context, ex exec.Executor, hosts []string) error {
	for _, h := range hosts {
		if err := ResetSysctl(ctx, ex, h); err != nil {
			return err
		}
		if err := ResetModules(ctx, ex, h); err != nil {
			return err
		}
	}
	return nil
}