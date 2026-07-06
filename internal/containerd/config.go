package containerd

import (
	"bytes"
	"fmt"
	"text/template"
)

// ConfigToml is the containerd config rendered to /etc/containerd/config.toml.
//
// All nodes share the same config (per SPEC §3.3). User edits go in
// vendor/containerd/config.toml and `ko pack` bakes them in; at runtime `ko init`
// writes the same content to every node.
//
// The Tuning* fields below are the v0.0.5 offline-airgap defaults
// (sensible for airgap; not strictly required for online). Leaving them
// at zero uses the offline defaults (3 concurrent downloads, 30s pull
// timeout, snapshot annotations enabled, unbounded container log lines).
type ConfigToml struct {
	Version            int
	RegistryMirrors    []Mirror
	InsecureRegistries []string
	SandboxImage       string
	AdditionalRuntimes []Runtime

	// v0.0.5 offline-airgap tuning — applied to both online and offline
	// paths so the configs are identical regardless of install mode.
	// Zero values fall back to OfflineTuningDefaults below.
	TuningMaxConcurrentDownloads  int
	TuningPullTimeout             string
	TuningDisableSnapshotAnnos    bool // false = annotations ON (default)
	TuningMaxContainerLogLineSize int
}

// Mirror maps an upstream registry (the host the kubelet asks for, e.g.
// "quay.io") to a mirror endpoint (the URL the kubelet actually pulls
// from, e.g. "http://ko.local:5000"). A single upstream can map to
// multiple endpoints; containerd tries them in order.
type Mirror struct {
	Upstream string
	Endpoint string
}

type Runtime struct {
	Name string
	Type string
}

const (
	defaultSandboxImage = "registry.k8s.io/pause:3.10"

	// OfflineTuningMaxConcurrentDownloads caps parallel image pulls so a
	// single kubelet doesn't open N×registry-connections and saturate the
	// in-cluster registry at init time. Default 3 (matches containerd 2.x
	// built-in default for many distros).
	OfflineTuningMaxConcurrentDownloads = 3

	// OfflineTuningPullTimeout widens the per-blob pull timeout from the
	// 10s default — airgap clusters occasionally hit slow filesystem reads
	// on the registry's storage driver.
	OfflineTuningPullTimeout = "30s"

	// OfflineTuningMaxContainerLogLineSize = -1 means unbounded. containerd's
	// default is 16 KiB which truncates k8s component logs and makes
	// post-mortem debugging harder. Unbounded for airgap ops convenience.
	OfflineTuningMaxContainerLogLineSize = -1

	// OfflineMirrorUpstreams is the canonical set of upstream registries
	// any of the bundled images come from. The offline runner mirrors
	// each of these to the in-cluster registry.
	OfflineMirrorUpstreams = "quay.io registry.k8s.io docker.io ghcr.io"
)

// DefaultConfig renders the offline-tuned config (mirrors + insecure +
// v0.0.5 tuning). Online init gets the same tuning — the tuning is
// universally sensible, not airgap-specific.
//
// `mirrors` is a list of upstream→endpoint pairs. Online callers usually
// pass one entry per public mirror (with Upstream="docker.io"); offline
// callers should use OfflineConfig instead, which generates the full set
// of offline mirrors pointing at the in-cluster registry.
func DefaultConfig(mirrors []Mirror, insecure []string) string {
	return Render(ConfigToml{
		Version:                       2,
		RegistryMirrors:               mirrors,
		InsecureRegistries:            insecure,
		SandboxImage:                  defaultSandboxImage,
		TuningMaxConcurrentDownloads:  OfflineTuningMaxConcurrentDownloads,
		TuningPullTimeout:             OfflineTuningPullTimeout,
		TuningMaxContainerLogLineSize: OfflineTuningMaxContainerLogLineSize,
	})
}

