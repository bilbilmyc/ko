package cluster

import "context"

// CleanupKubeProxy removes the kube-proxy DaemonSet and ConfigMap that kubeadm
// pre-stages. Cilium replaces it, so leaving them around causes confusion
// (and conflicts with iptables rules).
func CleanupKubeProxy(ctx context.Context, k8s Client) error {
	return k8s.Delete(ctx, "kube-system", []string{
		"daemonset.apps/kube-proxy",
		"configmap/kube-proxy",
	})
}

// Client is the minimal k8s client interface used by post-kubeadm cleanup.
// The real implementation lives in client-go (S4 / init.go wires it in).
type Client interface {
	Delete(ctx context.Context, namespace string, resources []string) error
}
