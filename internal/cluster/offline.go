// Package cluster — offline.go carries a ko OCI bundle into an airgapped
// target cluster and starts an in-cluster image registry that kubeadm +
// cilium will pull from. This is what makes `ko init --offline` actually
// offline: no airgap host needs to reach out to the internet at any point
// after `ko pack build` has run on the operator's box.
//
// v0.0.5+: The in-cluster registry is no longer a container (nerdctl run).
// Instead, the ko bundle carries a static Go binary (distribution/distribution)
// which is extracted to /usr/local/bin/registry and run as a hardened systemd
// service with resource limits and security sandboxing. containerd is tuned
// for offline registry (max_concurrent_downloads=3, timeout=30s, etc.).
package cluster

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ko-build/ko/internal/containerd"
	"github.com/ko-build/ko/internal/image"
	"github.com/ko-build/ko/pkg/config"
)

const (
	// DefaultRegistryImage is what ko spins up in-cluster for the offline
	// image mirror. Pinned to upstream Docker distribution.
	DefaultRegistryImage = "registry:2"
	// DefaultRegistryPort is the port the in-cluster registry listens on.
	DefaultRegistryPort = 5000
	// DefaultLocalRegistryHost is the hostname every node uses to reach the
	// in-cluster registry. /etc/hosts on every node maps this to either
	// the master-1 IP (during early init) or the HA VIP (after kube-vip
	// takes over).
	DefaultLocalRegistryHost = "ko.local"
)

// OfflineRunner carries a ko bundle into an airgapped cluster and sets up
// the in-cluster registry. After Run() returns, the operator can call
// (*Init).Run() with KubeadmOptions.ImageRepository set to
// "<host>:<port>" — kubeadm will pull its images from the in-cluster
// registry, and cilium / join masters / join workers will too.
type OfflineRunner struct {
	Cfg    *config.File
	Exec   Executor
	Bundle string // local path to .oci.tar.gz on the operator's box
	Master1 string // first master — registry runs here

	// Overrides for tests + advanced ops.
	RegistryImage string // default "registry:2"
	RegistryPort  int    // default 5000
	LocalHost     string // default "ko.local"
}

type offlineLayout struct {
	bundleRoot    string // /var/lib/ko/bundle on master-1
	localRegistry string // ko.local:5000
	masters       []string
	workers       []string
}

// layerPaths holds the on-master-1 paths to each extracted bundle layer.
type layerPaths struct {
	Containerd     string
	Kubeadm        string
	K8sImages      string
	CiliumImages   string
	RegistryImage  string // optional (legacy: docker-archive, no longer used at runtime)
	RegistryBinary string // required: static registry Go binary from distribution/distribution
	CiliumChart    string
}

