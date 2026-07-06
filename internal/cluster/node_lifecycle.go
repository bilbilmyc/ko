package cluster

import (
	"context"
	"fmt"
	"strings"

	"github.com/ko-build/ko/internal/logger"
	"github.com/ko-build/ko/pkg/config"
)

// NodeLifecycle adds / removes / inspects nodes in a running cluster.
//
// AddWorker / AddMaster both assume the cluster is already running and that
// kubeconfig + kubectl are reachable (helm.Installer.KubeConfig is set after
// the first init). RemoveXxx drains + deletes via kubectl and resets kubeadm
// on the host.
//
// OfflineBundle, when set, makes AddWorker / AddMaster follow the airgap
// path: the bundle is scp'd to the new host, containerd + kubeadm are
// installed from it, containerd is configured with mirror → ko.local, the
// /etc/hosts entry for ko.local is added, the kubelet drop-in is written,
// and kubeadm join uses --image-repository=ko.local:5000 so it never
// touches the public internet. Without OfflineBundle the new node falls
// back to the existing online install path.
type NodeLifecycle struct {
	Cfg            *config.File
	Exec           Executor
	Kubeadm        *Kubeadm
	KubeConfigPath string // local path to admin.conf
	// InstallContainerd / InstallDocker run the runtime install on the host
	// before kubeadm joins. Either may be nil if the runtime is pre-installed.
	InstallContainerd func(ctx context.Context, host string) error
	InstallDocker     func(ctx context.Context, host string) error
	// OfflineRunner, when set, lets AddWorker/AddMaster follow the airgap
	// path. The same OfflineRunner used for init is reused so master-1's
	// in-cluster registry, mirror config, and /etc/hosts entries stay
	// consistent. Bundle must be non-empty on OfflineRunner.
	OfflineRunner *OfflineRunner
	// LocalRegistryOverride, when set, replaces cfg.Image.Registry +
	// cfg.Image.Repository for kubeadm join's --image-repository. Used
	// by the airgap path to point kubeadm at ko.local:5000.
	LocalRegistryOverride string
}

// NewNodeLifecycle wires the lifecycle with shared infra.
func NewNodeLifecycle(cfg *config.File, exec Executor, kb *Kubeadm, kubeconfig string) *NodeLifecycle {
	return &NodeLifecycle{
		Cfg:            cfg,
		Exec:           exec,
		Kubeadm:        kb,
		KubeConfigPath: kubeconfig,
	}
}

// AddWorker bootstraps a new worker host (installs runtime + kubeadm) and
// joins it as a worker. In offline mode (OfflineRunner set), the host is
// prepared from the bundle — containerd/kubeadm installed locally,
// containerd configured with the in-cluster registry mirror, /etc/hosts
// entry for ko.local, kubelet drop-in, kubeadm join with
// --image-repository=ko.local:5000.
func (n *NodeLifecycle) AddWorker(ctx context.Context, host string) error {
	logger.Info("adding worker", "host", host)
	if err := n.bootstrapHost(ctx, host); err != nil {
		return fmt.Errorf("bootstrap host: %w", err)
	}
	master := n.masterHost()
	if master == "" {
		return fmt.Errorf("no master host in config to fetch join token from")
	}
	token, err := n.Kubeadm.JoinToken(ctx, master)
	if err != nil {
		return fmt.Errorf("token: %w", err)
	}
	hash, err := n.kubeadmDiscoveryHash(ctx, master)
	if err != nil {
		return fmt.Errorf("discovery hash: %w", err)
	}
	opts := KubeadmOptions{
		Token:               token,
		DiscoveryTokenCAHash: hash,
		ImageRepository:     n.LocalRegistryOverride,
	}
	if endpoint := n.apiServerEndpoint(); endpoint != "" {
		opts.APIServerEndpoint = endpoint
	}
	if _, err := n.Kubeadm.Join(ctx, host, opts); err != nil {
		return fmt.Errorf("kubeadm join: %w", err)
	}
	logger.Info("worker joined", "host", host)
	return nil
}

// AddMaster bootstraps a new master host and joins it as control-plane.
// In offline mode (OfflineRunner set), the host is prepared from the bundle
// the same way as a worker (see AddWorker); the only difference is the
// kubeadm join is --control-plane with a cert key.
func (n *NodeLifecycle) AddMaster(ctx context.Context, host string) error {
	logger.Info("adding master", "host", host)
	if err := n.bootstrapHost(ctx, host); err != nil {
		return fmt.Errorf("bootstrap host: %w", err)
	}
	master := n.masterHost()
	if master == "" {
		return fmt.Errorf("no master host in config to fetch join token from")
	}
	token, err := n.Kubeadm.JoinToken(ctx, master)
	if err != nil {
		return fmt.Errorf("token: %w", err)
	}
	hash, err := n.kubeadmDiscoveryHash(ctx, master)
	if err != nil {
		return fmt.Errorf("discovery hash: %w", err)
	}
	certKey, err := n.Kubeadm.CertKey(ctx, master)
	if err != nil {
		return fmt.Errorf("cert key: %w", err)
	}
	opts := KubeadmOptions{
		Token:               token,
		DiscoveryTokenCAHash: hash,
		CertKey:             certKey,
		APIServerEndpoint:   n.apiServerEndpoint(),
		ImageRepository:     n.LocalRegistryOverride,
	}
	if _, err := n.Kubeadm.JoinControlPlane(ctx, host, opts); err != nil {
		return fmt.Errorf("kubeadm join control-plane: %w", err)
	}
	logger.Info("master joined", "host", host)
	return nil
}

