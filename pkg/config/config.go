package config

import (
	"fmt"
	"os"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclparse"
)

type File struct {
	Cluster        ClusterBlock        `hcl:"cluster,block"`
	Image          ImageBlock          `hcl:"image,block"`
	Containerd     ContainerdBlock     `hcl:"containerd,block"`
	Runtime        RuntimeBlock        `hcl:"runtime,block"`
	Etcd           EtcdBlock           `hcl:"etcd,block"`
	HA             HABlock             `hcl:"ha,block"`
	CNI            CNIBlock            `hcl:"cni,block"`
	Nodes          NodesBlock          `hcl:"nodes,block"`
	NodesOverride  []NodesOverrideBlock `hcl:"nodes_override,block"`
	Tune           TuneBlock           `hcl:"tune,block"`
	Dashboard      DashboardBlock      `hcl:"dashboard,block"`
	Certificates   CertificatesBlock   `hcl:"certificates,block"`
	Remain         hcl.Body            `hcl:",remain"`
}

type ClusterBlock struct {
	Name    string `hcl:"name,optional"`
	Version string `hcl:"version,optional"`
	CIDR    string `hcl:"cidr,optional"`
	SVCCIDR string `hcl:"svc_cidr,optional"`
}

type ImageBlock struct {
	Registry           string   `hcl:"registry,optional"`
	Repository         string   `hcl:"repository,optional"`
	Tag                string   `hcl:"tag,optional"`
	OfflinePack        string   `hcl:"offline_pack,optional"`
	RegistryMirrors    []string `hcl:"registry_mirrors,optional"`
	InsecureRegistries []string `hcl:"insecure_registries,optional"`
}

type ContainerdBlock struct {
	Source  string `hcl:"source,optional"`
	Version string `hcl:"version,optional"`
}

type RuntimeBlock struct {
	Default string         `hcl:"default,optional"`
	Docker  DockerSubBlock `hcl:"docker,block"`
}

type DockerSubBlock struct {
	Version      string `hcl:"version,optional"`
	CgroupDriver string `hcl:"cgroup_driver,optional"`
}

type EtcdBlock struct {
	Mode      string         `hcl:"mode,optional"`
	Endpoints []string       `hcl:"endpoints,optional"`
	Members   []EtcdMemberBlock `hcl:"members,block"`
	PKIDir    string         `hcl:"pki_dir,optional"`
}

type EtcdMemberBlock struct {
	Name string `hcl:"name,label"`
	Host string `hcl:"host,optional"`
}

type HABlock struct {
	VIP          string `hcl:"vip,optional"`
	Interface    string `hcl:"iface,optional"`
	KubeVipImage string `hcl:"kube_vip_image,optional"`
}

type CNIBlock struct {
	Plugin  string          `hcl:"plugin,optional"`
	Cilium  CiliumSubBlock  `hcl:"cilium,block"`
	Flannel FlannelSubBlock `hcl:"flannel,block"`
}

type CiliumSubBlock struct {
	KubeProxyReplacement string `hcl:"kube_proxy_replacement,optional"`
}

type FlannelSubBlock struct {
	Backend string `hcl:"backend,optional"`
}

type NodesBlock struct {
	Masters []string `hcl:"masters,optional"`
	Workers []string `hcl:"workers,optional"`
	SSH     SSHBlock `hcl:"ssh,block"`
}

type NodesOverrideBlock struct {
	Host    string `hcl:"host,label"`
	Runtime string `hcl:"runtime,optional"`
	Arch    string `hcl:"arch,optional"`
	CNI     string `hcl:"cni,optional"`
}

type SSHBlock struct {
	User     string `hcl:"user,optional"`
	Port     int    `hcl:"port,optional"`
	KeyFile  string `hcl:"key_file,optional"`
	Password string `hcl:"password,optional"`
}

type TuneBlock struct {
	Profile       string            `hcl:"profile,optional"`
	SwapOff       bool              `hcl:"swap_off,optional"`
	KernelModules []string          `hcl:"kernel_modules,optional"`
	Sysctl        map[string]string `hcl:"sysctl,optional"`
	Systemd       map[string]string `hcl:"systemd,optional"`
}

type DashboardBlock struct {
	Listen      string         `hcl:"listen,optional"`
	BasicAuth   BasicAuthBlock `hcl:"basic_auth,block"`
	AllowOrigin string         `hcl:"allow_origin,optional"`
}

type BasicAuthBlock struct {
	User     string `hcl:"user,optional"`
	Password string `hcl:"password,optional"`
}

type CertificatesBlock struct {
	Validity string `hcl:"validity,optional"`
}

// topLevelBlockSchemas lists the top-level block types we recognize, with their labels.
var topLevelBlockSchemas = []hcl.BlockHeaderSchema{
	{Type: "cluster", LabelNames: nil},
	{Type: "image", LabelNames: nil},
	{Type: "containerd", LabelNames: nil},
	{Type: "runtime", LabelNames: nil},
	{Type: "etcd", LabelNames: nil},
	{Type: "members", LabelNames: []string{"name"}},
	{Type: "ha", LabelNames: nil},
	{Type: "cni", LabelNames: nil},
	{Type: "nodes", LabelNames: nil},
	{Type: "nodes_override", LabelNames: []string{"host"}},
	{Type: "tune", LabelNames: nil},
	{Type: "dashboard", LabelNames: nil},
	{Type: "certificates", LabelNames: nil},
}

