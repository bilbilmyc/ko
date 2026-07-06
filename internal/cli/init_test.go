package cli

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInit_RejectsOfflineWithoutBundle pins the v0.0.5 contract: `--offline`
// without `--bundle` is a silent footgun (OfflineRunner.Run would fail
// mid-init with a confusing "bundle is required" message). Init must
// reject the combination up front with a clear, actionable error.
//
// Both forms of the offline flag are checked: the local `--offline` and
// the global `--offline`. Either alone, paired with no --bundle, fails.
func TestInit_RejectsOfflineWithoutBundle(t *testing.T) {
	cases := []struct {
		name           string
		offlineFlag    bool
		globalOffline  bool
		bundle         string
		expectErr      bool
		errContains    string
	}{
		{
			name:        "local --offline without --bundle",
			offlineFlag: true,
			bundle:      "",
			expectErr:   true,
			errContains: "--offline requires --bundle",
		},
		{
			name:          "global --offline without --bundle",
			globalOffline: true,
			bundle:        "",
			expectErr:     true,
			errContains:   "--offline requires --bundle",
		},
		{
			name:        "both offline flags without --bundle",
			offlineFlag: true,
			globalOffline: true,
			bundle:      "",
			expectErr:   true,
			errContains: "--offline requires --bundle",
		},
		{
			name:        "local --offline with --bundle",
			offlineFlag: true,
			bundle:      "/tmp/bundle.oci.tar.gz",
			expectErr:   false,
		},
		{
			name:          "global --offline with --bundle",
			globalOffline: true,
			bundle:        "/tmp/bundle.oci.tar.gz",
			expectErr:     false,
		},
		{
			name:      "neither flag, no bundle (online mode)",
			bundle:    "",
			expectErr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := NewRootCmd()
			// Set the persistent (global) flag if requested.
			if tc.globalOffline {
				require.NoError(t, cmd.PersistentFlags().Set("offline", "true"))
			}
			args := []string{"init"}
			if tc.offlineFlag {
				args = append(args, "--offline")
			}
			if tc.bundle != "" {
				args = append(args, "--bundle", tc.bundle)
			}
			cmd.SetArgs(args)
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			err := cmd.Execute()
			if tc.expectErr {
				require.Error(t, err, "expected --offline without --bundle to fail")
				assert.True(t,
					strings.Contains(err.Error(), tc.errContains),
					"error must mention the missing flag: got %q", err.Error())
			} else if err != nil {
				// For the "ok" cases, we don't try to drive init to success
				// (no SSH executor, no real cluster). We just assert the
				// guard didn't trip — i.e., the error is NOT about
				// --bundle being missing.
				assert.NotContains(t, err.Error(), "--offline requires --bundle",
					"guard should not fire when bundle is set")
			}
		})
	}
}