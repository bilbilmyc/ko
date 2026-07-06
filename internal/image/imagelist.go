package image

import "strings"

// K8sImagesForVersion returns the static list of control-plane + CNI-host
// images kubeadm pulls at init for the given k8s version. This is the
// v1.32 set; older/newer versions may differ — keep this in sync with
// kubeadm's `kubeadm config images list` output.
//
// Used by OfflineRunner.pushImages to know which refs to retag and push
// into the in-cluster registry after `ctr -n=k8s.io images import`. The
// pre-baked tar at third_party/images/k8s-<ver>-<arch>.tar contains all
// of these as a docker-archive.
func K8sImagesForVersion(version, arch string) []string {
	v := strings.TrimPrefix(version, "v")
	registry := "registry.k8s.io"
	_ = arch // arch only affects pause on some k8s versions; we use the multi-arch default.
	return []string{
		registry + "/kube-apiserver:v" + v,
		registry + "/kube-controller-manager:v" + v,
		registry + "/kube-scheduler:v" + v,
		registry + "/kube-proxy:v" + v,
		registry + "/coredns/coredns:v1.11.3",
		registry + "/pause:3.10",
		registry + "/etcd:3.5.16-0",
	}
}

// CiliumImagesForVersion returns the static list of images the cilium
// helm chart (1.16.x) deploys. Cilium 1.17+ may add/drop — keep this in
// sync. Used by OfflineRunner.pushImages the same way K8sImagesForVersion is.
func CiliumImagesForVersion(version string) []string {
	return []string{
		"quay.io/cilium/cilium:v" + version,
		"quay.io/cilium/operator-generic:v" + version,
		"quay.io/cilium/hubble-relay:v" + version,
		"quay.io/cilium/hubble-ui:v0.13.2",
		"quay.io/cilium/hubble-ui-backend:v0.13.2",
		"quay.io/cilium/certgen:v0.2.3",
	}
}

// PrometheusImagesForVersion returns the static list of images
// kube-prometheus-stack (chart 75.6.1) deploys. Used by any future
// prometheus offline install path — the pre-baked tar at
// third_party/images/prometheus-stack-<ver>.tar contains all of them.
func PrometheusImagesForVersion(stackVersion string) []string {
	return []string{
		"quay.io/prometheus-operator/prometheus-operator:v0.81.0",
		"quay.io/prometheus/prometheus:v3.2.1",
		"quay.io/prometheus/alertmanager:v0.28.1",
		"quay.io/prometheus/node-exporter:v1.8.2",
		"registry.k8s.io/kube-state-metrics/kube-state-metrics:v2.13.0",
		"grafana/grafana:11.3.1",
	}
}
