package version

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestVendorVersionsSync asserts that the asset versions in
// internal/version/versions.go match the values declared in
// vendor-versions.env at the repo root.
//
// This is the contract that keeps `ko vendor fetch` (which reads from
// vendor-versions.env) and `ko pack build` (which reads from
// internal/version) in lockstep. A drift between the two means either
// the bundle embeds different binaries than the fetch script downloads
// (waste) or the fetch script downloads versions ko then refuses to
// bake (silent failure).
//
// Test walks up from the test working dir to the repo root (where
// vendor-versions.env lives) — works under `go test ./...` from the
// repo root.
func TestVendorVersionsSync(t *testing.T) {
	repoRoot, err := findRepoRoot()
	require.NoError(t, err, "couldn't locate repo root for vendor-versions.env")

	envPath := filepath.Join(repoRoot, "vendor-versions.env")
	envVals, err := parseEnvFile(envPath)
	require.NoError(t, err, "parse vendor-versions.env")

	// Map env-var name -> versions.go constant. Every entry MUST exist on
	// both sides — a missing entry is a silent regression (either the
	// fetch script downloads a version ko won't bake, or vice versa).
	cases := []struct {
		envVar     string
		goConstVal string
		label      string
	}{
		{"CONTAINERD_VERSION", ContainerdVersion, "containerd"},
		{"DOCKER_VERSION", DockerVersion, "docker"},
		{"KUBE_VERSION", KubeVersion, "kubeVersion"},
		{"KUBEAADM_VERSION", KubeadmVersion, "kubeadmVersion"},
		{"KUBELET_VERSION", KubeletVersion, "kubeletVersion"},
		{"CRI_DOCKERD_VERSION", CRIDockerdVersion, "cri-dockerd"},
		{"REGISTRY_VERSION", RegistryVersion, "registry"},
		{"CILIUM_VERSION", CiliumVersion, "cilium"},
		{"PROMETHEUS_STACK_VERSION", PrometheusStackVersion, "prometheus stack"},
	}

	for _, c := range cases {
		envVal, envOK := envVals[c.envVar]
		if assert.True(t, envOK,
			"vendor-versions.env missing %s (bump it there when bumping %s in internal/version)",
			c.envVar, c.label) {
			assert.Equal(t, envVal, c.goConstVal,
				"%s drift: vendor-versions.env has %q, internal/version has %q",
				c.label, envVal, c.goConstVal)
		}
	}
}

// findRepoRoot walks up from the current test directory until it finds
// the go.mod marker. Used so the test passes regardless of where the
// operator runs `go test ./...` from.
func findRepoRoot() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	dir := cwd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}

// parseEnvFile reads a KEY=VALUE file (ignoring comments and blanks)
// and returns a map. Used to ingest vendor-versions.env without pulling
// in a full shell-evaluator dep — the file's grammar is intentionally
// flat.
func parseEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		// strip surrounding quotes (the env file doesn't use them, but
		// be tolerant for hand-edits)
		v = strings.Trim(v, `"'`)
		out[k] = v
	}
	return out, sc.Err()
}