// Run orchestrates the full offline bring-up on master-1. The function is
// idempotent: re-running on a partially-set-up host retries each step.
//
// Order matters:
//  1. scp + extract bundle
//  2. identify layers by mediaType
//  3. install containerd + kubeadm
//  4. configure containerd (insecure-registries)
//  5. ctr image import k8s-images / registry-image / cilium-images
//  6. nerdctl run registry:2 (--net=host, /var/lib/ko-registry volume)
//  7. retag + push all images to ko.local:5000
//  8. /etc/hosts on every node -> ko.local = master-1 IP (and VIP later)
func (r *OfflineRunner) Run(ctx context.Context) error {
	o := r.layout()

	if r.Bundle == "" {
		return fmt.Errorf("OfflineRunner.Bundle is required")
	}
	if _, err := os.Stat(r.Bundle); err != nil {
		return fmt.Errorf("bundle %s: %w", r.Bundle, err)
	}

	// 1. scp + extract
	const bundleFile = "/tmp/ko-bundle.oci.tar.gz"
	if err := r.Exec.Scp(ctx, r.Master1, r.Bundle, bundleFile); err != nil {
		return fmt.Errorf("scp bundle to %s: %w", r.Master1, err)
	}
	if res := r.Exec.Run(ctx, r.Master1, fmt.Sprintf(
		"set -euo pipefail; mkdir -p %s; tar -xzf %s -C %s",
		o.bundleRoot, bundleFile, o.bundleRoot)); res.Failed() {
		return fmt.Errorf("extract bundle on %s: %w (stderr=%s)", r.Master1, res.Err, string(res.Stderr))
	}

	// 2. identify layers
	layers, err := r.identifyLayers(o.bundleRoot)
	if err != nil {
		return err
	}
	if layers.Containerd == "" {
		return fmt.Errorf("bundle missing containerd layer")
	}
	if layers.Kubeadm == "" {
		return fmt.Errorf("bundle missing kubeadm layer")
	}
	if layers.K8sImages == "" {
		return fmt.Errorf("bundle missing k8s images layer")
	}
	if layers.RegistryBinary == "" {
		return fmt.Errorf("bundle missing registry binary layer (re-bake bundle with v0.0.5+)")
	}

	// 3. install runtime from bundle
	if err := r.InstallRuntimeFromBundle(ctx, r.Master1, layers); err != nil {
		return err
	}

	// 4. configure containerd insecure-registries
	if err := r.ConfigureContainerd(ctx, r.Master1, o.localRegistry); err != nil {
		return err
	}

	// 5. import images into host containerd
	if err := r.importImages(ctx, r.Master1, layers); err != nil {
		return err
	}

	// 6. start in-cluster registry
	if err := r.startRegistry(ctx, r.Master1, o, layers); err != nil {
		return err
	}

	// 7. retag + push all images
	if err := r.pushImages(ctx, r.Master1, o); err != nil {
		return err
	}

	// 8. /etc/hosts on every node
	if err := r.writeHosts(ctx, o); err != nil {
		return err
	}

	// 9. kubelet drop-in. In an airgap, kubelet's default
	// --image-pull-progress-deadline=1m aborts large-image pulls (cilium
	// alone is ~200M); the drop-in extends it to 30m and adds production
	// eviction thresholds. Applied to every master+worker so `ko node
	// add` against an already-initialised cluster doesn't have to redo it.
	if err := writeKubeletDropInAll(ctx, r.Exec, append(append([]string{}, o.masters...), o.workers...)); err != nil {
		return err
	}

	return nil
}

// LocalRegistry returns the in-cluster registry address (e.g. "ko.local:5000")
// that callers should pass to kubeadm as --image-repository and to the
// cilium helm install as --set image.repository=ko.local:5000/...
func (r *OfflineRunner) LocalRegistry() string {
	host := r.LocalHost
	if host == "" {
		host = DefaultLocalRegistryHost
	}
	port := r.RegistryPort
	if port == 0 {
		port = DefaultRegistryPort
	}
	return fmt.Sprintf("%s:%d", host, port)
}

func (r *OfflineRunner) layout() offlineLayout {
	return offlineLayout{
		bundleRoot:    "/var/lib/ko/bundle",
		localRegistry: r.LocalRegistry(),
		masters:       r.Cfg.Nodes.Masters,
		workers:       r.Cfg.Nodes.Workers,
	}
}

// identifyLayers walks an already-extracted OCI bundle directory and
// returns the on-disk paths to each layer we care about. This is the
// on-host extract path used by OfflineRunner.Run — the bundle has been
// scp'd to master-1 and tar-extracted into bundleRoot, so the layout is
// index.json + blobs/sha256/<digest>.
func (r *OfflineRunner) identifyLayers(bundleRoot string) (*layerPaths, error) {
	indexPath := filepath.Join(bundleRoot, "index.json")
	raw, err := os.ReadFile(indexPath)
	if err != nil {
		return nil, fmt.Errorf("read index.json: %w", err)
	}
	var idx image.ImageIndex
	if err := json.Unmarshal(raw, &idx); err != nil {
		return nil, fmt.Errorf("parse index.json: %w", err)
	}
	paths := &layerPaths{}
	// Take the first manifest per mediaType — k8s-images / registry-image
	// are arch-neutral and shared across the multi-arch index, so the
	// first one we see is enough.
	for _, m := range idx.Manifests {
		manifestPath := filepath.Join(bundleRoot, "blobs/sha256", strings.TrimPrefix(m.Digest, "sha256:"))
		mraw, err := os.ReadFile(manifestPath)
		if err != nil {
			return nil, fmt.Errorf("read manifest %s: %w", m.Digest, err)
		}
		var mf image.Manifest
		if err := json.Unmarshal(mraw, &mf); err != nil {
			return nil, fmt.Errorf("parse manifest %s: %w", m.Digest, err)
		}
		for _, l := range mf.Layers {
			blobPath := filepath.Join(bundleRoot, "blobs/sha256", strings.TrimPrefix(l.Digest, "sha256:"))
			r.classifyLayer(l.MediaType, blobPath, paths)
		}
	}
	return paths, nil
}

