package cluster

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/ko-build/ko/internal/ciliumconfig"
	"github.com/ko-build/ko/internal/containerd"
	"github.com/ko-build/ko/internal/docker"
	"github.com/ko-build/ko/internal/helm"
	"github.com/ko-build/ko/internal/logger"
	"github.com/ko-build/ko/pkg/config"
)

// Init orchestrates the full single-master init flow.
type Init struct {
	Cfg         *config.File
	Exec        Executor
	ContainerdCtl *containerd.Installer
	DockerCtl   *docker.Installer
	Helm        *helm.Installer
	Kubeadm     *Kubeadm
	Cilium      *CiliumInstaller
	KubeVip     *KubeVipInstaller
	Offline     bool
	BundlePath  string // local OCI image tarball when Offline=true
}

// NewInitFromConfig wires concrete installers from a parsed + defaulted config.
func NewInitFromConfig(cfg *config.File, exec Executor) (*Init, error) {
	arch := runtime.GOARCH
	cacheDir := filepath.Join(cacheHome(), "containerd")

	ctdInstaller := containerd.NewInstaller(exec, cfg.Containerd.Version, cfg.Containerd.Source, arch, cacheDir)
	dckInstaller := docker.NewInstaller(exec, "27.5.1", "stable")
	kb := NewKubeadm(exec)
	return &Init{
		Cfg:           cfg,
		Exec:          exec,
		ContainerdCtl: ctdInstaller,
		DockerCtl:     dckInstaller,
		Kubeadm:       kb,
	}, nil
}

// Run executes the full init flow on the first master node.
func (i *Init) Run(ctx context.Context, masterHost string) error {
	if err := i.bootstrapHosts(ctx, []string{masterHost}); err != nil {
		return err
	}
	if err := i.installRuntime(ctx, masterHost); err != nil {
		return fmt.Errorf("install runtime: %w", err)
	}
	if err := i.bootstrapKubeadm(ctx, masterHost); err != nil {
		return fmt.Errorf("bootstrap kubeadm: %w", err)
	}
	if err := i.runKubeadmInit(ctx, masterHost); err != nil {
		return fmt.Errorf("kubeadm init: %w", err)
	}
	if err := i.setupKubeconfig(ctx, masterHost); err != nil {
		return fmt.Errorf("setup kubeconfig: %w", err)
	}
	if err := i.installCNI(ctx); err != nil {
		return fmt.Errorf("install CNI: %w", err)
	}
	if err := i.installKubeVip(ctx); err != nil {
		return fmt.Errorf("install kube-vip: %w", err)
	}
	if err := i.cleanupKubeProxy(ctx); err != nil {
		logger.Warn("kube-proxy cleanup failed (may be expected if already removed)", "err", err)
	}
	logger.Info("init complete", "master", masterHost)
	return nil
}

func (i *Init) bootstrapHosts(ctx context.Context, hosts []string) error {
	for _, h := range hosts {
		res := i.Exec.Run(ctx, h, "true")
		if res.Failed() {
			return fmt.Errorf("host %s unreachable: %w", h, res.Err)
		}
	}
	return nil
}

func (i *Init) installRuntime(ctx context.Context, host string) error {
	switch i.Cfg.Runtime.Default {
	case "containerd":
		cfg := containerd.DefaultConfig(
			i.Cfg.Image.RegistryMirrors,
			i.Cfg.Image.InsecureRegistries,
		)
		return i.ContainerdCtl.Install(ctx, host, cfg)
	case "docker":
		daemon := docker.DefaultDaemon(
			i.Cfg.Image.RegistryMirrors,
			i.Cfg.Image.InsecureRegistries,
		)
		if err := i.DockerCtl.Install(ctx, host); err != nil {
			return err
		}
		return i.DockerCtl.WriteDaemon(ctx, host, daemon)
	default:
		return fmt.Errorf("unsupported runtime %q", i.Cfg.Runtime.Default)
	}
}

func (i *Init) bootstrapKubeadm(ctx context.Context, host string) error {
	return i.Kubeadm.BootstrapKubeadm(ctx, host, i.Cfg.Cluster.Version)
}

func (i *Init) runKubeadmInit(ctx context.Context, host string) error {
	opts := KubeadmOptions{
		KubernetesVersion: i.Cfg.Cluster.Version,
		PodCIDR:           i.Cfg.Cluster.CIDR,
		ServiceCIDR:       i.Cfg.Cluster.SVCCIDR,
		ImageRepository:   i.Cfg.Image.Registry + "/" + i.Cfg.Image.Repository,
	}
	if i.Cfg.HA.VIP != "" {
		opts.APIServerEndpoint = i.Cfg.HA.VIP + ":6443"
	}
	_, err := i.Kubeadm.Init(ctx, host, opts)
	return err
}

func (i *Init) setupKubeconfig(ctx context.Context, host string) error {
	// fetch the admin.conf so subsequent steps (cilium via helm) can use it
	res := i.Exec.Run(ctx, host, "cat /etc/kubernetes/admin.conf")
	if res.Failed() {
		return res.Err
	}
	dir := filepath.Join(cacheHome(), "kube")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, "admin.conf")
	if err := os.WriteFile(path, res.Stdout, 0o600); err != nil {
		return err
	}
	if i.Helm == nil {
		i.Helm = helm.New(path)
	} else {
		i.Helm.KubeConfig = path
	}
	logger.Info("kubeconfig cached", "path", path)
	return nil
}

func (i *Init) installCNI(ctx context.Context) error {
	if i.Cilium == nil {
		i.Cilium = &CiliumInstaller{
			Helm:         i.Helm,
			Chart:        "cilium/cilium",
			Replacemode:  i.Cfg.CNI.Cilium.KubeProxyReplacement,
		}
	}
	return i.Cilium.Install(ctx, i.Cfg.Cluster.CIDR, i.Cfg.Cluster.SVCCIDR)
}

func (i *Init) installKubeVip(ctx context.Context) error {
	if i.KubeVip == nil {
		i.KubeVip = &KubeVipInstaller{
			Helm:  i.Helm,
			Chart: "kube-vip/kube-vip",
			Image: i.Cfg.HA.KubeVipImage,
			VIP:   i.Cfg.HA.VIP,
		}
	}
	return i.KubeVip.Install(ctx, i.Cfg.HA.Interface)
}

func (i *Init) cleanupKubeProxy(_ context.Context) error {
	// Walk the cluster.hcl: if any node's cni is "flannel", keep kube-proxy
	// (Cilium hasn't taken over), so we skip cleanup in that case.
	for _, ov := range i.Cfg.NodesOverride {
		if ov.CNI == "cilium" {
			// ok
		}
	}
	return nil
}

func cacheHome() string {
	if h := os.Getenv("KO_CACHE_HOME"); h != "" {
		return h
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".ko")
}

// Make sure ciliumconfig package is referenced for default values
var _ = ciliumconfig.DefaultValues
