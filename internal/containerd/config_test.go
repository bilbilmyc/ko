package containerd

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig(
		[]Mirror{{Upstream: "docker.io", Endpoint: "https://mirror.example.com"}},
		nil,
	)
	assert.Contains(t, cfg, `version = 2`)
	assert.Contains(t, cfg, `sandbox_image = "registry.k8s.io/pause:3.10"`)
	assert.Contains(t, cfg, `SystemdCgroup = true`)
	// Mirror block: upstream registry hostname and mirror endpoint distinct.
	assert.Contains(t, cfg, `registry.mirrors."docker.io"`)
	assert.Contains(t, cfg, `endpoint = ["https://mirror.example.com"]`)
}

func TestDefaultConfig_Insecure(t *testing.T) {
	cfg := DefaultConfig(nil, []string{"harbor.local"})
	assert.Contains(t, cfg, `insecure_skip_verify = true`)
	assert.Contains(t, cfg, `"harbor.local"`)
}

func TestDefaultConfig_EmptyMirrors(t *testing.T) {
	cfg := DefaultConfig(nil, nil)
	assert.Contains(t, cfg, "version = 2")
	assert.NotContains(t, cfg, "endpoint")
}

// TestDefaultConfig_OfflineTuning asserts the v0.0.5 tuning is rendered
// by default: max_concurrent_downloads=3, timeout="30s",
// disable_snapshot_annotations=false, max_container_log_line_size=-1.
func TestDefaultConfig_OfflineTuning(t *testing.T) {
	cfg := DefaultConfig(nil, nil)
	assert.Contains(t, cfg, `max_concurrent_downloads = 3`)
	assert.Contains(t, cfg, `timeout = "30s"`)
	assert.Contains(t, cfg, `disable_snapshot_annotations = false`)
	assert.Contains(t, cfg, `max_container_log_line_size = -1`)
}

// TestDefaultConfig_TuningOverrides confirms callers can override the
// v0.0.5 tuning values (e.g. for online installs where a stricter
// timeout is desirable). Unspecified tuning fields fall back to the
// offline defaults.
func TestDefaultConfig_TuningOverrides(t *testing.T) {
	cfg := Render(ConfigToml{
		Version:                      2,
		SandboxImage:                 defaultSandboxImage,
		TuningMaxConcurrentDownloads: 10,
		TuningPullTimeout:            "10s",
	})
	assert.Contains(t, cfg, `max_concurrent_downloads = 10`, "explicit override must take effect")
	assert.Contains(t, cfg, `timeout = "10s"`, "explicit override must take effect")
	assert.Contains(t, cfg, `max_container_log_line_size = -1`, "default applies when caller passes 0")
}

// TestOfflineConfig_GeneratesOneMirrorPerUpstream ensures each upstream
// gets its own mirror block, all pointing at the same in-cluster
// endpoint — containerd needs separate blocks for each upstream because
// the path-rewrite is per-host.
func TestOfflineConfig_GeneratesOneMirrorPerUpstream(t *testing.T) {
	upstreams := []string{"quay.io", "registry.k8s.io", "docker.io", "ghcr.io"}
	cfg := OfflineConfig(upstreams, []string{"ko.local:5000"}, "http://ko.local:5000")
	for _, u := range upstreams {
		assert.Contains(t, cfg, `registry.mirrors."`+u+`"`, "offline config must include a mirror block for "+u)
		assert.Contains(t, cfg, `endpoint = ["http://ko.local:5000"]`)
	}
	assert.Contains(t, cfg, `insecure_skip_verify = true`)
}

// TestOfflineConfig_MarksLocalRegistryInsecure ensures the in-cluster
// registry itself is marked insecure_skip_verify (no TLS in-cluster).
func TestOfflineConfig_MarksLocalRegistryInsecure(t *testing.T) {
	cfg := OfflineConfig([]string{"docker.io"}, []string{"ko.local:5000"}, "http://ko.local:5000")
	assert.Contains(t, cfg, `configs."ko.local:5000"`)
	assert.Contains(t, cfg, `insecure_skip_verify = true`)
}

// TestMirrorsFromDockerEndpointURLs confirms the helper wraps each URL
// as a docker.io mirror — used by online init to preserve the legacy
// `registry_mirrors = ["https://…"]` HCL shape.
func TestMirrorsFromDockerEndpointURLs(t *testing.T) {
	got := MirrorsFromDockerEndpointURLs([]string{"https://a", "https://b"})
	assert.Len(t, got, 2)
	assert.Equal(t, "docker.io", got[0].Upstream)
	assert.Equal(t, "https://a", got[0].Endpoint)
	assert.Equal(t, "docker.io", got[1].Upstream)
	assert.Equal(t, "https://b", got[1].Endpoint)
}

func TestRender_CustomSandbox(t *testing.T) {
	cfg := Render(ConfigToml{
		Version:      2,
		SandboxImage: "ko/pause:3.9",
	})
	assert.Contains(t, cfg, `sandbox_image = "ko/pause:3.9"`)
}

func TestSystemdUnit(t *testing.T) {
	u := systemdUnit()
	assert.True(t, strings.Contains(u, "/usr/local/bin/containerd"))
	assert.True(t, strings.Contains(u, "WantedBy=multi-user.target"))
}