// identifyLayersFromTar identifies layers in a bundle tar.gz without
// touching the host's filesystem. It extracts to a temp dir locally,
// reads index.json + manifests, and returns the local paths so the
// caller can scp them or refer to them by content. Used by
// NodeLifecycle.bootstrapHostOffline where the operator side has the
// bundle file but the new host hasn't been touched yet.
func (r *OfflineRunner) identifyLayersFromTar(bundleTarGZ string) (*layerPaths, error) {
	tmp, err := os.MkdirTemp("", "ko-bundle-*")
	if err != nil {
		return nil, fmt.Errorf("mktemp: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	if err := untarGz(bundleTarGZ, tmp); err != nil {
		return nil, fmt.Errorf("extract bundle %s: %w", bundleTarGZ, err)
	}
	return r.identifyLayers(tmp)
}

// classifyLayer routes a (mediaType, blobPath) pair into the matching
// layerPaths slot. Pulled out so both identifyLayers (from an extracted
// dir) and any future reader-based variants share the dispatch table.
func (r *OfflineRunner) classifyLayer(mediaType, blobPath string, paths *layerPaths) {
	switch mediaType {
	case image.MediaTypeKoContainerdTar:
		if paths.Containerd == "" {
			paths.Containerd = blobPath
		}
	case image.MediaTypeKoKubeadmBinary:
		if paths.Kubeadm == "" {
			paths.Kubeadm = blobPath
		}
	case image.MediaTypeKoK8sImagesTar:
		if paths.K8sImages == "" {
			paths.K8sImages = blobPath
		}
	case image.MediaTypeKoCiliumImagesTar:
		if paths.CiliumImages == "" {
			paths.CiliumImages = blobPath
		}
	case image.MediaTypeKoRegistryImage:
		if paths.RegistryImage == "" {
			paths.RegistryImage = blobPath
		}
	case image.MediaTypeKoRegistryBinary:
		if paths.RegistryBinary == "" {
			paths.RegistryBinary = blobPath
		}
	case image.MediaTypeKoHelmChart:
		if paths.CiliumChart == "" {
			paths.CiliumChart = blobPath
		}
	}
}

// untarGz is a small helper: extract a tar.gz to dest. Used by
// identifyLayersFromTar for operator-side bundle inspection. It
// creates parent directories on demand — tar entries from image.Build
// are flat ("blobs/sha256/<digest>") with no intermediate dir entries.
func untarGz(src, dest string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		target := filepath.Join(dest, h.Name)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		out, err := os.Create(target)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, tr); err != nil {
			_ = out.Close()
			return err
		}
		_ = out.Close()
	}
}

func (r *OfflineRunner) InstallRuntimeFromBundle(ctx context.Context, host string, l *layerPaths) error {
	// v0.0.5: install the runtime binaries + enable the service. The full
	// containerd config (mirrors + insecure + offline tuning) is written
	// separately by configureContainerd, which uses the Go template. We
	// don't write a config here — containerd starts with built-in defaults
	// for the brief moment until configureContainerd rewrites the config
	// and restarts the service.
	script := fmt.Sprintf(`set -euo pipefail
tar -xzf %[1]s -C /usr/local
# kubeadm tarball contains a single ./kubeadm entry — extract into a temp
# path then install -m 0755 so the mode is correct even if tar preserved
# the source's 0644.
mkdir -p /tmp/ko-kubeadm
tar -xzf %[2]s -C /tmp/ko-kubeadm
install -m 0755 /tmp/ko-kubeadm/kubeadm /usr/local/bin/kubeadm
rm -rf /tmp/ko-kubeadm

systemctl daemon-reload
systemctl enable --now containerd

# Dirs the kubelet + static-pod manifests will live in. Filled in by
# kubeadm init shortly after.
mkdir -p /var/lib/kubelet /etc/kubernetes/manifests
`, l.Containerd, l.Kubeadm)
	res := r.Exec.Run(ctx, host, script)
	if res.Failed() {
		return fmt.Errorf("install runtime on %s: %w (stderr=%s)", host, res.Err, string(res.Stderr))
	}
	return nil
}

