package cluster

import (
	"context"
	"fmt"
	"strings"

	"github.com/ko-build/ko/internal/helm"
	"github.com/ko-build/ko/internal/logger"
)

type KubeVipInstaller struct {
	Helm    *helm.Installer
	Chart   string
	Version string
	Image   string
	VIP     string
}

func (k *KubeVipInstaller) Values(iface, controlPlaneVIP string) map[string]any {
	image := k.Image
	if image == "" {
		image = "ghcr.io/kube-vip/kube-vip:latest"
	}
	v := map[string]any{
		"image":           image,
		"interface":       iface,
		"vip_interface":   iface,
		"controlPlane":    true,
		"servicesEnabled": true,
		"svclb":           true,
	}
	if controlPlaneVIP != "" {
		v["address"] = controlPlaneVIP
	}
	return v
}

// Install applies the kube-vip chart, which deploys kube-vip as a DaemonSet
// for both control-plane VIP and ServiceLB.
func (k *KubeVipInstaller) Install(ctx context.Context, iface string) error {
	logger.Info("installing kube-vip", "vip", k.VIP, "image", k.Image)
	rel, err := k.Helm.Install(ctx, helm.InstallOptions{
		ReleaseName: "kube-vip",
		Namespace:   "kube-system",
		Chart:       k.Chart,
		Version:     k.Version,
		Values:      k.Values(iface, k.VIP),
		Wait:        true,
		Timeout:     "5m",
	})
	if err != nil {
		// kube-vip may already be present from a prior init; that's OK
		if !strings.Contains(err.Error(), "already exists") {
			return fmt.Errorf("kube-vip install: %w", err)
		}
	}
	logger.Info("kube-vip installed", "release", rel.Name, "version", rel.Chart.Metadata.Version)
	return nil
}
