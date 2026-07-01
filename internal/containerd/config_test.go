package containerd

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig([]string{"https://mirror.example.com"}, nil)
	assert.Contains(t, cfg, `version = 2`)
	assert.Contains(t, cfg, `sandbox_image = "registry.k8s.io/pause:3.10"`)
	assert.Contains(t, cfg, `SystemdCgroup = true`)
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

func TestRender_CustomSandbox(t *testing.T) {
	cfg := Render(ConfigToml{
		Version:       2,
		SandboxImage:  "ko/pause:3.9",
	})
	assert.Contains(t, cfg, `sandbox_image = "ko/pause:3.9"`)
}

func TestSystemdUnit(t *testing.T) {
	u := systemdUnit()
	assert.True(t, strings.Contains(u, "/usr/local/bin/containerd"))
	assert.True(t, strings.Contains(u, "WantedBy=multi-user.target"))
}
