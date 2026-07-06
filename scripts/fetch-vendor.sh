#!/usr/bin/env bash
# scripts/fetch-vendor.sh — populate vendor/ with every binary + image +
# chart ko needs to build the offline bundle.
#
# Idempotent: skips a file that already exists at vendor/<path>. Sizes are
# sanity-checked (>= 1KB) so a 404 page masquerading as a tarball is
# rejected and re-fetched.
#
# Reads versions from vendor-versions.env. Override with env vars, e.g.
#   CONTAINERD_VERSION=v2.0.5 ./scripts/fetch-vendor.sh
#
# Usage:
#   ./scripts/fetch-vendor.sh                  # all assets
#   ./scripts/fetch-vendor.sh containerd       # one asset
#   ./scripts/fetch-vendor.sh --print-paths    # dry-run: print where each
#                                              # asset would land (no fetch)
#
# Exit code: 0 on full success, 1 if any download failed.

set -euo pipefail

# ---- locate project root ----
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
VENDOR="$ROOT/vendor"
VERSIONS_FILE="$ROOT/vendor-versions.env"

if [[ ! -f "$VERSIONS_FILE" ]]; then
    echo "ERROR: $VERSIONS_FILE not found" >&2
    exit 1
fi
# shellcheck source=/dev/null
set -a; source "$VERSIONS_FILE"; set +a