func (r *OfflineRunner) ConfigureContainerd(ctx context.Context, host, localRegistry string) error {
	// v0.0.5: render the full containerd config from the Go template
	// (mirrors + insecure + offline-airgap tuning) and write it atomically.
	// The previous bash-sed approach was fragile — sed could fail silently,
	// could land in the wrong section of `containerd config default`'s
	// output, and the tuning was offline-only. The Go template applies the
	// same tuning to online and offline init.
	upstreams := []string{"quay.io", "registry.k8s.io", "docker.io", "ghcr.io"}
	configToml := containerd.OfflineConfig(upstreams, []string{localRegistry}, "http://"+localRegistry)
	script := fmt.Sprintf(`set -euo pipefail
mkdir -p /etc/containerd
cat > /etc/containerd/config.toml <<'KO_CONFIG_EOF'
%s
KO_CONFIG_EOF
systemctl restart containerd
`, configToml)
	res := r.Exec.Run(ctx, host, script)
	if res.Failed() {
		return fmt.Errorf("configure containerd on %s: %w (stderr=%s)", host, res.Err, string(res.Stderr))
	}
	return nil
}

func (r *OfflineRunner) importImages(ctx context.Context, host string, l *layerPaths) error {
	// Only k8s control-plane + cilium images are imported — the registry
	// itself is now a static binary under /usr/local/bin/registry, so we
	// don't need its docker-archive anymore. The RegistryImage layer is
	// kept in the bundle for backwards compatibility / inspection but
	// isn't imported or run here.
	script := fmt.Sprintf(`set -euo pipefail
ctr -n=k8s.io images import %[1]s
if [ -n "%[2]s" ]; then
  ctr -n=k8s.io images import %[2]s
fi
`, l.K8sImages, l.CiliumImages)
	res := r.Exec.Run(ctx, host, script)
	if res.Failed() {
		return fmt.Errorf("import images on %s: %w (stderr=%s)", host, res.Err, string(res.Stderr))
	}
	return nil
}

func (r *OfflineRunner) startRegistry(ctx context.Context, host string, _ offlineLayout, l *layerPaths) error {
	port := r.registryPort()

	// Extract the static registry binary from bundle layer
	script := fmt.Sprintf(`set -euo pipefail
# Extract registry binary to /usr/local/bin
tar -xzf %[1]s -C /usr/local/bin --no-overwrite-dir
chmod +x /usr/local/bin/registry

# Create registry data directory with proper permissions
mkdir -p /var/lib/ko-registry
chown -R 65534:65534 /var/lib/ko-registry 2>/dev/null || true

# Write registry config
cat > /etc/registry-config.yml <<'REG_EOF'
version: 0.1
log:
  level: info
  formatter: text
storage:
  filesystem:
    rootdirectory: /var/lib/ko-registry
  cache:
    blobdescriptor: inmemory
http:
  addr: :%[2]d
  secret: ko-registry-secret-random-string-change-in-production
  debug:
    addr: :5001
    prometheus:
      enabled: true
      path: /metrics
health:
  storagedriver:
    enabled: true
    interval: 10s
    threshold: 3
REG_EOF

# Install systemd unit file for hardened, production-ready registry
cat > /etc/systemd/system/ko-registry.service <<'UNIT_EOF'
[Unit]
Description=ko offline image registry
Documentation=https://docs.docker.com/registry/
After=network.target
Wants=network-online.target

[Service]
Type=simple
User=65534
Group=65534
ExecStart=/usr/local/bin/registry serve /etc/registry-config.yml
Restart=always
RestartSec=5
LimitNOFILE=65536
LimitNPROC=4096
# Resource limits for airgap registry (adjust based on node capacity)
MemoryLimit=2G
CPUQuota=200%%
# Security hardening
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/ko-registry
ReadOnlyPaths=/etc/registry-config.yml /usr/local/bin/registry
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
AmbientCapabilities=CAP_NET_BIND_SERVICE
# Sandboxing
PrivateDevices=true
ProtectControlGroups=true
ProtectKernelModules=true
ProtectKernelTunables=true
LockPersonality=true
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
RestrictNamespaces=true
RestrictRealtime=true
SystemCallFilter=@system-service
SystemCallFilter=~@privileged

[Install]
WantedBy=multi-user.target
UNIT_EOF

# Reload systemd and start registry
systemctl daemon-reload
systemctl enable --now ko-registry.service

# Wait for the registry to answer /v2/ (up to 30s).
for i in $(seq 1 30); do
  if curl -fsS http://127.0.0.1:%[2]d/v2/ >/dev/null 2>&1; then
    echo "registry up after ${i}s"
    exit 0
  fi
  sleep 1
done
echo "registry failed to come up within 30s" >&2
journalctl -u ko-registry.service --no-pager -n 50 >&2 || true
exit 1
`, l.RegistryBinary, port)
	res := r.Exec.Run(ctx, host, script)
	if res.Failed() {
		return fmt.Errorf("start registry on %s: %w (stderr=%s)", host, res.Err, string(res.Stderr))
	}
	return nil
}

