// Package docker installs Docker CE on remote nodes and configures the daemon.
package docker

import (
	"bytes"
	"fmt"
	"text/template"
)

type DaemonConfig struct {
	CgroupDriver    string
	RegistryMirrors []string
	Insecure        []string
	LogDriver       string
	LogOpts         map[string]string
}

const (
	defaultCgroupDriver = "systemd"
	defaultLogDriver    = "json-file"
	defaultVersion      = "27.5.1"
)

func DefaultDaemon(mirrors, insecure []string) string {
	return Render(DaemonConfig{
		CgroupDriver:    defaultCgroupDriver,
		RegistryMirrors: mirrors,
		Insecure:        insecure,
		LogDriver:       defaultLogDriver,
	})
}

func Render(c DaemonConfig) string {
	if c.CgroupDriver == "" {
		c.CgroupDriver = defaultCgroupDriver
	}
	if c.LogDriver == "" {
		c.LogDriver = defaultLogDriver
	}
	var buf bytes.Buffer
	if err := daemonTpl.Execute(&buf, c); err != nil {
		panic(fmt.Sprintf("daemonTpl execute: %v", err))
	}
	return buf.String()
}

var daemonTpl = template.Must(template.New("docker-daemon").Parse(`{
  "log-driver": "{{.LogDriver}}",
{{- if .LogOpts}}
  "log-opts": {
{{- range $k, $v := .LogOpts}}
    "{{$k}}": "{{$v}}",
{{- end}}
  },
{{- end}}
  "exec-opts": ["native.cgroupdriver={{.CgroupDriver}}"],
  "storage-driver": "overlay2",
  "features": {
    "containerd-snapshotter": false
  },
{{- if .RegistryMirrors}}
  "registry-mirrors": [
{{- range .RegistryMirrors}}
    "{{.}}",
{{- end}}
  ],
{{- end}}
{{- if .Insecure}}
  "insecure-registries": [
{{- range .Insecure}}
    "{{.}}",
{{- end}}
  ],
{{- end}}
  "live-restore": true,
  "userland-proxy": false
}
`))
