package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeHCL(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "cluster.hcl")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

func TestParse_Minimal(t *testing.T) {
	path := writeHCL(t, `
cluster {
  name    = "test"
  version = "1.35"
  cidr    = "10.244.0.0/16"
}
`)
	cfg, err := ParseFile(path)
	require.NoError(t, err)
	assert.Equal(t, "test", cfg.Cluster.Name)
	assert.Equal(t, "1.35", cfg.Cluster.Version)
	assert.Equal(t, "10.244.0.0/16", cfg.Cluster.CIDR)
}

func TestParse_Full(t *testing.T) {
	path := writeHCL(t, `
cluster {
  name     = "prod"
  version  = "1.36"
  cidr     = "10.244.0.0/16"
  svc_cidr = "10.96.0.0/12"
}

image {
  registry   = "registry.example.com"
  repository = "ko"
  tag        = "v0.0.1"
  registry_mirrors = [
    "https://mirror.a.com",
    "https://mirror.b.com",
  ]
}

containerd {
  source  = "upstream"
  version = "v2.0.x"
}

runtime {
  default = "containerd"
  docker {
    version       = "27.x"
    cgroup_driver = "systemd"
  }
}

etcd {
  mode = "stacked"
}

ha {
  vip            = "10.0.0.100"
  iface          = "eth0"
  kube_vip_image = "ghcr.io/kube-vip/kube-vip:latest"
}

cni {
  plugin = "cilium"
  cilium {
    kube_proxy_replacement = "strict"
  }
  flannel {
    backend = "vxlan"
  }
}

nodes {
  masters = ["10.0.0.5", "10.0.0.6"]
  workers = ["10.0.0.20", "10.0.0.21"]
  ssh {
    user     = "root"
    port     = 22
    key_file = "~/.ssh/id_rsa"
  }
}

nodes_override "10.0.0.20" {
  runtime = "docker"
}
nodes_override "10.0.0.21" {
  cni = "flannel"
}

tune {
  profile        = "production"
  swap_off       = true
  kernel_modules = ["br_netfilter", "ip_vs"]
  sysctl = {
    "net.ipv4.ip_forward" = "1"
  }
  systemd = {
    "LimitNOFILE" = "65536"
  }
}

dashboard {
  listen = "127.0.0.1:8080"
  basic_auth {
    user     = "admin"
    password = "secret"
  }
}

certificates {
  validity = "876000h"
}
`)
	cfg, err := ParseFile(path)
	require.NoError(t, err)
	cfg.ApplyDefaults()

	assert.Equal(t, "prod", cfg.Cluster.Name)
	assert.Equal(t, "1.36", cfg.Cluster.Version)
	assert.Equal(t, "registry.example.com", cfg.Image.Registry)
	assert.Equal(t, []string{"https://mirror.a.com", "https://mirror.b.com"}, cfg.Image.RegistryMirrors)
	assert.Equal(t, "upstream", cfg.Containerd.Source)
	assert.Equal(t, "containerd", cfg.Runtime.Default)
	assert.Equal(t, "stacked", cfg.Etcd.Mode)
	assert.Equal(t, "10.0.0.100", cfg.HA.VIP)
	assert.Equal(t, "cilium", cfg.CNI.Plugin)
	assert.Equal(t, "strict", cfg.CNI.Cilium.KubeProxyReplacement)
	assert.Equal(t, []string{"10.0.0.5", "10.0.0.6"}, cfg.Nodes.Masters)
	assert.Equal(t, "docker", cfg.NodesOverride[0].Runtime)
	assert.Equal(t, "10.0.0.20", cfg.NodesOverride[0].Host)
	assert.Equal(t, "flannel", cfg.NodesOverride[1].CNI)
	assert.Equal(t, "root", cfg.Nodes.SSH.User)
	assert.Equal(t, "production", cfg.Tune.Profile)
	assert.Equal(t, "127.0.0.1:8080", cfg.Dashboard.Listen)
	assert.Equal(t, "876000h", cfg.Certificates.Validity)
}

func TestParse_Defaults(t *testing.T) {
	path := writeHCL(t, `
cluster {
  name    = "x"
  version = "1.35"
}
`)
	cfg, err := ParseFile(path)
	require.NoError(t, err)
	cfg.ApplyDefaults()

	assert.Equal(t, "registry.cn-hangzhou.aliyuncs.com", cfg.Image.Registry)
	assert.Equal(t, "ko", cfg.Image.Repository)
	assert.Equal(t, "v0.0.1", cfg.Image.Tag)
	assert.NotEmpty(t, cfg.Image.RegistryMirrors)
	assert.Equal(t, "upstream", cfg.Containerd.Source)
	assert.Equal(t, "containerd", cfg.Runtime.Default)
	assert.Equal(t, "stacked", cfg.Etcd.Mode)
	assert.Equal(t, "cilium", cfg.CNI.Plugin)
	assert.Equal(t, "strict", cfg.CNI.Cilium.KubeProxyReplacement)
	assert.Equal(t, "production", cfg.Tune.Profile)
	assert.Equal(t, "127.0.0.1:8080", cfg.Dashboard.Listen)
	assert.Equal(t, "876000h", cfg.Certificates.Validity)
}

func TestParse_MissingFile(t *testing.T) {
	_, err := ParseFile("/nonexistent/cluster.hcl")
	require.Error(t, err)
}

func TestParse_InvalidHCL(t *testing.T) {
	path := writeHCL(t, `this is not valid hcl { { {`)
	_, err := ParseFile(path)
	require.Error(t, err)
}

func TestParseValidityDuration(t *testing.T) {
	d, err := ParseValidityDuration("876000h")
	require.NoError(t, err)
	// 100 years approx: 100 * 365 * 24h
	assert.InDelta(t, float64(100*365*24), d.Hours(), 1.0)
}