// OfflineConfig renders the full config the offline runner writes to
// every node: every bundled-image upstream mirrored to `mirrorEndpoint`
// (typically "http://ko.local:5000"), `insecureRegistries` marked
// insecure_skip_verify, and the v0.0.5 offline tuning applied. The
// returned string is a complete /etc/containerd/config.toml — write it
// atomically (`cat >`) and restart containerd.
//
// `upstreams` is a list of upstream registry hostnames. `mirrorEndpoint`
// is the URL all of them point at (e.g. "http://ko.local:5000"). Each
// upstream is registered as a separate mirror so containerd rewrites
// quay.io/cilium/cilium → ko.local:5000/cilium/cilium (and similarly
// for the other upstreams) without per-image config.
func OfflineConfig(upstreams []string, insecureRegistries []string, mirrorEndpoint string) string {
	mirrors := make([]Mirror, 0, len(upstreams))
	for _, u := range upstreams {
		mirrors = append(mirrors, Mirror{Upstream: u, Endpoint: mirrorEndpoint})
	}
	return DefaultConfig(mirrors, insecureRegistries)
}

// MirrorsFromDockerEndpointURLs converts a list of public docker.io
// mirror URLs (the legacy `registry_mirrors` HCL field) to []Mirror with
// Upstream="docker.io". Used by online init / `ko node add` to preserve
// the existing HCL shape (a flat list of endpoints). For multi-upstream
// configurations, build the []Mirror directly.
func MirrorsFromDockerEndpointURLs(endpoints []string) []Mirror {
	out := make([]Mirror, 0, len(endpoints))
	for _, e := range endpoints {
		out = append(out, Mirror{Upstream: "docker.io", Endpoint: e})
	}
	return out
}

func Render(c ConfigToml) string {
	if c.Version == 0 {
		c.Version = 2
	}
	if c.SandboxImage == "" {
		c.SandboxImage = defaultSandboxImage
	}
	// Apply offline-tuning defaults when callers don't override.
	if c.TuningMaxConcurrentDownloads == 0 {
		c.TuningMaxConcurrentDownloads = OfflineTuningMaxConcurrentDownloads
	}
	if c.TuningPullTimeout == "" {
		c.TuningPullTimeout = OfflineTuningPullTimeout
	}
	if c.TuningMaxContainerLogLineSize == 0 {
		c.TuningMaxContainerLogLineSize = OfflineTuningMaxContainerLogLineSize
	}
	var buf bytes.Buffer
	if err := configTpl.Execute(&buf, c); err != nil {
		// template execution on a static template should never error
		panic(fmt.Sprintf("configTpl execute: %v", err))
	}
	return buf.String()
}

var configTpl = template.Must(template.New("containerd").Option("missingkey=zero").Parse(`version = {{.Version}}

[plugins]
  [plugins."io.containerd.snapshotter.v1.native"]
{{- range .RegistryMirrors}}

  [plugins."io.containerd.grpc.v1.cri".registry.mirrors."{{.Upstream}}"]
    endpoint = ["{{.Endpoint}}"]
{{- end}}

{{- range .InsecureRegistries}}

  [plugins."io.containerd.grpc.v1.cri".registry.configs."{{.}}"]
    [plugins."io.containerd.grpc.v1.cri".registry.configs."{{.}}".tls]
      insecure_skip_verify = true
{{- end}}

  [plugins."io.containerd.grpc.v1.cri"]
    sandbox_image = "{{.SandboxImage}}"
    disable_apparmor = true
    disable_cgroup = false
    disable_hugetlb_controller = true
    restrict_oom_score_adj = true
    max_concurrent_downloads = {{.TuningMaxConcurrentDownloads}}
    disable_snapshot_annotations = {{if .TuningDisableSnapshotAnnos}}true{{else}}false{{end}}
{{ if .TuningPullTimeout }}    timeout = "{{.TuningPullTimeout}}"
{{ end }}{{ if ne .TuningMaxContainerLogLineSize 0 }}    max_container_log_line_size = {{.TuningMaxContainerLogLineSize}}
{{ end }}
{{- range .AdditionalRuntimes}}

  [plugins."io.containerd.grpc.v1.cri".containerd.runtimes."{{.Name}}"]
    runtime_type = "{{.Type}}"
{{- end}}

  [plugins."io.containerd.grpc.v1.cri".containerd]
    [plugins."io.containerd.grpc.v1.cri".containerd.runtimes]
      [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc]
        runtime_type = "io.containerd.runc.v2"
        [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc.options]
          SystemdCgroup = true
`))
