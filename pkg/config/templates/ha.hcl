# =============================================================================
# ko cluster config — profile: ha
# =============================================================================
# 3+ masters behind a kube-vip-managed VIP, with stacked etcd. No more
# single point of failure on the control plane.
#
# HA flow on `ko init`:
#   1. kubeadm init on the first master (whichever is listed first)
#   2. kube-vip deployed as a DaemonSet → grabs the VIP on every master
#   3. Remaining masters join via `kubeadm join --control-plane`
#   4. Workers join via `kubeadm join`
#   5. Cilium CNI installs with kubeProxyReplacement=strict
#
# To generate: `ko init --generate-config=ha`
# To verify  : `ko doctor --config cluster.hcl`
# To apply   : `ko init  --config cluster.hcl`
# =============================================================================

cluster {
  name     = "{{.ClusterName}}"
  version  = "1.35"
  cidr     = "{{.ClusterCIDR}}"
  svc_cidr = "{{.SVCCIDR}}"
}

image {
  registry   = "registry.cn-hangzhou.aliyuncs.com"
  repository = "ko"
  tag        = "v0.0.1"
  registry_mirrors = [
    "https://docker.m.daocloud.io",
    "https://dockerproxy.com",
    "https://docker.mirrors.ustc.edu.cn",
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

# Stacked etcd: one etcd pod per master, replicated by kubeadm.
etcd {
  mode = "stacked"
}

# HA — VIP is the *single* apiserver endpoint clients use.
# All apiservers serve behind it via kube-vip (BGP/ARP, no external LB).
ha {
  vip            = "{{.VIP}}"
  iface          = "{{.VIPIface}}"
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
  # Need an ODD number (1, 3, 5…) for etcd quorum. 3 is the standard.
  masters = ["{{.Master1}}", "{{.Master2}}", "{{.Master3}}"]
  workers = ["{{.Worker1}}", "{{.Worker2}}"]

  ssh {
    user     = "root"
    port     = 22
    key_file = "{{.SSHKeyFile}}"
  }
}

# Optional: pin a master to a different CNI (e.g. one master on flannel
# for cross-CNI debugging — rare, mostly here as an example).
# nodes_override "{{.Master3}}" {
#   cni = "flannel"
# }

tune {
  profile        = "production"
  swap_off       = true
  kernel_modules = ["br_netfilter", "ip_vs", "overlay"]
  sysctl = {
    "net.ipv4.ip_forward"                = "1"
    "net.bridge.bridge-nf-call-iptables" = "1"
    "fs.file-max"                         = "2097152"
  }
  systemd = {
    "LimitNOFILE" = "65536"
  }
}

dashboard {
  # Listen on all interfaces if you want LAN access; otherwise keep 127.0.0.1.
  listen = "0.0.0.0:8080"
  basic_auth {
    user     = "admin"
    password = "changeme"
  }
  # Lock to dashboard origin in production.
  # allow_origin = "https://dashboard.example.com"
}

certificates {
  validity = "876000h"
}