func (r *OfflineRunner) pushImages(ctx context.Context, host string, o offlineLayout) error {
	// Build a single script that retags + pushes every image to the local
	// registry. We hardcode the k8s 1.32 / cilium 1.16 image lists here
	// because the offline cluster has no way to call `kubeadm config
	// images list` yet (kubeadm isn't on PATH for the operator).
	imgs := append([]string{}, image.K8sImagesForVersion(r.Cfg.Cluster.Version, "amd64")...)
	imgs = append(imgs, image.CiliumImagesForVersion(r.CiliumVersion())...)
	var b strings.Builder
	b.WriteString("set -euo pipefail\n")

	// Optimize image push with concurrency and retry
	b.WriteString(`
# Set up parallel push with retry
export CTR_MAX_CONCURRENT=5
export CTR_RETRY_COUNT=3
export CTR_RETRY_DELAY=2

push_with_retry() {
  local img=$1
  local target=$2
  local attempt=0
  while [ $attempt -lt $CTR_RETRY_COUNT ]; do
    if ctr -n=k8s.io images push --plain-http $target $target 2>/dev/null; then
      echo "✓ pushed $target"
      return 0
    fi
    attempt=$((attempt + 1))
    if [ $attempt -lt $CTR_RETRY_COUNT ]; then
      echo "retry $attempt/$CTR_RETRY_COUNT for $target"
      sleep $CTR_RETRY_DELAY
    fi
  done
  echo "✗ failed to push $target after $CTR_RETRY_COUNT attempts" >&2
  return 1
}

`)
	for _, img := range imgs {
		// registry.k8s.io/coredns/coredns -> ko.local:5000/coredns/coredns
		// quay.io/cilium/cilium -> ko.local:5000/cilium/cilium
		path := img
		if _, after, found := strings.Cut(img, "/"); found {
			path = after
		}
		target := o.localRegistry + "/" + path
		fmt.Fprintf(&b, "ctr -n=k8s.io images tag %s %s\n", img, target)
		fmt.Fprintf(&b, "push_with_retry %s %s &\n", img, target)
	}
	b.WriteString("\nwait\n")

	// v0.0.5: every image we just pushed is now also resident in the
	// in-cluster registry's blob store. The local ctr image cache is
	// pure duplication — k8s/cilium images can be 200M+, and master-1
	// runs at limited disk on airgap hardware. Drop the local copies so
	// the next `ko init --offline` (or operator introspection) doesn't
	// have to chew through a second copy of every image.
	//
	// What we DO NOT remove:
	//   - /var/lib/ko/bundle/* — the extracted layer directory is still
	//     needed if the operator wants to inspect mediaType / extract a
	//     single binary (e.g. to verify the registry binary version).
	//     `ko reset --purge` is the explicit knob for nuking it.
	//   - /usr/local/bin/registry — the static binary the registry
	//     service runs from.
	for _, img := range imgs {
		path := img
		if _, after, found := strings.Cut(img, "/"); found {
			path = after
		}
		target := o.localRegistry + "/" + path
		// Untag then remove — `ctr images rm` on a tag still pointing at
		// the same content is a no-op for the blob layer but keeps the
		// tag around. Untag first so the image is fully gone.
		fmt.Fprintf(&b, "ctr -n=k8s.io images unset %s 2>/dev/null || true\n", img)
		fmt.Fprintf(&b, "ctr -n=k8s.io images unset %s 2>/dev/null || true\n", target)
		fmt.Fprintf(&b, "ctr -n=k8s.io images rm %s 2>/dev/null || true\n", img)
		fmt.Fprintf(&b, "ctr -n=k8s.io images rm %s 2>/dev/null || true\n", target)
	}
	// The bundle tarball was scp'd to /tmp by OfflineRunner.Run step 1;
	// it served its purpose (host extract). Remove it now — re-extracting
	// would require keeping it under /tmp, which on airgap boxes is often
	// a tiny tmpfs.
	b.WriteString("rm -f /tmp/ko-bundle.oci.tar.gz\n")

	res := r.Exec.Run(ctx, host, b.String())
	if res.Failed() {
		return fmt.Errorf("push images on %s: %w (stderr=%s)", host, res.Err, string(res.Stderr))
	}
	return nil
}