# ---- tiny logger ----
log()  { echo "[fetch-vendor] $*" >&2; }
die()  { log "ERROR: $*"; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

# ---- fetch helper: $1 = url, $2 = dest, $3 = min size bytes (default 1024) ----
fetch() {
    local url="$1" dest="$2" min_size="${3:-1024}"
    if [[ -f "$dest" ]] && [[ $(stat -f%z "$dest" 2>/dev/null || stat -c%s "$dest" 2>/dev/null || echo 0) -ge "$min_size" ]]; then
        log "skip  $(realpath --relative-to="$ROOT" "$dest" 2>/dev/null || echo "$dest")"
        return 0
    fi
    mkdir -p "$(dirname "$dest")"
    log "fetch $(realpath --relative-to="$ROOT" "$dest" 2>/dev/null || echo "$dest")"
    if have curl; then
        if ! curl -fsSL --retry 3 --retry-delay 2 -o "$dest.tmp" "$url"; then
            rm -f "$dest.tmp"
            die "curl failed: $url"
        fi
    elif have wget; then
        if ! wget -q -O "$dest.tmp" "$url"; then
            rm -f "$dest.tmp"
            die "wget failed: $url"
        fi
    else
        die "neither curl nor wget available; install one"
    fi
    mv "$dest.tmp" "$dest"
    local sz; sz=$(stat -f%z "$dest" 2>/dev/null || stat -c%s "$dest" 2>/dev/null || echo 0)
    [[ "$sz" -ge "$min_size" ]] || die "downloaded file too small ($sz bytes): $url"
    log "ok    $dest ($sz bytes)"
}

# ---- nerdctl/docker pull+save helper ----
pull_save_image() {
    local imgs=("$@")
    local tool
    if have nerdctl; then tool=nerdctl
    elif have docker; then tool=docker
    else die "neither nerdctl nor docker available; install one to bake image archives"
    fi
    log "pull  (${tool}) ${imgs[*]}"
    "${tool}" pull "${imgs[@]}" >/dev/null
    local dest="$VENDOR/images/_partial.tmp"
    "${tool}" save -o "$dest" "${imgs[@]}" >/dev/null
    echo "$dest"
}

# ---- per-asset fetchers ----

fetch_containerd() {
    local v="${CONTAINERD_VERSION#v}"
    fetch "https://github.com/containerd/containerd/releases/download/v${v}/containerd-${v}-linux-amd64.tar.gz" \
          "$VENDOR/containerd/${CONTAINERD_VERSION}/linux-amd64.tar.gz"
    fetch "https://github.com/containerd/containerd/releases/download/v${v}/containerd-${v}-linux-arm64.tar.gz" \
          "$VENDOR/containerd/${CONTAINERD_VERSION}/linux-arm64.tar.gz"
}

fetch_kubeadm() {
    local v="${KUBE_VERSION#v}"
    # dl.k8s.io serves the bare kubeadm binary; wrap it in a tar.gz so the
    # bundle layer shape matches containerd/registry/cri-dockerd (one
    # ./kubeadm entry). The Go code does the wrap at build time — fetch
    # here is just the raw binary.
    local amd="$VENDOR/kubeadm/${KUBE_VERSION}/linux-amd64.tar.gz"
    local arm="$VENDOR/kubeadm/${KUBE_VERSION}/linux-arm64.tar.gz"
    if [[ ! -f "$amd" ]] || [[ $(stat -f%z "$amd" 2>/dev/null || stat -c%s "$amd" 2>/dev/null || echo 0) -lt 1024 ]]; then
        local tmp_amd; tmp_amd=$(mktemp)
        fetch "https://dl.k8s.io/release/v${v}/bin/linux/amd64/kubeadm" "$tmp_amd" 50000000
        wrap_binary "$tmp_amd" "$amd" kubeadm
        rm -f "$tmp_amd"
    else
        log "skip  $(realpath --relative-to="$ROOT" "$amd" 2>/dev/null || echo "$amd")"
    fi
    if [[ ! -f "$arm" ]] || [[ $(stat -f%z "$arm" 2>/dev/null || stat -c%s "$arm" 2>/dev/null || echo 0) -lt 1024 ]]; then
        local tmp_arm; tmp_arm=$(mktemp)
        fetch "https://dl.k8s.io/release/v${v}/bin/linux/arm64/kubeadm" "$tmp_arm" 50000000
        wrap_binary "$tmp_arm" "$arm" kubeadm
        rm -f "$tmp_arm"
    else
        log "skip  $(realpath --relative-to="$ROOT" "$arm" 2>/dev/null || echo "$arm")"
    fi
}

fetch_kubelet() {
    local v="${KUBE_VERSION#v}"
    local amd="$VENDOR/kubelet/${KUBE_VERSION}/linux-amd64.tar.gz"
    local arm="$VENDOR/kubelet/${KUBE_VERSION}/linux-arm64.tar.gz"
    if [[ ! -f "$amd" ]] || [[ $(stat -f%z "$amd" 2>/dev/null || stat -c%s "$amd" 2>/dev/null || echo 0) -lt 1024 ]]; then
        local tmp_amd; tmp_amd=$(mktemp)
        fetch "https://dl.k8s.io/release/v${v}/bin/linux/amd64/kubelet" "$tmp_amd" 100000000
        wrap_binary "$tmp_amd" "$amd" kubelet
        rm -f "$tmp_amd"
    else
        log "skip  $(realpath --relative-to="$ROOT" "$amd" 2>/dev/null || echo "$amd")"
    fi
    if [[ ! -f "$arm" ]] || [[ $(stat -f%z "$arm" 2>/dev/null || stat -c%s "$arm" 2>/dev/null || echo 0) -lt 1024 ]]; then
        local tmp_arm; tmp_arm=$(mktemp)
        fetch "https://dl.k8s.io/release/v${v}/bin/linux/arm64/kubelet" "$tmp_arm" 100000000
        wrap_binary "$tmp_arm" "$arm" kubelet
        rm -f "$tmp_arm"
    else
        log "skip  $(realpath --relative-to="$ROOT" "$arm" 2>/dev/null || echo "$arm")"
    fi
}

# wrap_binary: take a single downloaded binary and wrap it in a tar.gz
# containing one ./<name> entry. The bundle layer shape is always
# "tar.gz with one root entry" so offline init can extract uniformly.
wrap_binary() {
    local src="$1" dest="$2" name="$3"
    local tmp; tmp=$(mktemp -d)
    mkdir -p "$(dirname "$dest")"
    cp "$src" "$tmp/$name"
    chmod 0755 "$tmp/$name"
    (cd "$tmp" && tar -czf "$dest" "$name")
    rm -rf "$tmp"
    log "ok    $dest (wrapped from $name)"
}

fetch_cri_dockerd() {
    # Mirantis/cri-dockerd ships tar.gz with a ./cri-dockerd binary.
    # CRI_DOCKERD_VERSION is the upstream tag (e.g. v0.3.14) — used
    # verbatim in both the URL path and the asset filename.
    local url_base="https://github.com/Mirantis/cri-dockerd/releases/download/${CRI_DOCKERD_VERSION}"
    fetch "$url_base/cri-dockerd-${CRI_DOCKERD_VERSION}-linux-amd64.tar.gz" \
          "$VENDOR/cri-dockerd/${CRI_DOCKERD_VERSION}/linux-amd64.tar.gz"
    fetch "$url_base/cri-dockerd-${CRI_DOCKERD_VERSION}-linux-arm64.tar.gz" \
          "$VENDOR/cri-dockerd/${CRI_DOCKERD_VERSION}/linux-arm64.tar.gz"
}

fetch_docker() {
    # Docker CE: download .deb (Debian/Ubuntu) and .rpm (RHEL/CentOS) per
    # arch. Names follow Docker's official channel layout.
    local v="${DOCKER_VERSION}"
    local deb_base="https://download.docker.com/linux/static/stable"
    fetch "${deb_base}/x86_64/docker-${v}.tgz" \
          "$VENDOR/docker/static/x86_64/docker-${v}.tgz"
    fetch "${deb_base}/aarch64/docker-${v}.tgz" \
          "$VENDOR/docker/static/aarch64/docker-${v}.tgz"
    # .deb / .rpm come from the OS package index; only .deb for debian and
    # .rpm for fedora/rhel are needed. Pulling the full repo index is heavy;
    # pin to known mirrors for now (deb.debian.org / rpmfind is unreliable).
    # For the airgap case, operators can drop their pre-curated .deb/.rpm
    # into vendor/docker/{deb,rpm}/<arch>/ manually and the script will
    # leave them alone (idempotent skip).
    local deb_amd="$VENDOR/docker/deb/amd64/docker-ce_${v}-1~debian.12_amd64.deb"
    local deb_arm="$VENDOR/docker/deb/arm64/docker-ce_${v}-1~debian.12_arm64.deb"
    if [[ ! -f "$deb_amd" ]]; then
        log "skip  $(realpath --relative-to="$ROOT" "$deb_amd" 2>/dev/null || echo "$deb_amd") (manual; drop .deb here for airgap docker install)"
    else
        log "skip  $(realpath --relative-to="$ROOT" "$deb_amd" 2>/dev/null || echo "$deb_amd")"
    fi
    if [[ ! -f "$deb_arm" ]]; then
        log "skip  $(realpath --relative-to="$ROOT" "$deb_arm" 2>/dev/null || echo "$deb_arm") (manual; drop .deb here for airgap docker install)"
    else
        log "skip  $(realpath --relative-to="$ROOT" "$deb_arm" 2>/dev/null || echo "$deb_arm")"
    fi
    local rpm_amd="$VENDOR/docker/rpm/x86_64/docker-ce-${v}.el9.x86_64.rpm"
    local rpm_arm="$VENDOR/docker/rpm/aarch64/docker-ce-${v}.el9.aarch64.rpm"
    if [[ ! -f "$rpm_amd" ]]; then
        log "skip  $(realpath --relative-to="$ROOT" "$rpm_amd" 2>/dev/null || echo "$rpm_amd") (manual; drop .rpm here for airgap docker install)"
    else
        log "skip  $(realpath --relative-to="$ROOT" "$rpm_amd" 2>/dev/null || echo "$rpm_amd")"
    fi
    if [[ ! -f "$rpm_arm" ]]; then
        log "skip  $(realpath --relative-to="$ROOT" "$rpm_arm" 2>/dev/null || echo "$rpm_arm") (manual; drop .rpm here for airgap docker install)"
    else
        log "skip  $(realpath --relative-to="$ROOT" "$rpm_arm" 2>/dev/null || echo "$rpm_arm")"
    fi
}

fetch_registry() {
    local v="${REGISTRY_VERSION}"
    local url_base="https://github.com/distribution/distribution/releases/download/v${v}"
    local amd_tar="$VENDOR/registry/v${REGISTRY_VERSION}/linux-amd64.tar.gz"
    local arm_tar="$VENDOR/registry/v${REGISTRY_VERSION}/linux-arm64.tar.gz"
    if [[ ! -f "$amd_tar" ]] || [[ $(stat -f%z "$amd_tar" 2>/dev/null || stat -c%s "$amd_tar" 2>/dev/null || echo 0) -lt 1024 ]]; then
        local tmp_amd; tmp_amd=$(mktemp)
        fetch "${url_base}/registry_${v}_linux_amd64.tar.gz" "$tmp_amd" 10000000
        # GitHub release tarball is a tar.gz with ./registry at root.
        # Extract just the binary and re-wrap to match the kubeadm shape.
        local tmp_dir; tmp_dir=$(mktemp -d)
        tar -xzf "$tmp_amd" -C "$tmp_dir"
        [[ -f "$tmp_dir/registry" ]] || die "no ./registry in $tmp_amd"
        wrap_binary "$tmp_dir/registry" "$amd_tar" registry
        rm -rf "$tmp_dir" "$tmp_amd"
    else
        log "skip  $(realpath --relative-to="$ROOT" "$amd_tar" 2>/dev/null || echo "$amd_tar")"
    fi
    if [[ ! -f "$arm_tar" ]] || [[ $(stat -f%z "$arm_tar" 2>/dev/null || stat -c%s "$arm_tar" 2>/dev/null || echo 0) -lt 1024 ]]; then
        local tmp_arm; tmp_arm=$(mktemp)
        fetch "${url_base}/registry_${v}_linux_arm64.tar.gz" "$tmp_arm" 10000000
        local tmp_dir; tmp_dir=$(mktemp -d)
        tar -xzf "$tmp_arm" -C "$tmp_dir"
        [[ -f "$tmp_dir/registry" ]] || die "no ./registry in $tmp_arm"
        wrap_binary "$tmp_dir/registry" "$arm_tar" registry
        rm -rf "$tmp_dir" "$tmp_arm"
    else
        log "skip  $(realpath --relative-to="$ROOT" "$arm_tar" 2>/dev/null || echo "$arm_tar")"
    fi
}

fetch_k8s_images() {
    local v="${KUBE_VERSION#v}"
    local amd="$VENDOR/images/k8s-${KUBE_VERSION}-amd64.tar"
    local arm="$VENDOR/images/k8s-${KUBE_VERSION}-arm64.tar"
    local imgs=(
        "registry.k8s.io/kube-apiserver:v${v}"
        "registry.k8s.io/kube-controller-manager:v${v}"
        "registry.k8s.io/kube-scheduler:v${v}"
        "registry.k8s.io/kube-proxy:v${v}"
        "registry.k8s.io/coredns/coredns:v1.11.3"
        "registry.k8s.io/pause:3.10"
        "registry.k8s.io/etcd:3.5.16-0"
    )
    [[ -f "$amd" && $(stat -f%z "$amd" 2>/dev/null || stat -c%s "$amd" 2>/dev/null || echo 0) -ge 100000 ]] && {
        log "skip  $(realpath --relative-to="$ROOT" "$amd" 2>/dev/null || echo "$amd")"
    } || {
        local tool; if have nerdctl; then tool=nerdctl; elif have docker; then tool=docker; else die "need nerdctl or docker for image fetch"; fi
        log "pull+save (${tool}) k8s v${v} (amd64)"
        "${tool}" pull --platform linux/amd64 "${imgs[@]}" >/dev/null
        "${tool}" save -o "$amd" "${imgs[@]}" >/dev/null
        log "ok    $amd ($(stat -f%z "$amd" 2>/dev/null || stat -c%s "$amd" 2>/dev/null) bytes)"
        "${tool}" rmi "${imgs[@]}" >/dev/null 2>&1 || true
    }
    [[ -f "$arm" && $(stat -f%z "$arm" 2>/dev/null || stat -c%s "$arm" 2>/dev/null || echo 0) -ge 100000 ]] && {
        log "skip  $(realpath --relative-to="$ROOT" "$arm" 2>/dev/null || echo "$arm")"
    } || {
        local tool; if have nerdctl; then tool=nerdctl; elif have docker; then tool=docker; else die "need nerdctl or docker for image fetch"; fi
        log "pull+save (${tool}) k8s v${v} (arm64)"
        "${tool}" pull --platform linux/arm64 "${imgs[@]}" >/dev/null
        "${tool}" save -o "$arm" "${imgs[@]}" >/dev/null
        log "ok    $arm ($(stat -f%z "$arm" 2>/dev/null || stat -c%s "$arm" 2>/dev/null) bytes)"
        "${tool}" rmi "${imgs[@]}" >/dev/null 2>&1 || true
    }
}

fetch_cilium_images() {
    local v="$CILIUM_VERSION"
    local dest="$VENDOR/images/cilium-v${v}.tar"
    [[ -f "$dest" && $(stat -f%z "$dest" 2>/dev/null || stat -c%s "$dest" 2>/dev/null || echo 0) -ge 100000 ]] && {
        log "skip  $(realpath --relative-to="$ROOT" "$dest" 2>/dev/null || echo "$dest")"
        return 0
    }
    local tool; if have nerdctl; then tool=nerdctl; elif have docker; then tool=docker; else die "need nerdctl or docker"; fi
    local imgs=(
        "quay.io/cilium/cilium:v${v}"
        "quay.io/cilium/operator-generic:v${v}"
        "quay.io/cilium/hubble-relay:v${v}"
        "quay.io/cilium/hubble-ui:v0.13.2"
        "quay.io/cilium/hubble-ui-backend:v0.13.2"
        "quay.io/cilium/certgen:v0.2.3"
    )
    log "pull+save (${tool}) cilium v${v}"
    "${tool}" pull "${imgs[@]}" >/dev/null
    "${tool}" save -o "$dest" "${imgs[@]}" >/dev/null
    log "ok    $dest ($(stat -f%z "$dest" 2>/dev/null || stat -c%s "$dest" 2>/dev/null) bytes)"
    "${tool}" rmi "${imgs[@]}" >/dev/null 2>&1 || true
}

fetch_prometheus_images() {
    # kube-prometheus-stack ships prometheus + alertmanager + node-exporter
    # + grafana + the operator itself as OCI images. We bake the entire
    # image set the chart references (collected from the chart's
    # values.yaml) into a single docker-archive so the bundle is
    # self-contained for any airgap prometheus install.
    local v="$PROMETHEUS_STACK_VERSION"
    local dest="$VENDOR/images/prometheus-stack-v${v}.tar"
    [[ -f "$dest" && $(stat -f%z "$dest" 2>/dev/null || stat -c%s "$dest" 2>/dev/null || echo 0) -ge 100000 ]] && {
        log "skip  $(realpath --relative-to="$ROOT" "$dest" 2>/dev/null || echo "$dest")"
        return 0
    }
    # Image list is pinned to the kube-prometheus-stack v75.6.1 set
    # (see https://github.com/prometheus-community/helm-charts/blob/kube-prometheus-stack-75.6.1/charts/kube-prometheus-stack/values.yaml).
    local imgs=(
        "quay.io/prometheus-operator/prometheus-operator:v0.81.0"
        "quay.io/prometheus/prometheus:v3.2.1"
        "quay.io/prometheus/alertmanager:v0.28.1"
        "quay.io/prometheus/node-exporter:v1.8.2"
        "registry.k8s.io/kube-state-metrics/kube-state-metrics:v2.13.0"
        "grafana/grafana:11.3.1"
    )
    local tool; if have nerdctl; then tool=nerdctl; elif have docker; then tool=docker; else die "need nerdctl or docker"; fi
    log "pull+save (${tool}) kube-prometheus-stack v${v}"
    "${tool}" pull "${imgs[@]}" >/dev/null
    "${tool}" save -o "$dest" "${imgs[@]}" >/dev/null
    log "ok    $dest ($(stat -f%z "$dest" 2>/dev/null || stat -c%s "$dest" 2>/dev/null) bytes)"
    "${tool}" rmi "${imgs[@]}" >/dev/null 2>&1 || true
}

fetch_cilium_chart() {
    local v="$CILIUM_VERSION"
    fetch "https://helm.cilium.io/cilium-${v}.tgz" \
          "$VENDOR/charts/cilium-${v}.tgz" 5000
}

fetch_prometheus_chart() {
    # kube-prometheus-stack chart lives in the prometheus-community
    # helm-charts repo; GitHub release tarball is the canonical
    # distribution channel. The .tgz sits at the repo root.
    local v="$PROMETHEUS_STACK_VERSION"
    fetch "https://github.com/prometheus-community/helm-charts/releases/download/kube-prometheus-stack-${v}/kube-prometheus-stack-${v}.tgz" \
          "$VENDOR/charts/kube-prometheus-stack-${v}.tgz" 5000
}

# ---- dispatch ----
print_paths() {
    echo "containerd:        $VENDOR/containerd/${CONTAINERD_VERSION}/{linux-amd64,linux-arm64}.tar.gz"
    echo "kubeadm:           $VENDOR/kubeadm/${KUBE_VERSION}/{linux-amd64,linux-arm64}.tar.gz"
    echo "kubelet:           $VENDOR/kubelet/${KUBE_VERSION}/{linux-amd64,linux-arm64}.tar.gz"
    echo "cri-dockerd:       $VENDOR/cri-dockerd/${CRI_DOCKERD_VERSION}/{linux-amd64,linux-arm64}.tar.gz"
    echo "docker (static):   $VENDOR/docker/static/{x86_64,aarch64}/docker-${DOCKER_VERSION}.tgz"
    echo "docker (.deb/.rpm):$VENDOR/docker/{deb,rpm}/<arch>/  (operator drops manually)"
    echo "registry:          $VENDOR/registry/v${REGISTRY_VERSION}/{linux-amd64,linux-arm64}.tar.gz"
    echo "k8s images:        $VENDOR/images/k8s-${KUBE_VERSION}-{amd64,arm64}.tar"
    echo "cilium images:     $VENDOR/images/cilium-v${CILIUM_VERSION}.tar"
    echo "prometheus images: $VENDOR/images/prometheus-stack-v${PROMETHEUS_STACK_VERSION}.tar"
    echo "cilium chart:      $VENDOR/charts/cilium-${CILIUM_VERSION}.tgz"
    echo "prometheus chart:  $VENDOR/charts/kube-prometheus-stack-${PROMETHEUS_STACK_VERSION}.tgz"
}

if [[ "${1:-}" == "--print-paths" ]]; then
    print_paths
    exit 0
fi

if [[ "${1:-}" == "--clean" ]]; then
    log "removing vendor/{containerd,kubeadm,kubelet,cri-dockerd,docker,registry,images,charts} contents"
    find "$VENDOR" -mindepth 1 -maxdepth 2 -not -name '.gitkeep' -exec rm -rf {} +
    log "done. re-run without --clean to repopulate."
    exit 0
fi

# Map of names -> functions (case-dispatched; bash 3.2 has no associative arrays)
fetch_one() {
    case "$1" in
        containerd)  fetch_containerd ;;
        kubeadm)     fetch_kubeadm ;;
        kubelet)     fetch_kubelet ;;
        cri-dockerd) fetch_cri_dockerd ;;
        docker)      fetch_docker ;;
        registry)    fetch_registry ;;
        k8s)         fetch_k8s_images ;;
        cilium)      fetch_cilium_images ;;
        prometheus)  fetch_prometheus_images ;;
        charts)      fetch_cilium_chart; fetch_prometheus_chart ;;
        all)
            fetch_containerd; fetch_kubeadm; fetch_kubelet
            fetch_cri_dockerd; fetch_docker; fetch_registry
            fetch_k8s_images; fetch_cilium_images; fetch_prometheus_images
            fetch_cilium_chart; fetch_prometheus_chart
            ;;
        *)
            log "unknown asset: $1 (known: containerd kubeadm kubelet cri-dockerd docker registry k8s cilium prometheus charts all)"
            exit 1
            ;;
    esac
}

if [[ $# -gt 0 ]]; then
    for name in "$@"; do
        fetch_one "$name"
    done
else
    fetch_one all
fi

log "all requested assets present under $VENDOR/"