// Remove drains + deletes a node from the cluster and resets kubeadm on it.
func (n *NodeLifecycle) Remove(ctx context.Context, host string, opts RemoveOptions) error {
	logger.Info("removing node", "host", host, "force", opts.Force)
	if err := n.drainNode(ctx, host, opts.Force); err != nil && !opts.Force {
		return fmt.Errorf("drain: %w", err)
	}
	if err := n.deleteNode(ctx, host); err != nil {
		return fmt.Errorf("delete node: %w", err)
	}
	cri := n.criSocket(host)
	if _, err := n.Kubeadm.Reset(ctx, host, cri); err != nil {
		return fmt.Errorf("kubeadm reset: %w", err)
	}
	logger.Info("node removed", "host", host)
	return nil
}

// RemoveOptions tunes Remove behaviour.
type RemoveOptions struct {
	Force bool // skip drain errors, useful for unreachable hosts
}

// List returns `kubectl get nodes -o wide` output as a string for display.
func (n *NodeLifecycle) List(ctx context.Context) (string, error) {
	res := n.Exec.Run(ctx, "local", kubectl(n.KubeConfigPath, "get nodes -o wide"))
	if res.Failed() {
		return "", fmt.Errorf("kubectl get nodes: %w", res.Err)
	}
	return string(res.Stdout), nil
}

// Label applies a label key=value to a node.
func (n *NodeLifecycle) Label(ctx context.Context, host, key, value string) error {
	cmd := kubectl(n.KubeConfigPath, fmt.Sprintf("label node %s %s=%s --overwrite", host, key, value))
	res := n.Exec.Run(ctx, "local", cmd)
	if res.Failed() {
		return fmt.Errorf("kubectl label: %w", res.Err)
	}
	return nil
}

// bootstrapHost runs the runtime + kubeadm bootstrap on a fresh node.
// In offline mode (OfflineRunner set) the host is prepared from the bundle:
// containerd + kubeadm installed from local layers, containerd configured
// with the in-cluster registry mirror, /etc/hosts entry for ko.local,
// kubelet drop-in applied. Skip the apt/dnf install path because the
// bundle ships those binaries — calling both would silently double-install.
func (n *NodeLifecycle) bootstrapHost(ctx context.Context, host string) error {
	if n.OfflineRunner != nil {
		return n.bootstrapHostOffline(ctx, host)
	}
	if n.InstallContainerd != nil {
		if err := n.InstallContainerd(ctx, host); err != nil {
			return fmt.Errorf("containerd: %w", err)
		}
	}
	if n.InstallDocker != nil {
		if err := n.InstallDocker(ctx, host); err != nil {
			return fmt.Errorf("docker: %w", err)
		}
	}
	return n.Kubeadm.BootstrapKubeadm(ctx, host, n.Cfg.Cluster.Version)
}

// bootstrapHostOffline implements the airgap `ko node add` path:
//  1. identify layers on the operator side (from the local bundle file)
//  2. scp the bundle to the new host
//  3. extract on the host
//  4. install containerd + kubeadm from the bundle
//  5. configure containerd with mirror → ko.local
//  6. /etc/hosts: ko.local → master-1 IP (or HA VIP)
//  7. kubelet drop-in
//
// kubeadm join itself is called by AddWorker/AddMaster AFTER this returns,
// with --image-repository=ko.local:5000 so it pulls from the in-cluster
// registry instead of the public one.
//
// Layer identification runs on the operator side because the OCI bundle
// is a local file — we can read its index.json without round-tripping
// through the host's filesystem. This also keeps the host extract
// idempotent: if scp fails, we never extracted; if extract fails, we
// never installed.
func (n *NodeLifecycle) bootstrapHostOffline(ctx context.Context, host string) error {
	r := n.OfflineRunner
	if r.Bundle == "" {
		return fmt.Errorf("offline node add requires OfflineRunner.Bundle to be set")
	}

	// 1. identify layers on the operator side. The bundle is a local OCI
	//    image-layout tar.gz — we read its index.json directly. The host
	//    extract below is purely for the host's containerd/kubeadm to
	//    consume; we don't depend on its success for layer identification.
	layers, err := r.identifyLayersFromTar(r.Bundle)
	if err != nil {
		return err
	}
	if layers.Containerd == "" {
		return fmt.Errorf("bundle missing containerd layer")
	}
	if layers.Kubeadm == "" {
		return fmt.Errorf("bundle missing kubeadm layer")
	}

	const remoteBundlePath = "/tmp/ko-bundle.oci.tar.gz"
	if err := r.Exec.Scp(ctx, host, r.Bundle, remoteBundlePath); err != nil {
		return fmt.Errorf("scp bundle to %s: %w", host, err)
	}

	const extractRoot = "/var/lib/ko/bundle"
	if res := r.Exec.Run(ctx, host, fmt.Sprintf(
		"set -euo pipefail; mkdir -p %s; tar -xzf %s -C %s",
		extractRoot, remoteBundlePath, extractRoot)); res.Failed() {
		return fmt.Errorf("extract bundle on %s: %w (stderr=%s)", host, res.Err, string(res.Stderr))
	}

	master1IP, err := r.resolveMaster1IP(ctx)
	if err != nil {
		return err
	}
	return r.PrepareHostFromBundle(ctx, host, master1IP, layers)
}

