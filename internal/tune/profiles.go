package tune

// Profile is a bundle of sysctl + modules that ko applies as a unit.
type Profile struct {
	Name    string
	Sysctl  map[string]string
	Modules []string
}

// Profiles returns the built-in profiles. Order:
//   - production: hardened defaults for HA clusters, kube-proxy replacement
//   - dev:        permissive, leaves swap on for compatibility
//   - minimal:    only the bare minimum to run kubelet + containerd
func Profiles() map[string]Profile {
	return map[string]Profile{
		"production": {
			Name: "production",
			Sysctl: map[string]string{
				"net.ipv4.ip_forward":                 "1",
				"net.bridge.bridge-nf-call-iptables":  "1",
				"net.bridge.bridge-nf-call-ip6tables": "1",
				"net.core.somaxconn":                  "32768",
				"net.ipv4.tcp_max_syn_backlog":        "32768",
				"vm.swappiness":                       "0",
				"fs.inotify.max_user_watches":         "524288",
				"fs.inotify.max_user_instances":       "512",
				"kernel.pid_max":                      "4194304",
			},
			Modules: []string{"br_netfilter", "overlay", "nf_conntrack"},
		},
		"dev": {
			Name: "dev",
			Sysctl: map[string]string{
				"net.ipv4.ip_forward":                "1",
				"net.bridge.bridge-nf-call-iptables": "1",
				"fs.inotify.max_user_watches":        "524288",
			},
			Modules: []string{"br_netfilter", "overlay"},
		},
		"minimal": {
			Name: "minimal",
			Sysctl: map[string]string{
				"net.ipv4.ip_forward":                "1",
				"net.bridge.bridge-nf-call-iptables": "1",
			},
			Modules: []string{"br_netfilter"},
		},
	}
}

// LookupProfile returns the named profile or nil if unknown.
func LookupProfile(name string) *Profile {
	if p, ok := Profiles()[name]; ok {
		return &p
	}
	return nil
}