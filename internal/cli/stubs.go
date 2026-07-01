package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/ko-build/ko/internal/cluster"
	"github.com/ko-build/ko/internal/logger"
	"github.com/ko-build/ko/pkg/config"
)

func newInitCmd() *cobra.Command {
	var (
		runtimeFlag string
		offline     bool
		bundle      string
	)
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize a Kubernetes cluster on the first master node",
		Long: `init runs the full single-master bootstrap on the first host listed
in ` + "`nodes.masters`" + ` (or whichever host you pass via --master). It installs the
configured container runtime, bootstraps kubeadm, runs ` + "`kubeadm init`" + `, then
installs kube-vip and Cilium with kube-proxy replacement.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfgPath := flags.ConfigPath
			if cfgPath == "" {
				return fmt.Errorf("--config is required")
			}
			cfg, err := config.ParseFile(cfgPath)
			if err != nil {
				return fmt.Errorf("parse %q: %w", cfgPath, err)
			}
			cfg.ApplyDefaults()
			if runtimeFlag != "" {
				cfg.Runtime.Default = runtimeFlag
			}
			master := ""
			if len(args) > 0 {
				master = args[0]
			} else if len(cfg.Nodes.Masters) > 0 {
				master = cfg.Nodes.Masters[0]
			}
			if master == "" {
				return fmt.Errorf("no master host: provide one as an argument or set nodes.masters in %s", cfgPath)
			}

			sshCfg := cluster.SSHConfig{
				User:     cfg.Nodes.SSH.User,
				Port:     cfg.Nodes.SSH.Port,
				KeyFile:  cfg.Nodes.SSH.KeyFile,
				Password: cfg.Nodes.SSH.Password,
				Timeout:  60 * time.Second,
			}
			exec, err := cluster.NewSSHExecutor(sshCfg)
			if err != nil {
				return fmt.Errorf("init ssh executor: %w", err)
			}
			defer exec.Close()

			init, err := cluster.NewInitFromConfig(cfg, exec)
			if err != nil {
				return err
			}
			init.Offline = offline || flags.Offline
			init.BundlePath = bundle

			logger.Info("starting init", "master", master, "runtime", cfg.Runtime.Default, "offline", init.Offline)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			if err := init.Run(ctx, master); err != nil {
				logger.Error("init failed", "err", err)
				return err
			}
			cmd.Println("✓ cluster initialized")
			cmd.Println("  kubeconfig:", filepath.Join(homeDir(), ".ko", "kube", "admin.conf"))
			if cfg.HA.VIP != "" {
				cmd.Printf("  apiserver:  https://%s:6443\n", cfg.HA.VIP)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&runtimeFlag, "runtime", "", "override runtime: containerd | docker")
	cmd.Flags().BoolVar(&offline, "offline", false, "init from local bundle (use --bundle or --offline global)")
	cmd.Flags().StringVar(&bundle, "bundle", "", "path to ko offline OCI image (when --offline)")
	return cmd
}

func newResetCmd() *cobra.Command {
	return stubCmd("reset", "Tear down a cluster and clean all nodes (S6)")
}

func homeDir() string {
	h, _ := os.UserHomeDir()
	return h
}

func newNodeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "node",
		Short: "Manage cluster nodes (S4)",
	}
	cmd.AddCommand(stubCmd("add", "Add a node to the cluster (S4)"))
	cmd.AddCommand(stubCmd("remove", "Remove a node from the cluster (S4)"))
	cmd.AddCommand(stubCmd("list", "List cluster nodes (S4)"))
	cmd.AddCommand(stubCmd("label", "Label a node (S4)"))
	return cmd
}

func newTuneCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tune",
		Short: "Apply host tuning profiles (S5)",
	}
	cmd.AddCommand(stubCmd("apply", "Apply tuning profile (S5)"))
	cmd.AddCommand(stubCmd("show", "Show current vs desired tuning (S5)"))
	cmd.AddCommand(stubCmd("reset", "Revert tuning to defaults (S5)"))
	return cmd
}

func newClusterCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cluster",
		Short: "Cluster-level operations (S6)",
	}
	cmd.AddCommand(stubCmd("info", "Show cluster info (S6)"))
	cmd.AddCommand(stubCmd("certs", "List certificate expiry (S6)"))
	cmd.AddCommand(stubCmd("backup", "etcd snapshot (S6, stacked mode only)"))
	return cmd
}

func newPackCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pack",
		Short: "Build / push / inspect offline bundles (S7)",
	}
	cmd.AddCommand(stubCmd("build", "Build offline OCI bundle (S7)"))
	cmd.AddCommand(stubCmd("push", "Push bundle to registry (S7)"))
	cmd.AddCommand(stubCmd("inspect", "Show bundle contents (S7)"))
	return cmd
}

func newDashboardCmd() *cobra.Command {
	return stubCmd("dashboard", "Launch the Web Dashboard (S9)")
}

func newCompletionCmd() *cobra.Command {
	return &cobra.Command{
		Use:       "completion <bash|zsh|fish>",
		Short:     "Generate shell completion script (S1)",
		ValidArgs: []string{"bash", "zsh", "fish"},
		Args:      cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			shell := args[0]
			switch shell {
			case "bash":
				return cmd.Root().GenBashCompletion(cmd.OutOrStdout())
			case "zsh":
				return cmd.Root().GenZshCompletion(cmd.OutOrStdout())
			case "fish":
				return cmd.Root().GenFishCompletion(cmd.OutOrStdout(), true)
			}
			return nil
		},
	}
}

func stubCmd(use, desc string) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: desc,
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.Printf("not yet implemented: %s\n", use)
			return nil
		},
	}
}
