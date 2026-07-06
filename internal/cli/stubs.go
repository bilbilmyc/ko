package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ko-build/ko/internal/cluster"
	"github.com/ko-build/ko/internal/logger"
	"github.com/ko-build/ko/pkg/config"
)

func newInitCmd() *cobra.Command {
	var (
		runtimeFlag    string
		offline        bool
		bundle         string
		generateConfig string
		profile        string
		outputPath     string
		forceOverwrite bool
	)
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize a Kubernetes cluster on the first master node",
		Long: `init runs the full single-master bootstrap on the first host listed
in ` + "`nodes.masters`" + ` (or whichever host you pass via --master). It installs the
configured container runtime, bootstraps kubeadm, runs ` + "`kubeadm init`" + `, then
installs kube-vip and Cilium with kube-proxy replacement.

Use ` + "`--generate-config=PROFILE`" + ` to write a starter config file (sealos-style) instead
of running an actual init. Profiles: single | ha | external-etcd.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Branch 1: --generate-config (sealos-style). No SSH, no init —
			// we just write the chosen profile template and exit.
			if generateConfig != "" {
				return runGenerateConfig(cmd, generateConfig, profile, outputPath, forceOverwrite)
			}

			// v0.0.5: --offline without --bundle is a silent footgun.
			// Without a bundle, OfflineRunner.Run can't install containerd /
			// kubeadm / start the in-cluster registry; init would still
			// proceed but fail mid-way with a confusing `bundle is required`
			// error. Reject up front.
			if (offline || flags.Offline) && bundle == "" {
				return fmt.Errorf("--offline requires --bundle <path-to-oci-tar.gz>; see `ko pack build` to produce one")
			}

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
	// --generate-config is mutually exclusive with --config: it WRITES the
	// config file, doesn't read one. We still keep them separate flags so a
	// user can say `ko init --generate-config=ha --output ./prod.hcl` without
	// setting --config.
	cmd.Flags().StringVar(&generateConfig, "generate-config", "", "write a starter config file and exit (sealos-style). profiles: single|ha|external-etcd")
	cmd.Flags().StringVar(&profile, "profile", "", "optional profile selector when --generate-config is set (default: same as --generate-config)")
	cmd.Flags().StringVarP(&outputPath, "output", "o", "", "output path for --generate-config (default: ./cluster.hcl, or --config value)")
	cmd.Flags().BoolVarP(&forceOverwrite, "force", "f", false, "overwrite existing file when used with --generate-config")
	return cmd
}

// runGenerateConfig implements the sealos-style `ko init --generate-config`
// branch. It picks a profile, renders the embedded template, and writes
// the result via WriteAtomic.
func runGenerateConfig(cmd *cobra.Command, generate, profileFlag, outputPath string, force bool) error {
	// If --profile wasn't given, derive it from --generate-config.
	prof := profileFlag
	if prof == "" {
		prof = generate
	}
	if !config.IsValidProfile(prof) {
		return fmt.Errorf("invalid profile %q (want one of: %s)",
			prof, strings.Join(config.ListProfiles(), ", "))
	}

	// Resolve output path: --output wins, else --config, else ./cluster.hcl.
	out := outputPath
	if out == "" {
		out = flags.ConfigPath
	}
	if out == "" {
		out = "./cluster.hcl"
	}

	vars := config.DefaultVars()
	data, err := config.RenderTemplate(prof, vars)
	if err != nil {
		return fmt.Errorf("render %q: %w", prof, err)
	}

	n, err := config.WriteAtomic(out, data, force)
	if err != nil {
		return err
	}
	cmd.Printf("✓ wrote %s profile to %s (%d bytes)\n", prof, out, n)
	cmd.Println("next:")
	cmd.Println("  ko doctor --config", out)
	cmd.Println("  ko init   --config", out)
	return nil
}

func homeDir() string {
	h, _ := os.UserHomeDir()
	return h
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
