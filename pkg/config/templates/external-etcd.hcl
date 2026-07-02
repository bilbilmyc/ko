# =============================================================================
# ko cluster config — profile: external-etcd
# =============================================================================
# A HA masters cluster that talks to an ETCD CLUSTER YOU MANAGE ELSEWHERE.
# We do NOT install etcd as pods — kube-apiserver just gets a list of
# endpoints + PKI certs and connects to them.
#
# When to use this:
#   * You have a dedicated etcd team / iron already
#   * You need etcd backup cadence outside what kubeadm stack does
#   * You want etcd on its own hardware / VMs (different failure domain)
#
# Pre-reqs (NOT installed by `ko init`):
#   * An odd number (3 / 5) of etcd hosts reachable from each master
#   * mTLS:   ca.crt, server.crt, server.key   (CN=etcd-server, SAN=<host>)
#             ca.crt, client.crt, client.key   (for kube-apiserver → etcd)
#   * Endpoints listen on https://<host>:2379
#
# Provisioning:
#   ko init will rsync the certs to every master under
#   /etc/kubernetes/pki/etcd/external/. You can also pre-place them there
#   and kubeadm will pick them up.
#
# To generate: `ko init --generate-config=external-etcd`
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

# External etcd — `endpoints` are the only required field for this mode.
# `ca`, `client_cert`, `client_key` are paths (relative or absolute) on
# every master. If left empty, ko will look under
# /etc/kubernetes/pki/etcd/external/.
etcd {
  mode      = "external"
  endpoints = [{{.EtcdHosts}}]
  pki_dir   = "/etc/etcd/pki"
  # ca            = "/etc/kubernetes/pki/etcd/external/ca.crt"
  # client_cert   = "/etc/kubernetes/pki/etcd/external/client.crt"
  # client_key    = "/etc/kubernetes/pki/etcd/external/client.key"
}

# Member list — one block per node. `ko etcd install` will deploy the
# binary, lay out the PKI, and start the systemd unit on each host.
# Hosts must be reachable via SSH with the same user/key as masters.
members "etcd-1" { host = "10.0.0.31" }
members "etcd-2" { host = "10.0.0.32" }
members "etcd-3" { host = "10.0.0.33" }

ha {
  # VIP is still recommended even with external etcd — masters can come
  # and go (node lifecycle) without disturbing the apiserver endpoint.
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
  masters = ["{{.Master1}}", "{{.Master2}}", "{{.Master3}}"]
  workers = ["{{.Worker1}}", "{{.Worker2}}"]

  ssh {
    user     = "root"
    port     = 22
    key_file = "{{.SSHKeyFile}}"
  }
}

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
  listen = "0.0.0.0:8080"
  basic_auth {
    user     = "admin"
    password = "changeme"
  }
}

certificates {
  validity = "876000h"
}
