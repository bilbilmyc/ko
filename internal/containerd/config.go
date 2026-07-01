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
type ConfigToml struct {
	Version             int
	RegistryMirrors     []string
	InsecureRegistries  []string
	SandboxImage        string
	AdditionalRuntimes  []Runtime
}

type Runtime struct {
	Name string
	Type string
}

const defaultSandboxImage = "registry.k8s.io/pause:3.10"

func DefaultConfig(mirrors, insecure []string) string {
	return Render(ConfigToml{
		Version:            2,
		RegistryMirrors:    mirrors,
		InsecureRegistries: insecure,
		SandboxImage:       defaultSandboxImage,
	})
}

func Render(c ConfigToml) string {
	if c.Version == 0 {
		c.Version = 2
	}
	if c.SandboxImage == "" {
		c.SandboxImage = defaultSandboxImage
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

  [plugins."io.containerd.grpc.v1.cri".registry.mirrors."{{.}}"]
    endpoint = ["{{.}}"]
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