// resolveMaster1IP asks master-1 for its global IPv4 (same lookup as
// writeHosts, factored out so AddWorker/AddMaster don't duplicate it).
func (r *OfflineRunner) resolveMaster1IP(ctx context.Context) (string, error) {
	res := r.Exec.Run(ctx, r.Master1, "ip -4 -o addr show scope global | awk '{print $4}' | cut -d/ -f1")
	if res.Failed() {
		return "", fmt.Errorf("resolve master-1 IP: %w (stderr=%s)", res.Err, string(res.Stderr))
	}
	ip := strings.TrimSpace(string(res.Stdout))
	if ip == "" {
		return "", fmt.Errorf("no global IPv4 on master-1 %s", r.Master1)
	}
	if i := strings.IndexByte(ip, '\n'); i >= 0 {
		ip = ip[:i]
	}
	return ip, nil
}

func (n *NodeLifecycle) drainNode(ctx context.Context, host string, force bool) error {
	args := []string{"drain", host, "--ignore-daemonsets", "--delete-emptydir-data"}
	if force {
		args = append(args, "--force")
	}
	res := n.Exec.Run(ctx, "local", kubectl(n.KubeConfigPath, strings.Join(args, " ")))
	if res.Failed() {
		return fmt.Errorf("drain: %w", res.Err)
	}
	return nil
}

func (n *NodeLifecycle) deleteNode(ctx context.Context, host string) error {
	res := n.Exec.Run(ctx, "local", kubectl(n.KubeConfigPath, "delete node "+host))
	if res.Failed() {
		return fmt.Errorf("delete: %w", res.Err)
	}
	return nil
}

func (n *NodeLifecycle) masterHost() string {
	if len(n.Cfg.Nodes.Masters) == 0 {
		return ""
	}
	return n.Cfg.Nodes.Masters[0]
}

func (n *NodeLifecycle) apiServerEndpoint() string {
	if n.Cfg.HA.VIP != "" {
		return n.Cfg.HA.VIP + ":6443"
	}
	if host := n.masterHost(); host != "" {
		return host + ":6443"
	}
	return ""
}

func (n *NodeLifecycle) criSocket(host string) string {
	// k8s ≥ 1.24 dropped the in-tree dockershim — kubelet talks to the CRI
	// via a CRI endpoint, not the docker engine socket. The docker runtime
	// path therefore goes through cri-dockerd (Mirantis). containerd is its
	// own CRI endpoint. The docker engine socket at /var/run/docker.sock is
	// still where `docker` CLI / `crictl` (when configured for docker) lives,
	// but kubelet will hang forever pointing at it.
	if n.Cfg.LookupNodeOverride(host) != nil && n.Cfg.LookupNodeOverride(host).Runtime == "docker" {
		return "unix:///run/cri-dockerd/cri-dockerd.sock"
	}
	return "unix:///run/containerd/containerd.sock"
}

func (n *NodeLifecycle) kubeadmDiscoveryHash(ctx context.Context, host string) (string, error) {
	res := n.Exec.Run(ctx, host, "openssl x509 -pubkey -in /etc/kubernetes/pki/ca.crt | openssl rsa -pubin -outform der 2>/dev/null | openssl dgst -sha256 -hex | awk '{print \"sha256:\"$NF}'")
	if res.Failed() {
		return "", fmt.Errorf("compute discovery hash: %w", res.Err)
	}
	return string(trimNewlineBytes(res.Stdout)), nil
}

// kubectl builds a kubectl invocation using the cached admin.conf.
func kubectl(kubeConfigPath, args string) string {
	if kubeConfigPath == "" {
		return "kubectl " + args
	}
	return fmt.Sprintf("kubectl --kubeconfig=%s %s", kubeConfigPath, args)
}