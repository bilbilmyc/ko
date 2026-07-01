package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/ko-build/ko/internal/cluster"
	"github.com/ko-build/ko/internal/doctor"
	"github.com/ko-build/ko/pkg/config"
)

func newDoctorCmd() *cobra.Command {
	var noSSH bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Run preflight checks on the local host and (if --config is set) remote nodes",
		RunE: func(cmd *cobra.Command, args []string) error {
			results := doctor.LocalChecks()
			if !noSSH && flags.ConfigPath != "" {
				results = append(results, runRemoteChecks(flags.ConfigPath)...)
			}
			failed := 0
			cmd.Println("ko doctor:")
			for _, r := range results {
				mark := "✓"
				if !r.OK {
					mark = "✗"
				}
				cmd.Printf("  %s  %-32s  %s\n", mark, r.Name, r.Message)
				if !r.OK {
					failed++
				}
			}
			cmd.Println()
			if failed > 0 {
				cmd.Printf("%d check(s) failed\n", failed)
				return fmt.Errorf("doctor: %d failures", failed)
			}
			cmd.Println("all checks passed")
			return nil
		},
	}
	cmd.Flags().BoolVar(&noSSH, "no-ssh", false, "skip remote host checks (only check local host)")
	return cmd
}

func runRemoteChecks(path string) []doctor.Result {
	cfg, err := config.ParseFile(path)
	if err != nil {
		return []doctor.Result{{Name: "Config", OK: false, Message: err.Error()}}
	}
	cfg.ApplyDefaults()

	hosts := append([]string{}, cfg.Nodes.Masters...)
	hosts = append(hosts, cfg.Nodes.Workers...)

	sshCfg := cluster.SSHConfig{
		User:     cfg.Nodes.SSH.User,
		Port:     cfg.Nodes.SSH.Port,
		KeyFile:  cfg.Nodes.SSH.KeyFile,
		Password: cfg.Nodes.SSH.Password,
		Timeout:  10 * time.Second,
	}
	ex, err := cluster.NewSSHExecutor(sshCfg)
	if err != nil {
		return []doctor.Result{{Name: "SSH init", OK: false, Message: err.Error()}}
	}
	defer ex.Close()

	results := make([]doctor.Result, 0, len(hosts)*5)
	for _, h := range hosts {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		results = append(results, doctor.RemoteChecks(ctx, ex, h)...)
		cancel()
	}
	return results
}