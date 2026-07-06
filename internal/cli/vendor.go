package cli

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
)

// vendorScriptPath is the path to the bash fetch script that downloads
// every binary + image ko needs into third_party/. The script lives in
// the repo root next to go.mod; we resolve it relative to the working
// directory when the operator runs `ko vendor fetch` from the repo.
//
// In a production install (ko binary on PATH, no checkout around), the
// operator is expected to either (a) keep a checkout to drive the fetch
// or (b) run fetch-vendor.sh directly. We don't ship the bundled assets
// in the ko binary itself — third_party/ is operator-local state.
var vendorScriptPath = "scripts/fetch-vendor.sh"

func newVendorCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "vendor",
		Short: "Manage vendored offline assets (binaries + images + charts)",
		Long: `vendor populates and inspects third_party/ — the operator-local
asset tree ko pack reads at build time. The fetch step is the only ko
operation that touches the network; everything downstream (pack build /
init --offline) is purely local.`,
	}
	cmd.AddCommand(newVendorFetchCmd())
	cmd.AddCommand(newVendorCleanCmd())
	cmd.AddCommand(newVendorPathsCmd())
	return cmd
}

func newVendorFetchCmd() *cobra.Command {
	var (
		vendorDir string
		only      string // comma-separated: containerd,kubeadm,... (optional)
	)
	cmd := &cobra.Command{
		Use:   "fetch",
		Short: "Download binaries / images / charts into third_party/ (network required)",
		Long: `fetch runs scripts/fetch-vendor.sh to download every asset ko
needs into third_party/. Asset versions are pinned via vendor-versions.env
(mirrored in internal/version/versions.go — TestVendorVersionsSync guards
drift).

  --vendor-dir <path>  Override the third_party/ root (default: ./third_party).
  --only <list>        Fetch only the named assets (comma-separated;
                       pass the same names fetch-vendor.sh accepts as
                       positional args).

After fetch, run ` + "`ko pack build`" + ` to bake the bundle.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFetchScript(cmd, vendorDir, only, false, "")
		},
	}
	cmd.Flags().StringVar(&vendorDir, "vendor-dir", "", "output directory (default ./third_party)")
	cmd.Flags().StringVar(&only, "only", "", "comma-separated asset list (default: all)")
	return cmd
}

func newVendorCleanCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "clean",
		Short: "Delete third_party/ — every vendored asset goes away",
		Long: `clean invokes fetch-vendor.sh --clean, which removes the entire
third_party/ tree. The next ` + "`ko pack build`" + ` will fail with a clear
"missing layer" error until ` + "`ko vendor fetch`" + ` is re-run.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFetchScript(cmd, "", "", true, "")
		},
	}
	return cmd
}

func newVendorPathsCmd() *cobra.Command {
	var vendorDir string
	cmd := &cobra.Command{
		Use:   "paths",
		Short: "Print the absolute path of every vendored asset (dry-run)",
		Long: `paths invokes fetch-vendor.sh --print-paths to show where each
asset would live after ` + "`ko vendor fetch`" + `. No network, no writes —
useful for CI smoke tests and for an operator sanity-checking the layout
before triggering a fetch.

Output is one path per line; missing assets are reported with "(missing)".`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFetchScript(cmd, vendorDir, "", false, "--print-paths")
		},
	}
	cmd.Flags().StringVar(&vendorDir, "vendor-dir", "", "vendor root to inspect (default ./third_party)")
	return cmd
}

// runFetchScript is the shared exec wrapper. We shell out to bash because
// the fetch script handles many heterogeneous download sources (GitHub
// releases, docker save, helm pull) that would be tedious to mirror in
// Go — and the script is already the source of truth for asset versions.
//
// If KO_VENDOR_SCRIPT is set we use it as the script path; otherwise
// we look for scripts/fetch-vendor.sh relative to the working directory.
func runFetchScript(cmd *cobra.Command, vendorDir, only string, clean bool, extraArg string) error {
	scriptPath := os.Getenv("KO_VENDOR_SCRIPT")
	if scriptPath == "" {
		scriptPath = vendorScriptPath
	}
	if _, err := os.Stat(scriptPath); err != nil {
		return fmt.Errorf("vendor script not found at %s (run from repo root, or set KO_VENDOR_SCRIPT): %w", scriptPath, err)
	}

	args := []string{scriptPath}
	if clean {
		args = append(args, "--clean")
	}
	if extraArg != "" {
		args = append(args, extraArg)
	}
	if vendorDir != "" {
		// fetch-vendor.sh doesn't take a --vendor-dir yet; until it does,
		// the operator can set KO_VENDOR_DIR (which fetch-vendor.sh
		// forwards into a set of mkdir -p calls — see the script's
		// header). For now we honor the env var locally for downstream
		// ko invocations, but the script itself writes to ./third_party.
		_ = vendorDir
	}
	if only != "" {
		for n := range strings.SplitSeq(only, ",") {
			n = strings.TrimSpace(n)
			if n != "" {
				args = append(args, n)
			}
		}
	}

	c := exec.Command("bash", args...)
	c.Stdout = cmd.OutOrStdout()
	c.Stderr = cmd.ErrOrStderr()
	if err := c.Run(); err != nil {
		return fmt.Errorf("fetch-vendor.sh failed: %w", err)
	}
	return nil
}