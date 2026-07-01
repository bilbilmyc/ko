package cli

import (
	"github.com/spf13/cobra"

	"github.com/ko-build/ko/internal/version"
)

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print ko version information",
		Run: func(cmd *cobra.Command, args []string) {
			cmd.Println(version.Full())
		},
	}
}
