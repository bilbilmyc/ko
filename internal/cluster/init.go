package cluster

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/ko-build/ko/internal/ciliumconfig"
	"github.com/ko-build/ko/internal/containerd"
	"github.com/ko-build/ko/internal/docker"
	"github.com/ko-build/ko/internal/helm"
	"github.com/ko-build/ko/internal/logger"
	"github.com/ko-build/ko/pkg/config"
)

// Init orchestrates the full single-master or HA init flow.
type Init struct {
	Cfg           *config.File
	Exec          Executor
	ContainerdCtl *containerd.Installer
	DockerCtl     *docker.Installer
	Helm          *helm.Installer
	Kubeadm       *Kubeadm
	Cilium        *CiliumInstaller
	Flannel       *FlannelInstaller
	KubeVip       *KubeVipInstaller
	Offline       bool
	BundlePath    string // local OCI image tarball when Offline=true

	// imageRepositoryOverride, when set, replaces the image-repository
	// kubeadm uses. Set by the offline runner to point kubeadm at the
	// in-cluster mirror (e.g. "ko.local:5000") so it never hits the
	// public registry.k8s.io. Empty falls back to cfg.Image.Registry +
	// cfg.Image.Repository (the user-configured default).
	imageRepositoryOverride string
}

// NewInitFromConfig wires concrete installers from a parsed + defaulted config.
func NewInitFromConfig(cfg *config.File, exec Executor) (*Init, error) {
	arch := runtime.GOARCH
	cacheDir := filepath.Join(cacheHome(), "containerd")

	ctdInstaller := containerd.NewInstaller(exec, cfg.Containerd.Version, cfg.Containerd.Source, arch, cacheDir)
	dckInstaller := docker.NewInstaller(exec, "", "stable")
	kb := NewKubeadm(exec)
	return &Init{
		Cfg:           cfg,
		Exec:          exec,
		ContainerdCtl: ctdInstaller,
		DockerCtl:     dckInstaller,
		Kubeadm:       kb,
	}, nil
}

// masters returns the ordered list of master hosts (defaults to first node if
// the config didn't enumerate any).
func (i *Init) masters() []string {
	if len(i.Cfg.Nodes.Masters) == 0 {
		return nil
	}
	return i.Cfg.Nodes.Masters
}

// isHA reports whether the config requests HA (more than one master).
func (i *Init) isHA() bool {
	return len(i.masters()) > 1
}

// allHosts returns masters followed by workers (deduplicated).
func (i *Init) allHosts() []string {
	seen := map[string]bool{}
	var hosts []string
	for _, h := range i.masters() {
		if !seen[h] {
			seen[h] = true
			hosts = append(hosts, h)
		}
	}
	for _, h := range i.Cfg.Nodes.Workers {
		if !seen[h] {
			seen[h] = true
			hosts = append(hosts, h)
		}
	}
	return hosts
}

// needsFlannel returns true if any node override opts into Flannel instead of Cilium.
func (i *Init) needsFlannel() bool {
	if i.Cfg.CNI.Plugin == "flannel" {
		return true
	}
	for _, ov := range i.Cfg.NodesOverride {
		if ov.CNI == "flannel" {
			return true
		}
	}
	return false
}

