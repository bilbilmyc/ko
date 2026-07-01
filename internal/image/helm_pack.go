package image

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// HelmPullChart calls `helm pull <repo>/<chart> --version <v> --destination <dir>`
// and returns the local .tgz path. Used during `ko pack build` to vendor
// charts into the offline bundle.
func HelmPullChart(ctx context.Context, helmBin, repo, name, version, destDir string) (string, error) {
	if helmBin == "" {
		helmBin = "helm"
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", err
	}
	chart := repo + "/" + name
	cmd := exec.CommandContext(ctx, helmBin, "pull", chart, "--version", version, "--destination", destDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("helm pull %s: %w: %s", chart, err, out)
	}
	candidate := filepath.Join(destDir, fmt.Sprintf("%s-%s.tgz", name, version))
	if _, err := os.Stat(candidate); err != nil {
		return "", fmt.Errorf("helm pull: expected %s not found", candidate)
	}
	return candidate, nil
}

// HelmPullDefault runs HelmPullChart for the standard ko set: cilium,
// kube-vip, flannel.
func HelmPullDefault(ctx context.Context, helmBin, version, destDir string) (map[string]string, error) {
	out := map[string]string{}
	for _, c := range []struct {
		repo, name string
	}{
		{"cilium", "cilium"},
		{"kube-vip", "kube-vip"},
		{"flannel", "flannel"},
	} {
		path, err := HelmPullChart(ctx, helmBin, c.repo, c.name, version, destDir)
		if err != nil {
			return nil, err
		}
		out[c.name] = path
	}
	return out, nil
}