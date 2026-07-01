package cli

import (
	"runtime"

	"github.com/spf13/cobra"
)

func newArchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "arch",
		Short: "Print the architecture of the current ko binary",
		Run: func(cmd *cobra.Command, args []string) {
			cmd.Printf("%s/%s\n", runtime.GOOS, runtime.GOARCH)
		},
	}
}