// ParseFile parses a cluster.hcl file. All top-level blocks are optional; missing blocks
// fall through to ApplyDefaults to provide sensible values.
func ParseFile(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", path, err)
	}
	parser := hclparse.NewParser()
	file, diags := parser.ParseHCL(data, path)
	if diags.HasErrors() {
		return nil, fmt.Errorf("parse %q: %s", path, diags.Error())
	}

	content, _, diags := file.Body.PartialContent(&hcl.BodySchema{
		Blocks: topLevelBlockSchemas,
	})
	if diags.HasErrors() {
		return nil, fmt.Errorf("scan %q: %s", path, diags.Error())
	}

	out := &File{}
	for _, blk := range content.Blocks {
		if err := decodeOneBlock(blk, out); err != nil {
			return nil, fmt.Errorf("decode %s in %q: %w", blk.Type, path, err)
		}
	}
	return out, nil
}

func decodeOneBlock(blk *hcl.Block, out *File) error {
	var diags hcl.Diagnostics
	switch blk.Type {
	case "cluster":
		diags = gohcl.DecodeBody(blk.Body, nil, &out.Cluster)
	case "image":
		diags = gohcl.DecodeBody(blk.Body, nil, &out.Image)
	case "containerd":
		diags = gohcl.DecodeBody(blk.Body, nil, &out.Containerd)
	case "runtime":
		diags = gohcl.DecodeBody(blk.Body, nil, &out.Runtime)
	case "etcd":
		diags = gohcl.DecodeBody(blk.Body, nil, &out.Etcd)
	case "members":
		var m EtcdMemberBlock
		diags = gohcl.DecodeBody(blk.Body, nil, &m)
		if !diags.HasErrors() {
			m.Name = blk.Labels[0]
			out.Etcd.Members = append(out.Etcd.Members, m)
		}
	case "ha":
		diags = gohcl.DecodeBody(blk.Body, nil, &out.HA)
	case "cni":
		diags = gohcl.DecodeBody(blk.Body, nil, &out.CNI)
	case "nodes":
		diags = gohcl.DecodeBody(blk.Body, nil, &out.Nodes)
	case "nodes_override":
		var one NodesOverrideBlock
		diags = gohcl.DecodeBody(blk.Body, nil, &one)
		if !diags.HasErrors() {
			one.Host = blk.Labels[0]
			out.NodesOverride = append(out.NodesOverride, one)
		}
	case "tune":
		diags = gohcl.DecodeBody(blk.Body, nil, &out.Tune)
	case "dashboard":
		diags = gohcl.DecodeBody(blk.Body, nil, &out.Dashboard)
	case "certificates":
		diags = gohcl.DecodeBody(blk.Body, nil, &out.Certificates)
	default:
		return fmt.Errorf("unknown block type %q", blk.Type)
	}
	if diags.HasErrors() {
		return fmt.Errorf("%s", diags.Error())
	}
	return nil
}

func (f *File) ApplyDefaults() {
	if f.Cluster.Name == "" {
		f.Cluster.Name = "default"
	}
	if f.Cluster.Version == "" {
		f.Cluster.Version = "1.35"
	}
	if f.Cluster.CIDR == "" {
		f.Cluster.CIDR = "10.244.0.0/16"
	}
	if f.Cluster.SVCCIDR == "" {
		f.Cluster.SVCCIDR = "10.96.0.0/12"
	}
	if f.Image.Registry == "" {
		f.Image.Registry = "registry.cn-hangzhou.aliyuncs.com"
	}
	if f.Image.Repository == "" {
		f.Image.Repository = "ko"
	}
	if f.Image.Tag == "" {
		f.Image.Tag = "v0.0.1"
	}
	if len(f.Image.RegistryMirrors) == 0 {
		f.Image.RegistryMirrors = []string{
			"https://docker.m.daocloud.io",
			"https://dockerproxy.com",
			"https://docker.mirrors.ustc.edu.cn",
		}
	}
	if f.Containerd.Source == "" {
		f.Containerd.Source = "upstream"
	}
	if f.Containerd.Version == "" {
		f.Containerd.Version = "v2.0.x"
	}
	if f.Runtime.Default == "" {
		f.Runtime.Default = "containerd"
	}
	if f.Runtime.Docker.CgroupDriver == "" {
		f.Runtime.Docker.CgroupDriver = "systemd"
	}
	if f.Etcd.Mode == "" {
		f.Etcd.Mode = "stacked"
	}
	if f.Etcd.PKIDir == "" {
		f.Etcd.PKIDir = "/etc/etcd/pki"
	}
	if f.HA.KubeVipImage == "" {
		f.HA.KubeVipImage = "ghcr.io/kube-vip/kube-vip:latest"
	}
	if f.CNI.Plugin == "" {
		f.CNI.Plugin = "cilium"
	}
	if f.CNI.Cilium.KubeProxyReplacement == "" {
		f.CNI.Cilium.KubeProxyReplacement = "strict"
	}
	if f.CNI.Flannel.Backend == "" {
		f.CNI.Flannel.Backend = "vxlan"
	}
	if f.Nodes.SSH.User == "" {
		f.Nodes.SSH.User = "root"
	}
	if f.Nodes.SSH.Port == 0 {
		f.Nodes.SSH.Port = 22
	}
	if f.Tune.Profile == "" {
		f.Tune.Profile = "production"
	}
	if f.Dashboard.Listen == "" {
		f.Dashboard.Listen = "127.0.0.1:8080"
	}
	if f.Dashboard.BasicAuth.User == "" {
		f.Dashboard.BasicAuth.User = "admin"
	}
	if f.Certificates.Validity == "" {
		f.Certificates.Validity = "876000h"
	}
}

func ParseValidityDuration(s string) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("parse validity %q: %w", s, err)
	}
	return d, nil
}

// LookupNodeOverride returns the override for host, or nil if none.
func (f *File) LookupNodeOverride(host string) *NodesOverrideBlock {
	for i, ov := range f.NodesOverride {
		if ov.Host == host {
			return &f.NodesOverride[i]
		}
	}
	return nil
}
