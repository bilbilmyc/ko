package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/ko-build/ko/internal/cluster"
	"github.com/ko-build/ko/internal/containerd"
	"github.com/ko-build/ko/internal/docker"
	"github.com/ko-build/ko/internal/logger"
	"github.com/ko-build/ko/pkg/config"
)

func newNodeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "node",
		Short: "Manage cluster nodes (add / remove / list / label)",
	}
	cmd.AddCommand(newNodeListCmd())
	cmd.AddCommand(newNodeAddCmd())
	cmd.AddCommand(newNodeRemoveCmd())
	cmd.AddCommand(newNodeLabelCmd())
	return cmd
}

// loadNodeLifecycle parses config, builds an SSH executor, and returns a
// NodeLifecycle ready to use. It assumes the cluster has already been
// initialised (kubeconfig present in KO_CACHE_HOME/kube/admin.conf).
func loadNodeLifecycle(_ *cobra.Command) (*cluster.NodeLifecycle, *config.File, func(), error) {
	cfgPath := flags.ConfigPath
	if cfgPath == "" {
		return nil, nil, nil, fmt.Errorf("--config is required")
	}
	cfg, err := config.ParseFile(cfgPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("parse %q: %w", cfgPath, err)
	}
	cfg.ApplyDefaults()

	sshCfg := cluster.SSHConfig{
		User:     cfg.Nodes.SSH.User,
		Port:     cfg.Nodes.SSH.Port,
		KeyFile:  cfg.Nodes.SSH.KeyFile,
		Password: cfg.Nodes.SSH.Password,
		Timeout:  60 * time.Second,
	}
	exec, err := cluster.NewSSHExecutor(sshCfg)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("init ssh executor: %w", err)
	}
	kb := cluster.NewKubeadm(exec)

	arch := ""
	cache := filepath.Join(homeDir(), ".ko", "cache", "containerd")
	ctdInstaller := containerd.NewInstaller(exec, cfg.Containerd.Version, cfg.Containerd.Source, arch, cache)
	dckInstaller := docker.NewInstaller(exec, "27.5.1", "stable")

	nl := cluster.NewNodeLifecycle(cfg, exec, kb, filepath.Join(homeDir(), ".ko", "kube", "admin.conf"))
	nl.InstallContainerd = func(ctx context.Context, host string) error {
		return ctdInstaller.Install(ctx, host, containerd.DefaultConfig(cfg.Image.RegistryMirrors, cfg.Image.InsecureRegistries))
	}
	nl.InstallDocker = dckInstaller.Install
	return nl, cfg, func() { _ = exec.Close() }, nil
}

func newNodeListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List cluster nodes",
		RunE: func(cmd *cobra.Command, args []string) error {
			nl, _, closer, err := loadNodeLifecycle(cmd)
			if err != nil {
				return err
			}
			defer closer()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			out, err := nl.List(ctx)
			if err != nil {
				return err
			}
			cmd.Println(out)
			return nil
		},
	}
	return cmd
}

func newNodeAddCmd() *cobra.Command {
	var asMaster bool
	cmd := &cobra.Command{
		Use:   "add <host>",
		Short: "Add a node (worker by default, --master for control-plane)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			nl, _, closer, err := loadNodeLifecycle(cmd)
			if err != nil {
				return err
			}
			defer closer()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
			defer cancel()
			host := args[0]
			if asMaster {
				logger.Info("adding master", "host", host)
				if err := nl.AddMaster(ctx, host); err != nil {
					return err
				}
				cmd.Printf("✓ master %s added\n", host)
			} else {
				logger.Info("adding worker", "host", host)
				if err := nl.AddWorker(ctx, host); err != nil {
					return err
				}
				cmd.Printf("✓ worker %s added\n", host)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asMaster, "master", false, "add as control-plane node (HA)")
	return cmd
}

func newNodeRemoveCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "remove <host>",
		Short: "Drain, delete, and reset a node",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			nl, _, closer, err := loadNodeLifecycle(cmd)
			if err != nil {
				return err
			}
			defer closer()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
			defer cancel()
			if err := nl.Remove(ctx, args[0], cluster.RemoveOptions{Force: force}); err != nil {
				return err
			}
			cmd.Printf("✓ node %s removed\n", args[0])
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "skip drain errors (use when host is unreachable)")
	return cmd
}

func newNodeLabelCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "label <host> <key>=<value>",
		Short: "Apply a label to a node",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			nl, _, closer, err := loadNodeLifecycle(cmd)
			if err != nil {
				return err
			}
			defer closer()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := nl.Label(ctx, args[0], splitKey(args[1]), splitValue(args[1])); err != nil {
				return err
			}
			cmd.Printf("✓ node %s labelled %s\n", args[0], args[1])
			return nil
		},
	}
	return cmd
}

func splitKey(kv string) string {
	for i, r := range kv {
		if r == '=' {
			return kv[:i]
		}
	}
	return kv
}

func splitValue(kv string) string {
	for i, r := range kv {
		if r == '=' {
			return kv[i+1:]
		}
	}
	return ""
}