// WriteHostsEntry appends `<master1IP> <localHost>` to /etc/hosts on a
// single host. It is idempotent (guarded by `grep -qF`) and is exported so
// NodeLifecycle.PrepareOfflineHost can use the same line in airgap `ko
// node add` flows, where the new node also needs `ko.local` resolving to
// the existing master-1 (or HA VIP).
func (r *OfflineRunner) WriteHostsEntry(ctx context.Context, host, master1IP string) error {
	line := fmt.Sprintf("%s %s\n", master1IP, r.LocalHostOrDefault())
	script := fmt.Sprintf(`set -euo pipefail
grep -qF '%[1]s' /etc/hosts || echo '%[1]s' >> /etc/hosts
`, line)
	if res := r.Exec.Run(ctx, host, script); res.Failed() {
		return fmt.Errorf("write /etc/hosts on %s: %w (stderr=%s)", host, res.Err, string(res.Stderr))
	}
	return nil
}

// PrepareHostFromBundle does steps 3-9 of OfflineRunner.Run (containerd +
// kubeadm install, containerd mirror config, /etc/hosts, kubelet drop-in)
// for a single host that joins an existing airgap cluster. The host gets
// `ko.local → master1IP` in /etc/hosts and the kubelet drop-in so it can
// pull from the in-cluster registry.
//
// `master1IP` is the IP every node should reach `ko.local` at — the first
// master's IP for non-HA, or the VIP for HA setups. It must already be
// resolvable from the new host (which is true once /etc/hosts is
// rewritten below).
//
// The function is intentionally idempotent — `ko node add` can be retried
// against the same host without leaving duplicate lines in /etc/hosts.
func (r *OfflineRunner) PrepareHostFromBundle(ctx context.Context, host, master1IP string, layers *layerPaths) error {
	o := r.layout()
	if err := r.InstallRuntimeFromBundle(ctx, host, layers); err != nil {
		return err
	}
	if err := r.ConfigureContainerd(ctx, host, o.localRegistry); err != nil {
		return err
	}
	if err := r.WriteHostsEntry(ctx, host, master1IP); err != nil {
		return err
	}
	if err := WriteKubeletDropIn(ctx, r.Exec, host); err != nil {
		return err
	}
	return nil
}

// writeHosts appends `<master-1-IP> ko.local` to /etc/hosts on every node.
// Once kube-vip binds the VIP, the operator can swap the master-1 line
// for `<VIP> ko.local`; for now the master-1 IP is what every node can
// reach via SSH.
func (r *OfflineRunner) writeHosts(ctx context.Context, o offlineLayout) error {
	master1IP, err := r.resolveMaster1IP(ctx)
	if err != nil {
		return err
	}
	hosts := append([]string{}, o.masters...)
	hosts = append(hosts, o.workers...)
	for _, h := range hosts {
		if err := r.WriteHostsEntry(ctx, h, master1IP); err != nil {
			return err
		}
	}
	return nil
}

func (r *OfflineRunner) registryPort() int {
	if r.RegistryPort != 0 {
		return r.RegistryPort
	}
	return DefaultRegistryPort
}

func (r *OfflineRunner) LocalHostOrDefault() string {
	if r.LocalHost != "" {
		return r.LocalHost
	}
	return DefaultLocalRegistryHost
}

// CiliumVersion returns the cilium chart version baked into the bundle
// (currently hardcoded to "1.16.1" in pack.go). Exposed so the offline
// runner can build matching image-lists without re-asking the operator.
func (r *OfflineRunner) CiliumVersion() string {
	return "1.16.1"
}
