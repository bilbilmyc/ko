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
	"github.com/ko-build/ko/internal/etcd"
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
	cmd.AddCommand(newClusterRestoreCmd())
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
		Short: "Take an etcd snapshot (supports stacked + external modes)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, ex, masters, _, closer, err := loadClusterContext(cmd)
			if err != nil {
				return err
			}
			defer closer()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()

			if cluster.IsExternalEtcd(cfg) {
				members := cluster.EtcdExternalMembers(cfg)
				if len(members) == 0 {
					return fmt.Errorf("etcd.mode=external but no members declared")
				}
				bs := etcd.NewBackupService(ex)
				if cfg.Etcd.PKIDir != "" {
					bs.PKIDir = cfg.Etcd.PKIDir
				}
				cmd.Printf("external etcd: snapshotting %d member(s)\n", len(members))
				for _, m := range members {
					local, err := bs.Snapshot(ctx, m.Host, m.Name)
					if err != nil {
						return fmt.Errorf("snapshot %s: %w", m.Name, err)
					}
					cmd.Printf("  ✓ %s -> %s\n", m.Name, local)
				}
				return nil
			}

			if len(masters) == 0 {
				return fmt.Errorf("no masters in config")
			}
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

func newClusterRestoreCmd() *cobra.Command {
	var (
		snapshot string
		yes      bool
	)
	cmd := &cobra.Command{
		Use:   "restore",
		Short: "Restore etcd from a snapshot — DESTRUCTIVE, requires --yes",
		Long: `Restore replaces every etcd member's data directory with the contents
of a snapshot file. It stops the existing etcd (and kubelet, in stacked mode),
moves the live data dir aside, and runs etcdctl snapshot restore on every
member. Use with care: post-restore, any data written after the snapshot is
gone.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if snapshot == "" {
				return fmt.Errorf("--snapshot=<path> required")
			}
			if !yes {
				cmd.Println("WARNING: this will overwrite etcd data on every member.")
				cmd.Println("The current data dir will be moved aside (suffix .broken-<ts>)")
				cmd.Println("but it WILL NOT be readable as a live etcd anymore.")
				cmd.Print("Type 'yes' to continue: ")
				var confirm string
				fmt.Scanln(&confirm)
				if confirm != "yes" {
					cmd.Println("aborted.")
					return nil
				}
			}
			cfg, ex, masters, _, closer, err := loadClusterContext(cmd)
			if err != nil {
				return err
			}
			defer closer()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()

			if cluster.IsExternalEtcd(cfg) {
				members := cluster.EtcdExternalMembers(cfg)
				if len(members) == 0 {
					return fmt.Errorf("etcd.mode=external but no members declared")
				}
				svc := etcd.NewService(ex, "", "v"+etcd.DefaultVersion, etcd.ClusterConfig{
					Members:      members,
					ClusterToken: "ko-etcd-" + cfg.Cluster.Name,
					PKIDir:       cfg.Etcd.PKIDir,
					InitialState: "existing",
				})
				initialCluster := svc.InitialCluster() // exported helper we add below
				bs := etcd.NewBackupService(ex)
				if cfg.Etcd.PKIDir != "" {
					bs.PKIDir = cfg.Etcd.PKIDir
				}
				cmd.Printf("external etcd: restoring %d member(s) from %s\n", len(members), snapshot)
				for _, m := range members {
					cmd.Printf("  → %s (%s)\n", m.Name, m.Host)
					if err := bs.Restore(ctx, etcd.RestoreOptions{
						Member:         m,
						SnapshotPath:   snapshot,
						InitialCluster: initialCluster,
					}); err != nil {
						return fmt.Errorf("restore %s: %w", m.Name, err)
					}
				}
				cmd.Println("✓ external etcd restored on all members")
				return nil
			}

			if len(masters) == 0 {
				return fmt.Errorf("no masters in config")
			}
			td := cluster.NewTeardown(ex)
			cmd.Printf("stacked etcd: restoring %d master(s) from %s\n", len(masters), snapshot)
			if err := td.RestoreStackedEtcd(ctx, cluster.RestoreStackedOpts{
				SnapshotPath: snapshot,
				Masters:      masters,
			}); err != nil {
				return err
			}
			cmd.Println("✓ stacked etcd restored on all masters; apiserver is healthy")
			return nil
		},
	}
	cmd.Flags().StringVar(&snapshot, "snapshot", "", "path to local snapshot file")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip confirmation prompt")
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