// Package ciliumconfig holds default Helm values for Cilium that the init
// orchestrator reaches for when the user hasn't overridden them.
package ciliumconfig

// DefaultValues returns the sealos-style Cilium values used when
// `cni.cilium` is left at its defaults. kubeProxyReplacement is the
// only toggle the user is likely to want to change.
func DefaultValues() map[string]any {
	return map[string]any{
		"kubeProxyReplacement": "strict",
		"k8sServiceHost":       "127.0.0.1",
		"k8sServicePort":       6443,
		"ipv4NativeRoutingCIDR": "10.244.0.0/16",
		"hubble.enabled":        true,
		"hubble.relay.enabled":  true,
		"operator.replicas":     1,
		"image.pullPolicy":      "IfNotPresent",
		"ipam.mode":             "kubernetes",
	}
}
