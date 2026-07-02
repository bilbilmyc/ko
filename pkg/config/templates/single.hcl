# =============================================================================
# ko cluster config — profile: single
# =============================================================================
# Single-master / single-etcd setup. No VIP, no HA. Good for dev / CI / labs.
# Use `ha` instead if you need 3+ masters behind a VIP.
# Use `external-etcd` instead if you have a separate etcd cluster / hosts.
#
# To generate: `ko init --generate-config=single`
# To verify  : `ko doctor --config cluster.hcl`
# To apply   : `ko init  --config cluster.hcl`
# =============================================================================

cluster {
  # Cluster name (used for kube context + dashboard label)
  name = "{{.ClusterName}}"

  # Kubernetes version (matches our prebuilt bundles).
  version = "1.35"

  # Pod network CIDR — must not overlap with svc_cidr.
  cidr = "{{.ClusterCIDR}}"

  # Service network CIDR — must not overlap with cidr.
  svc_cidr = "{{.SVCCIDR}}"
}

image {
  # Upstream registry used to pull system images (kube-vip, cilium, etc.)
  registry = "registry.cn-hangzhou.aliyuncs.com"
  repository = "ko"
  tag        = "v0.0.1"

  # Mirror fallbacks (tried in order if the primary registry is unreachable).
  registry_mirrors = [
    "https://docker.m.daocloud.io",
    "https://dockerproxy.com",
    "https://docker.mirrors.ustc.edu.cn",
  ]
}

# Container runtime source — upstream containerd v2 is the default.
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

# etcd: stacked mode = one etcd pod per master, managed by kubeadm.
etcd {
  mode = "stacked"
}

# HA block is unused for the single profile. Set vip + iface + masters ≥ 3
# in `profile=ha` to enable kube-vip-backed control-plane HA.

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
  masters = ["{{.Master1}}"]
  workers = ["{{.Worker1}}", "{{.Worker2}}"]

  ssh {
    user     = "root"
    port     = 22
    key_file = "{{.SSHKeyFile}}"
  }
}

# Per-node overrides (optional). Each block targets one host.
# nodes_override "{{.Worker1}}" {
#   runtime = "docker"   # override this single node's runtime
#   arch    = "arm64"
# }

tune {
  profile        = "production"
  swap_off       = true
  kernel_modules = ["br_netfilter", "ip_vs", "overlay"]
  sysctl = {
    "net.ipv4.ip_forward"              = "1"
    "net.bridge.bridge-nf-call-iptables" = "1"
    "fs.file-max"                       = "2097152"
  }
  systemd = {
    "LimitNOFILE" = "65536"
  }
}

dashboard {
  listen = "127.0.0.1:8080"
  basic_auth {
    user     = "admin"
    password = "changeme"
  }
}

certificates {
  # 100-year validity for the internal CA — bumps default cluster certs.
  validity = "876000h"
}
