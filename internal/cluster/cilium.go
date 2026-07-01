package cluster

import (
	"context"
	"fmt"

	"github.com/ko-build/ko/internal/helm"
	"github.com/ko-build/ko/internal/logger"
)

// CiliumInstaller installs Cilium as a CNI with optional kube-proxy replacement.
type CiliumInstaller struct {
	Helm     *helm.Installer
	Chart    string // "cilium/cilium" for online, or path to tgz
	Version  string // empty = latest
	Replacemode string // "strict" | "disabled" | "probe"
}

func (c *CiliumInstaller) Values(clusterCIDR, serviceCIDR string) map[string]any {
	mode := c.Replacemode
	if mode == "" {
		mode = "strict"
	}
	v := map[string]any{
		"kubeProxyReplacement":    mode,
		"k8sServiceHost":          "127.0.0.1",
		"k8sServicePort":          6443,
		"ipv4NativeRoutingCIDR":   clusterCIDR,
		"cluster.id":              0,
		"cluster.name":            "ko",
		"hubble.enabled":           true,
		"hubble.relay.enabled":     true,
		"operator.replicas":       1,
		"image.pullPolicy":        "IfNotPresent",
		"ipam.mode":               "kubernetes",
	}
	if serviceCIDR != "" {
		v["ipv4NativeRoutingCIDR"] = clusterCIDR
	}
	return v
}

// Install applies the cilium Helm chart with kube-proxy-replacement config.
func (c *CiliumInstaller) Install(ctx context.Context, clusterCIDR, serviceCIDR string) error {
	logger.Info("installing cilium", "chart", c.Chart, "version", c.Version, "replacement", c.Replacemode)
	rel, err := c.Helm.Install(ctx, helm.InstallOptions{
		ReleaseName: "cilium",
		Namespace:   "kube-system",
		Chart:       c.Chart,
		Version:     c.Version,
		Values:      c.Values(clusterCIDR, serviceCIDR),
		Wait:        true,
		Timeout:     "10m",
	})
	if err != nil {
		return fmt.Errorf("cilium install: %w", err)
	}
	logger.Info("cilium installed", "release", rel.Name, "version", rel.Chart.Metadata.Version)
	return nil
}
