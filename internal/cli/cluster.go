package cli

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ko-build/ko/internal/cluster"
	"github.com/ko-build/ko/internal/logger"
	"github.com/ko-build/ko/pkg/config"
)

func newResetCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "reset",
		Short: "Tear down the cluster (kubeadm reset + cleanup on every node)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, ex, masters, workers, closer, err := loadClusterContext(cmd)
			if err != nil {
				return err
			}
			defer closer()
			if !force {
				cmd.Println("WARNING: this will wipe all kubernetes state on every node.")
				cmd.Print("Type 'yes' to continue: ")
				var confirm string
				fmt.Scanln(&confirm)
				if confirm != "yes" {
					cmd.Println("aborted.")
					return nil
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
			defer cancel()
			td := cluster.NewTeardown(ex)
			if err := td.ResetAll(ctx, masters, workers); err != nil {
				return err
			}
			cmd.Printf("✓ cluster reset on %d master(s) and %d worker(s)\n", len(masters), len(workers))
			_ = cfg
			return nil
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "skip confirmation prompt")
	return cmd
}

func newClusterCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cluster",
		Short: "Cluster-level operations",
	}
	cmd.AddCommand(newClusterInfoCmd())
	cmd.AddCommand(newClusterCertsCmd())
	cmd.AddCommand(newClusterBackupCmd())
	return cmd
}

func newClusterInfoCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "info",
		Short: "Show cluster info (name, version, apiserver, node count)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, _, masters, workers, closer, err := loadClusterContext(cmd)
			if err != nil {
				return err
			}
			defer closer()
			cmd.Printf("name:      %s\n", cfg.Cluster.Name)
			cmd.Printf("version:   %s\n", cfg.Cluster.Version)
			cmd.Printf("pod-cidr:  %s\n", cfg.Cluster.CIDR)
			cmd.Printf("svc-cidr:  %s\n", cfg.Cluster.SVCCIDR)
			cmd.Printf("masters:   %d (%s)\n", len(masters), joinShort(masters))
			cmd.Printf("workers:   %d (%s)\n", len(workers), joinShort(workers))
			if cfg.HA.VIP != "" {
				cmd.Printf("vip:       %s\n", cfg.HA.VIP)
				cmd.Printf("apiserver: https://%s:6443\n", cfg.HA.VIP)
			}
			cmd.Printf("runtime:   %s\n", cfg.Runtime.Default)
			cmd.Printf("cni:       %s\n", cfg.CNI.Plugin)
			return nil
		},
	}
	return cmd
}

func newClusterCertsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "certs",
		Short: "List certificate expiry for control-plane PKI",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, ex, masters, _, closer, err := loadClusterContext(cmd)
			if err != nil {
				return err
			}
			defer closer()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			td := cluster.NewTeardown(ex)
			certs, err := td.ListCertificates(ctx, masters)
			if err != nil {
				return err
			}
			sort.Slice(certs, func(i, j int) bool {
				return certs[i].NotAfter.Before(certs[j].NotAfter)
			})
			for _, c := range certs {
				days := int(time.Until(c.NotAfter).Hours() / 24)
				cmd.Printf("%-40s  %s  (%d days)  %s\n", c.Host, c.NotAfter.Format("2006-01-02"), days, c.Path)
			}
			return nil
		},
	}
	return cmd
}

func newClusterBackupCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Take an etcd snapshot (stacked mode only)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, ex, masters, _, closer, err := loadClusterContext(cmd)
			if err != nil {
				return err
			}
			defer closer()
			if cfg.Etcd.Mode != "stacked" {
				return fmt.Errorf("backup only supports stacked etcd mode (current: %q)", cfg.Etcd.Mode)
			}
			if len(masters) == 0 {
				return fmt.Errorf("no masters in config")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			td := cluster.NewTeardown(ex)
			path, err := td.BackupEtcd(ctx, masters[0])
			if err != nil {
				return err
			}
			cmd.Printf("✓ etcd snapshot saved: %s\n", path)
			return nil
		},
	}
	return cmd
}

func loadClusterContext(_ *cobra.Command) (*config.File, cluster.Executor, []string, []string, func(), error) {
	cfgPath := flags.ConfigPath
	if cfgPath == "" {
		return nil, nil, nil, nil, nil, fmt.Errorf("--config is required")
	}
	cfg, err := config.ParseFile(cfgPath)
	if err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("parse %q: %w", cfgPath, err)
	}
	cfg.ApplyDefaults()

	sshCfg := cluster.SSHConfig{
		User: cfg.Nodes.SSH.User, Port: cfg.Nodes.SSH.Port,
		KeyFile: cfg.Nodes.SSH.KeyFile, Password: cfg.Nodes.SSH.Password,
		Timeout: 60 * time.Second,
	}
	ex, err := cluster.NewSSHExecutor(sshCfg)
	if err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("ssh executor: %w", err)
	}
	logger.Info("loaded cluster context", "name", cfg.Cluster.Name, "masters", len(cfg.Nodes.Masters), "workers", len(cfg.Nodes.Workers))
	return cfg, ex, cfg.Nodes.Masters, cfg.Nodes.Workers, func() { _ = ex.Close() }, nil
}

func joinShort(s []string) string {
	if len(s) == 0 {
		return "-"
	}
	if len(s) <= 3 {
		var b strings.Builder
		for i, h := range s {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(h)
		}
		return b.String()
	}
	var b strings.Builder
	b.WriteString(s[0])
	b.WriteString(", ")
	b.WriteString(s[1])
	b.WriteString(", … (+")
	b.WriteString(strconv.Itoa(len(s) - 2))
	b.WriteByte(')')
	return b.String()
}