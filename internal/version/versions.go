package version

// Vendored asset versions (v0.0.5+). The fetch script in
// scripts/fetch-vendor.sh reads the same values from vendor-versions.env
// at fetch time; the two must stay in sync — TestVendorVersionsSync
// in internal/version/versions_test.go asserts this.
//
// Bump a version here, re-run `ko vendor fetch`, re-run `ko pack build`.
const (
	// Containerd (containerd/containerd releases). Used as the in-cluster
	// default container runtime on every node.
	ContainerdVersion = "v2.1.0"

	// KubeVersion is the kubeadm / kubelet release ko bakes into bundles.
	// Keep these in lockstep — kubeadm vX.Y.Z expects kubelet vX.Y.Z.
	KubeVersion = "v1.32.0"

	// KubeadmVersion / KubeletVersion are explicit per-binary pins, kept
	// separate from KubeVersion so an emergency kubelet hotfix can be
	// shipped without touching the kubeadm CLI. Both currently equal
	// KubeVersion.
	KubeadmVersion = "v1.32.0"
	KubeletVersion = "v1.32.0"

	// CRIDockerdVersion (Mirantis/cri-dockerd). Needed for k8s >= 1.24
	// when `runtime.default = "docker"` — dockershim was removed.
	CRIDockerdVersion = "v0.3.14"

	// RegistryVersion (distribution/distribution). The static Go binary
	// runs as `ko-registry.service` on master-1 in the airgap cluster.
	RegistryVersion = "2.8.3"

	// DockerVersion (moby/moby). Installed only when `runtime.default =
	// "docker"`; default install path is containerd.
	DockerVersion = "28.0.0"

	// CiliumVersion is the CNI ko deploys. Keep in sync with the chart
	// version baked by fetch-vendor.sh.
	CiliumVersion = "1.16.1"

	// PrometheusStackVersion is the kube-prometheus-stack chart version
	// (also pins the prometheus / alertmanager / node-exporter / grafana
	// image versions inside third_party/images/prometheus-stack-*).
	PrometheusStackVersion = "75.6.1"
)
