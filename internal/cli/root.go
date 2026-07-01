package cli

import (
	"github.com/spf13/cobra"

	"github.com/ko-build/ko/internal/logger"
	"github.com/ko-build/ko/internal/version"
)

type GlobalFlags struct {
	Verbose bool
	LogLevel string
	ConfigPath string
	Offline bool
}

var flags GlobalFlags

func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "ko",
		Short:         "Kubernetes cluster lifecycle tool (offline-first)",
		Long:          "ko manages K8s clusters end-to-end: init / node lifecycle / tune / release / dashboard.\nAll operations work offline once a bundle is loaded.",
		Version:       version.Full(),
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			lvl, _ := logger.ParseLevel(flags.LogLevel)
			if flags.Verbose {
				lvl = logger.LevelDebug
			}
			logger.SetLevel(lvl)
			return nil
		},
	}

	root.PersistentFlags().BoolVarP(&flags.Verbose, "verbose", "v", false, "verbose output (debug logging)")
	root.PersistentFlags().StringVar(&flags.LogLevel, "log-level", "info", "log level: debug|info|warn|error")
	root.PersistentFlags().StringVar(&flags.ConfigPath, "config", "", "path to cluster.hcl config file")
	root.PersistentFlags().BoolVar(&flags.Offline, "offline", false, "force offline mode (use local bundle only)")

	root.AddCommand(newVersionCmd())
	root.AddCommand(newArchCmd())
	root.AddCommand(newDoctorCmd())
	root.AddCommand(newInitCmd())
	root.AddCommand(newResetCmd())
	root.AddCommand(newNodeCmd())
	root.AddCommand(newTuneCmd())
	root.AddCommand(newClusterCmd())
	root.AddCommand(newPackCmd())
	root.AddCommand(newDashboardCmd())
	root.AddCommand(newCompletionCmd())

	return root
}
