package cli

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/ko-build/ko/internal/cluster"
	"github.com/ko-build/ko/internal/logger"
	"github.com/ko-build/ko/internal/tune"
	"github.com/ko-build/ko/pkg/config"
)

func newTuneCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tune",
		Short: "Apply host tuning profiles",
	}
	cmd.AddCommand(newTuneApplyCmd())
	cmd.AddCommand(newTuneShowCmd())
	cmd.AddCommand(newTuneResetCmd())
	return cmd
}

func newTuneApplyCmd() *cobra.Command {
	var profile string
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Apply tuning profile to all cluster nodes",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, ex, hosts, closer, err := loadTuneContext(cmd)
			if err != nil {
				return err
			}
			defer closer()
			if profile == "" {
				profile = cfg.Tune.Profile
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if err := tune.Apply(ctx, ex, hosts, tune.Config{
				Profile:       profile,
				SwapOff:       cfg.Tune.SwapOff,
				Sysctl:        cfg.Tune.Sysctl,
				KernelModules: cfg.Tune.KernelModules,
			}); err != nil {
				return err
			}
			cmd.Printf("✓ tuned %d host(s) with profile=%s\n", len(hosts), profile)
			return nil
		},
	}
	cmd.Flags().StringVar(&profile, "profile", "", "override profile: production | dev | minimal")
	return cmd
}

func newTuneShowCmd() *cobra.Command {
	var profile string
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Show current vs desired sysctl values",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, ex, hosts, closer, err := loadTuneContext(cmd)
			if err != nil {
				return err
			}
			defer closer()
			if profile == "" {
				profile = cfg.Tune.Profile
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			out, err := tune.Show(ctx, ex, hosts, tune.Config{
				Profile:       profile,
				SwapOff:       cfg.Tune.SwapOff,
				Sysctl:        cfg.Tune.Sysctl,
				KernelModules: cfg.Tune.KernelModules,
			})
			if err != nil {
				return err
			}
			for _, h := range hosts {
				cmd.Printf("== %s ==\n", h)
				keys := make([]string, 0, len(out[h]))
				for k := range out[h] {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					cmd.Printf("  %s = %s\n", k, out[h][k])
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&profile, "profile", "", "override profile: production | dev | minimal")
	return cmd
}

func newTuneResetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reset",
		Short: "Remove ko-managed sysctl + modules files",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, ex, hosts, closer, err := loadTuneContext(cmd)
			if err != nil {
				return err
			}
			defer closer()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			if err := tune.Reset(ctx, ex, hosts); err != nil {
				return err
			}
			cmd.Printf("✓ reset tuning on %d host(s)\n", len(hosts))
			return nil
		},
	}
	return cmd
}

func loadTuneContext(_ *cobra.Command) (*config.File, cluster.Executor, []string, func(), error) {
	cfgPath := flags.ConfigPath
	if cfgPath == "" {
		return nil, nil, nil, nil, fmt.Errorf("--config is required")
	}
	cfg, err := config.ParseFile(cfgPath)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("parse %q: %w", cfgPath, err)
	}
	cfg.ApplyDefaults()

	sshCfg := cluster.SSHConfig{
		User: cfg.Nodes.SSH.User, Port: cfg.Nodes.SSH.Port,
		KeyFile: cfg.Nodes.SSH.KeyFile, Password: cfg.Nodes.SSH.Password,
		Timeout: 60 * time.Second,
	}
	ex, err := cluster.NewSSHExecutor(sshCfg)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("ssh executor: %w", err)
	}

	hosts := append([]string{}, cfg.Nodes.Masters...)
	hosts = append(hosts, cfg.Nodes.Workers...)
	if len(hosts) == 0 {
		_ = ex.Close()
		return nil, nil, nil, nil, fmt.Errorf("no hosts in config")
	}
	logger.Info("tuning hosts", "count", len(hosts))
	return cfg, ex, hosts, func() { _ = ex.Close() }, nil
}