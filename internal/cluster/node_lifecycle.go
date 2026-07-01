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
type NodeLifecycle struct {
	Cfg            *config.File
	Exec           Executor
	Kubeadm        *Kubeadm
	KubeConfigPath string // local path to admin.conf
	// InstallContainerd / InstallDocker run the runtime install on the host
	// before kubeadm joins. Either may be nil if the runtime is pre-installed.
	InstallContainerd func(ctx context.Context, host string) error
	InstallDocker     func(ctx context.Context, host string) error
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
// joins it as a worker.
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
	opts := KubeadmOptions{Token: token, DiscoveryTokenCAHash: hash}
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
func (n *NodeLifecycle) bootstrapHost(ctx context.Context, host string) error {
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
	if n.Cfg.LookupNodeOverride(host) != nil && n.Cfg.LookupNodeOverride(host).Runtime == "docker" {
		return "unix:///var/run/docker.sock"
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