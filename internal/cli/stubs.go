package cli

import "github.com/spf13/cobra"

// v0.0.1 S1: commands are stubs that print "not yet implemented" so the CLI surface is complete.
// Subsequent slices (S2–S11) replace these with real implementations.

func newInitCmd() *cobra.Command {
	return stubCmd("init", "Initialize a Kubernetes cluster (S2)")
}

func newResetCmd() *cobra.Command {
	return stubCmd("reset", "Tear down a cluster and clean all nodes (S6)")
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
