package cluster

import (
	"context"
	"fmt"

	"github.com/ko-build/ko/internal/helm"
	"github.com/ko-build/ko/internal/logger"
)

// FlannelInstaller installs Flannel as a fallback CNI for nodes that can't run
// Cilium (no eBPF support, restricted kernel, etc.). The ko CNI selection rule:
//   - default CNI is set by cni.plugin ("cilium" or "flannel")
//   - per-node override via nodes_override "host" { cni = "flannel" } installs
//     flannel cluster-wide if ANY node needs it; Cilium kube-proxy-replacement
//     is then disabled for that node set.
type FlannelInstaller struct {
	Helm    *helm.Installer
	Chart   string // "flannel/flannel" for online, or path to tgz
	Version string // empty = latest
	Backend string // "vxlan" | "host-gw"
}

func (f *FlannelInstaller) Values(clusterCIDR string) map[string]any {
	backend := f.Backend
	if backend == "" {
		backend = "vxlan"
	}
	return map[string]any{
		"podCidr":         clusterCIDR,
		"backend":         backend,
		"image.pullPolicy": "IfNotPresent",
		"flannel.args": []string{
			"--iface=eth0",
		},
	}
}

// Install applies the flannel chart with default backend.
func (f *FlannelInstaller) Install(ctx context.Context, clusterCIDR string) error {
	logger.Info("installing flannel", "chart", f.Chart, "version", f.Version, "backend", f.Backend)
	rel, err := f.Helm.Install(ctx, helm.InstallOptions{
		ReleaseName: "flannel",
		Namespace:   "kube-flannel",
		Chart:       f.Chart,
		Version:     f.Version,
		Values:      f.Values(clusterCIDR),
		Wait:        true,
		Timeout:     "10m",
		CreateNS:    true,
	})
	if err != nil {
		return fmt.Errorf("flannel install: %w", err)
	}
	logger.Info("flannel installed", "release", rel.Name, "version", rel.Chart.Metadata.Version)
	return nil
}