// Run executes the full init flow. masterHost must be the first master in
// i.Cfg.Nodes.Masters (or the only master in non-HA setups).
func (i *Init) Run(ctx context.Context, masterHost string) error {
	// Offline mode: OfflineRunner has already installed containerd /
	// kubeadm on master-1, started the in-cluster registry, pushed every
	// bundled image, and written /etc/hosts on every node. It also sets
	// imageRepositoryOverride so kubeadm and cilium pull from the local
	// mirror instead of the public internet. After that, the rest of
	// the init flow is identical to online mode — just skip the
	// network-bound runtime install + node reachability ping.
	hosts := i.allHosts()
	if i.Offline {
		offline := &OfflineRunner{
			Cfg:     i.Cfg,
			Exec:    i.Exec,
			Bundle:  i.BundlePath,
			Master1: masterHost,
		}
		if err := offline.Run(ctx); err != nil {
			return fmt.Errorf("offline prepare: %w", err)
		}
		i.imageRepositoryOverride = offline.LocalRegistry()
	} else {
		if err := i.bootstrapHosts(ctx, hosts); err != nil {
			return err
		}
		for _, h := range hosts {
			if err := i.installRuntime(ctx, h); err != nil {
				return fmt.Errorf("install runtime on %s: %w", h, err)
			}
		}
		// kubelet drop-in: harmless online, essential offline. Even in
		// online mode, the eviction thresholds + image-pull deadline
		// are reasonable production defaults; the offline runner writes
		// the same drop-in again (idempotent — just rewrites the file).
		if err := writeKubeletDropInAll(ctx, i.Exec, hosts); err != nil {
			return err
		}
	}
	// In offline mode, OfflineRunner has already installed kubeadm to
	// /usr/local/bin on master-1. bootstrapKubeadm is a no-op (idempotent
	// apt/dnf install gated on a version match) but we still want to
	// systemd-enable kubelet, so we let it run.
	if !i.Offline {
		for _, h := range hosts {
			if err := i.bootstrapKubeadm(ctx, h); err != nil {
				return fmt.Errorf("bootstrap kubeadm on %s: %w", h, err)
			}
		}
	} else {
		// Just enable kubelet on each host; the kubeadm binary itself
		// is already in place from the bundle.
		for _, h := range hosts {
			if r := i.Exec.Run(ctx, h, "systemctl enable --now kubelet"); r.Failed() {
				return fmt.Errorf("enable kubelet on %s: %w (stderr=%s)", h, r.Err, string(r.Stderr))
			}
		}
	}
	// S14: when the user opted into an external etcd cluster, we install
	// the etcd members, generate the PKI, and distribute the client certs
	// to every master BEFORE kubeadm init runs. kubeadm then sees
	// --etcd-servers=... and skips its own local etcd.
	if IsExternalEtcd(i.Cfg) {
		if err := ProvisionExternalEtcd(ctx, i.Cfg, i.Exec, i.masters()); err != nil {
			return err
		}
	}
	if err := i.runKubeadmInit(ctx, masterHost); err != nil {
		return err
	}
	if err := i.setupKubeconfig(ctx, masterHost); err != nil {
		return err
	}
	if i.isHA() {
		if err := i.joinMasters(ctx, i.masters()[1:]); err != nil {
			return fmt.Errorf("join masters: %w", err)
		}
	}
	if len(i.Cfg.Nodes.Workers) > 0 {
		if err := i.joinWorkers(ctx, i.Cfg.Nodes.Workers); err != nil {
			return fmt.Errorf("join workers: %w", err)
		}
	}
	if err := i.installCNI(ctx); err != nil {
		return fmt.Errorf("install CNI: %w", err)
	}
	if i.Cfg.HA.VIP != "" {
		if err := i.installKubeVip(ctx); err != nil {
			return fmt.Errorf("install kube-vip: %w", err)
		}
	}
	if i.needsFlannel() {
		logger.Info("flannel fallback in use — skipping kube-proxy cleanup")
	} else if err := i.cleanupKubeProxy(ctx); err != nil {
		logger.Warn("kube-proxy cleanup failed (may be expected if already removed)", "err", err)
	}
	logger.Info("init complete", "master", masterHost, "ha", i.isHA())
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
	runtime := i.Cfg.Runtime.Default
	if ov := i.Cfg.LookupNodeOverride(host); ov != nil && ov.Runtime != "" {
		runtime = ov.Runtime
	}
	switch runtime {
	case "containerd":
		cfg := containerd.DefaultConfig(
			containerd.MirrorsFromDockerEndpointURLs(i.Cfg.Image.RegistryMirrors),
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
		return fmt.Errorf("unsupported runtime %q", runtime)
	}
}

func (i *Init) bootstrapKubeadm(ctx context.Context, host string) error {
	return i.Kubeadm.BootstrapKubeadm(ctx, host, i.Cfg.Cluster.Version)
}

func (i *Init) runKubeadmInit(ctx context.Context, host string) error {
	imageRepo := i.imageRepositoryOverride
	if imageRepo == "" {
		imageRepo = i.Cfg.Image.Registry + "/" + i.Cfg.Image.Repository
	}
	opts := KubeadmOptions{
		KubernetesVersion:   i.Cfg.Cluster.Version,
		PodCIDR:             i.Cfg.Cluster.CIDR,
		ServiceCIDR:         i.Cfg.Cluster.SVCCIDR,
		ImageRepository:     imageRepo,
		CertificateValidity: i.Cfg.Certificates.Validity,
	}
	if i.Cfg.HA.VIP != "" {
		opts.APIServerEndpoint = i.Cfg.HA.VIP + ":6443"
	}
	// S14: when external etcd mode is in use, feed kubeadm the endpoints
	// + the on-master PKI directory that ProvisionExternalEtcd populated.
	if IsExternalEtcd(i.Cfg) {
		opts.EtcdServers = strings.Join(i.Cfg.Etcd.Endpoints, ",")
		opts.EtcdPKIDir = i.Cfg.Etcd.PKIDir
	}
	_, err := i.Kubeadm.Init(ctx, host, opts)
	return err
}

func (i *Init) setupKubeconfig(ctx context.Context, host string) error {
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

// joinMasters fetches a fresh token + cert key from the first master and joins
// each of the supplied masters as control-plane nodes.
func (i *Init) joinMasters(ctx context.Context, masters []string) error {
	token, err := i.Kubeadm.JoinToken(ctx, i.masters()[0])
	if err != nil {
		return fmt.Errorf("token: %w", err)
	}
	hash, err := i.kubeadmDiscoveryHash(ctx, i.masters()[0])
	if err != nil {
		return fmt.Errorf("discovery hash: %w", err)
	}
	certKey, err := i.Kubeadm.CertKey(ctx, i.masters()[0])
	if err != nil {
		return fmt.Errorf("cert key: %w", err)
	}
	endpoint := i.Cfg.HA.VIP
	if endpoint == "" {
		endpoint = i.masters()[0]
	}
	for _, m := range masters {
		opts := KubeadmOptions{
			Token:               token,
			DiscoveryTokenCAHash: hash,
			APIServerEndpoint:   endpoint + ":6443",
			CertKey:             certKey,
			ImageRepository:     i.imageRepositoryOverride,
		}
		if _, err := i.Kubeadm.JoinControlPlane(ctx, m, opts); err != nil {
			return fmt.Errorf("master %s join: %w", m, err)
		}
	}
	return nil
}

// joinWorkers joins each worker node as a plain worker (no --control-plane).
func (i *Init) joinWorkers(ctx context.Context, workers []string) error {
	token, err := i.Kubeadm.JoinToken(ctx, i.masters()[0])
	if err != nil {
		return fmt.Errorf("token: %w", err)
	}
	hash, err := i.kubeadmDiscoveryHash(ctx, i.masters()[0])
	if err != nil {
		return fmt.Errorf("discovery hash: %w", err)
	}
	endpoint := i.Cfg.HA.VIP
	if endpoint == "" {
		endpoint = i.masters()[0]
	}
	for _, w := range workers {
		opts := KubeadmOptions{
			Token:               token,
			DiscoveryTokenCAHash: hash,
			ImageRepository:     i.imageRepositoryOverride,
		}
		if endpoint != "" {
			opts.APIServerEndpoint = endpoint + ":6443"
		}
		if _, err := i.Kubeadm.Join(ctx, w, opts); err != nil {
			return fmt.Errorf("worker %s join: %w", w, err)
		}
	}
	return nil
}

// kubeadmDiscoveryHash asks the existing master for the CA cert hash so that
// workers / additional masters can verify the cluster's CA on join.
func (i *Init) kubeadmDiscoveryHash(ctx context.Context, host string) (string, error) {
	res := i.Exec.Run(ctx, host, "openssl x509 -pubkey -in /etc/kubernetes/pki/ca.crt | openssl rsa -pubin -outform der 2>/dev/null | openssl dgst -sha256 -hex | awk '{print \"sha256:\"$NF}'")
	if res.Failed() {
		return "", fmt.Errorf("compute discovery hash: %w", res.Err)
	}
	return string(trimNewlineBytes(res.Stdout)), nil
}

func (i *Init) installCNI(ctx context.Context) error {
	if i.needsFlannel() {
		if i.Flannel == nil {
			i.Flannel = &FlannelInstaller{
				Helm:    i.Helm,
				Chart:   "flannel/flannel",
				Backend: i.Cfg.CNI.Flannel.Backend,
			}
		}
		return i.Flannel.Install(ctx, i.Cfg.Cluster.CIDR)
	}
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
	// cleanupKubeProxy is wired in S4 / Cilium_kpfree. For S3, the actual API
	// call to remove kube-proxy lives in cleanup_kpfree.go and will be invoked
	// from the dashboard / ko reset path. Leaving the no-op stub here keeps
	// the orchestrator stable across slices.
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