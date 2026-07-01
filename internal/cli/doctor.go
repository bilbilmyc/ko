package cli

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/shirou/gopsutil/v3/disk"
	"github.com/spf13/cobra"

	"github.com/ko-build/ko/internal/cluster"
	"github.com/ko-build/ko/internal/logger"
	"github.com/ko-build/ko/pkg/config"
)

type checkResult struct {
	Name    string
	OK      bool
	Message string
}

func (c checkResult) String() string {
	mark := "✓"
	if !c.OK {
		mark = "✗"
	}
	return fmt.Sprintf("  %s  %-22s  %s", mark, c.Name, c.Message)
}

func newDoctorCmd() *cobra.Command {
	var noSSH bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Run preflight checks on the local host and (if --config is set) remote nodes",
		RunE: func(cmd *cobra.Command, args []string) error {
			results := runLocalChecks()
			if !noSSH && flags.ConfigPath != "" {
				results = append(results, runRemoteChecks(flags.ConfigPath)...)
			}
			failed := 0
			cmd.Println("ko doctor:")
			for _, r := range results {
				cmd.Println(r.String())
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

func runLocalChecks() []checkResult {
	results := []checkResult{
		checkOS(),
		checkArch(),
		checkDisk("/"),
	}
	return results
}

func checkOS() checkResult {
	supported := map[string]bool{
		"linux": true,
	}
	if !supported[runtime.GOOS] {
		return checkResult{Name: "OS", OK: false, Message: fmt.Sprintf("%s (k8s nodes must be Linux)", runtime.GOOS)}
	}
	return checkResult{Name: "OS", OK: true, Message: runtime.GOOS}
}

func checkArch() checkResult {
	arch := runtime.GOARCH
	if arch != "amd64" && arch != "arm64" {
		return checkResult{Name: "Arch", OK: false, Message: fmt.Sprintf("%s (supported: amd64, arm64)", arch)}
	}
	return checkResult{Name: "Arch", OK: true, Message: arch}
}

func checkDisk(path string) checkResult {
	usage, err := disk.Usage(path)
	if err != nil {
		return checkResult{Name: "Disk " + path, OK: false, Message: err.Error()}
	}
	free := usage.Free
	need := uint64(20) * 1024 * 1024 * 1024
	if free < need {
		return checkResult{Name: "Disk " + path, OK: false, Message: fmt.Sprintf("only %s free, need ≥20G", humanBytes(free))}
	}
	return checkResult{Name: "Disk " + path, OK: true, Message: fmt.Sprintf("%s free", humanBytes(free))}
}

func runRemoteChecks(path string) []checkResult {
	cfg, err := config.ParseFile(path)
	if err != nil {
		return []checkResult{{Name: "Config", OK: false, Message: err.Error()}}
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
	exec, err := cluster.NewSSHExecutor(sshCfg)
	if err != nil {
		return []checkResult{{Name: "SSH init", OK: false, Message: err.Error()}}
	}
	defer exec.Close()

	results := make([]checkResult, 0, len(hosts))
	for _, h := range hosts {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		res := exec.Run(ctx, h, "uname -a")
		cancel()
		if res.Failed() {
			results = append(results, checkResult{
				Name:    fmt.Sprintf("SSH %s", h),
				OK:      false,
				Message: res.Error(),
			})
			continue
		}
		results = append(results, checkResult{
			Name:    fmt.Sprintf("SSH %s", h),
			OK:      true,
			Message: string(trimNewline(res.Stdout)),
		})
	}
	return results
}

func trimNewline(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

func humanBytes(n uint64) string {
	const (
		KiB = 1024
		MiB = 1024 * KiB
		GiB = 1024 * MiB
		TiB = 1024 * GiB
	)
	switch {
	case n >= TiB:
		return fmt.Sprintf("%.1fT", float64(n)/TiB)
	case n >= GiB:
		return fmt.Sprintf("%.1fG", float64(n)/GiB)
	case n >= MiB:
		return fmt.Sprintf("%.1fM", float64(n)/MiB)
	case n >= KiB:
		return fmt.Sprintf("%.1fK", float64(n)/KiB)
	default:
		return fmt.Sprintf("%dB", n)
	}
}

// guard: avoid unused import warnings when logger isn't otherwise referenced
var _ = logger.Info
var _ = os.Getenv
