package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/ko-build/ko/internal/cluster"
	"github.com/ko-build/ko/internal/etcd"
	"github.com/ko-build/ko/internal/logger"
	"github.com/ko-build/ko/pkg/config"
)

// newEtcdCmd is the parent for the `ko etcd ...` command family. It works
// for both stacked and external modes, but in stacked mode most
// sub-commands are no-ops (the snapshot lives on the masters, not on
// separate members).
func newEtcdCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "etcd",
		Short: "External etcd lifecycle (install / status / backup / uninstall)",
		Long: `Manage the external etcd cluster declared under the ` + "`etcd`" + ` block
of your cluster.hcl. Only meaningful when ` + "`etcd.mode = \"external\"`" + `.

Sub-commands:
  install    — download etcd, generate mTLS PKI, install systemd unit on every member
  status     — show member active state + endpoint health
  backup     — manual snapshot (the 8h timer does this automatically)
  uninstall  — stop the unit, remove PKI + data (BACKUPS LEFT IN PLACE)

In stacked etcd mode (default), most of these are no-ops. Use
` + "`ko cluster backup`" + ` for stacked-mode snapshots.`,
	}
	cmd.AddCommand(newEtcdInstallCmd())
	cmd.AddCommand(newEtcdStatusCmd())
	cmd.AddCommand(newEtcdBackupCmd())
	cmd.AddCommand(newEtcdUninstallCmd())
	return cmd
}

// loadEtcdContext parses cluster.hcl and returns the parsed config + an
// SSH executor. Used by every sub-command. Refuses to run on stacked mode.
func loadEtcdContext(_ *cobra.Command) (*config.File, cluster.Executor, []string, func(), error) {
	cfg, ex, masters, workers, closer, err := loadClusterContext(nil)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if cfg.Etcd.Mode != "external" {
		closer()
		return nil, nil, nil, nil, fmt.Errorf("ko etcd ... requires etcd.mode=external (current: %q)", cfg.Etcd.Mode)
	}
	_ = workers
	return cfg, ex, masters, closer, nil
}

func newEtcdInstallCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install etcd + systemd on every member declared in cluster.hcl",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, ex, masters, closer, err := loadEtcdContext(cmd)
			if err != nil {
				return err
			}
			defer closer()

			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
			defer cancel()
			if err := cluster.ProvisionExternalEtcd(ctx, cfg, ex, masters); err != nil {
				return err
			}
			members := cluster.EtcdExternalMembers(cfg)
			cmd.Println("✓ etcd installed on", len(members), "member(s)")
			for _, m := range members {
				cmd.Printf("  - %s@%s  (pki: %s)\n", m.Name, m.Host, cfg.Etcd.PKIDir)
			}
			return nil
		},
	}
	return cmd
}

func newEtcdStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show member active state and endpoint health",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, ex, _, closer, err := loadEtcdContext(cmd)
			if err != nil {
				return err
			}
			defer closer()

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			members := cluster.EtcdExternalMembers(cfg)
			// Build a Service just for Status(); tarball can be empty for status checks.
			svc := etcd.NewService(ex, "", "v"+trimV(etcd.DefaultVersion), etcd.ClusterConfig{
				Members:      members,
				ClusterToken: "ko-etcd-" + cfg.Cluster.Name,
				PKIDir:       cfg.Etcd.PKIDir,
			})
			statuses, err := svc.Status(ctx, members)
			if err != nil {
				return err
			}
			cmd.Printf("%-20s  %-12s  %-12s  %s\n", "MEMBER", "ACTIVE", "HEALTH", "HOST")
			for _, s := range statuses {
				cmd.Printf("%-20s  %-12s  %-12s  %s\n", s.Name, s.Active, s.EndpointHealth, s.Host)
			}
			return nil
		},
	}
	return cmd
}

func newEtcdBackupCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Take an on-demand snapshot (the 8h timer also does this automatically)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, ex, _, closer, err := loadEtcdContext(cmd)
			if err != nil {
				return err
			}
			defer closer()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			members := cluster.EtcdExternalMembers(cfg)
			backup := etcd.NewBackupService(ex)
			backup.PKIDir = cfg.Etcd.PKIDir
			for _, m := range members {
				local, err := backup.Snapshot(ctx, m.Host, m.Name)
				if err != nil {
					logger.Warn("snapshot failed", "host", m.Host, "err", err)
					continue
				}
				cmd.Println("✓", local)
			}
			return nil
		},
	}
	return cmd
}

func newEtcdUninstallCmd() *cobra.Command {
	var purgeBackups bool
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Stop systemd unit, remove PKI + data on every member",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, ex, _, closer, err := loadEtcdContext(cmd)
			if err != nil {
				return err
			}
			defer closer()

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			if err := cluster.UninstallExternalEtcd(ctx, cfg, ex, purgeBackups); err != nil {
				return err
			}
			members := cluster.EtcdExternalMembers(cfg)
			cmd.Println("✓ etcd uninstalled from", len(members), "member(s)")
			return nil
		},
	}
	cmd.Flags().BoolVar(&purgeBackups, "purge-backups", false, "also delete the 14d backup history under /var/backups/etcd")
	return cmd
}

// trimV strips a leading "v" from a version string, used when feeding
// versions into URL templates that already include the prefix.
func trimV(s string) string {
	if len(s) > 0 && s[0] == 'v' {
		return s[1:]
	}
	return s
}
