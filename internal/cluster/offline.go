// Package cluster — offline.go carries a ko OCI bundle into an airgapped
// target cluster and starts an in-cluster image registry that kubeadm +
// cilium will pull from. This is what makes `ko init --offline` actually
// offline: no airgap host needs to reach out to the internet at any point
// after `ko pack build` has run on the operator's box.
package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
	Containerd    string
	Kubeadm       string
	K8sImages     string
	CiliumImages  string
	RegistryImage string
	CiliumChart   string
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
	if layers.RegistryImage == "" {
		return fmt.Errorf("bundle missing registry image layer")
	}

	// 3. install runtime from bundle
	if err := r.installRuntimeFromBundle(ctx, r.Master1, layers); err != nil {
		return err
	}

	// 4. configure containerd insecure-registries
	if err := r.configureContainerd(ctx, r.Master1, o.localRegistry); err != nil {
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
			switch l.MediaType {
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
			case image.MediaTypeKoHelmChart:
				if paths.CiliumChart == "" {
					paths.CiliumChart = blobPath
				}
			}
		}
	}
	return paths, nil
}

func (r *OfflineRunner) installRuntimeFromBundle(ctx context.Context, host string, l *layerPaths) error {
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
`, l.Containerd, l.Kubeadm)
	res := r.Exec.Run(ctx, host, script)
	if res.Failed() {
		return fmt.Errorf("install runtime on %s: %w (stderr=%s)", host, res.Err, string(res.Stderr))
	}
	return nil
}

func (r *OfflineRunner) configureContainerd(ctx context.Context, host, localRegistry string) error {
	// Each upstream registry that any of the bundled images come from
	// needs a containerd mirror entry pointing at ko.local:<port>, so the
	// kubelet (and cilium pods) automatically pull from the in-cluster
	// mirror without per-image config. The mirror preserves the path —
	// quay.io/cilium/cilium -> http://ko.local:5000/cilium/cilium — so
	// the repository names in the local registry just have to match the
	// path-after-host, which PushImages below guarantees.
	mirrors := []string{
		"quay.io",
		"registry.k8s.io",
		"docker.io",
		"ghcr.io",
	}
	var b strings.Builder
	b.WriteString("set -euo pipefail\n")
	b.WriteString("mkdir -p /etc/containerd\n")
	b.WriteString("if [ ! -f /etc/containerd/config.toml ]; then\n")
	b.WriteString("  containerd config default > /etc/containerd/config.toml\n")
	b.WriteString("fi\n")
	for _, m := range mirrors {
		fmt.Fprintf(&b, "if ! grep -q 'registry.mirrors.\"%[1]s\"' /etc/containerd/config.toml; then\n", m)
		fmt.Fprintf(&b, "  cat >> /etc/containerd/config.toml <<'EOF'\n\n[plugins.\"io.containerd.grpc.v1.cri\".registry.mirrors.\"%[1]s\"]\n  endpoint = [\"http://%[2]s\"]\nEOF\n", m, localRegistry)
		b.WriteString("fi\n")
	}
	// Mark ko.local:5000 itself as insecure (no TLS, in-cluster only).
	fmt.Fprintf(&b, "if ! grep -q 'configs.\"%s\".tls' /etc/containerd/config.toml; then\n", localRegistry)
	fmt.Fprintf(&b, "  cat >> /etc/containerd/config.toml <<'EOF'\n\n[plugins.\"io.containerd.grpc.v1.cri\".registry.configs.\"%[1]s\".tls]\n  insecure_skip_verify = true\nEOF\n", localRegistry)
	b.WriteString("fi\n")
	b.WriteString("systemctl restart containerd\n")
	res := r.Exec.Run(ctx, host, b.String())
	if res.Failed() {
		return fmt.Errorf("configure containerd on %s: %w (stderr=%s)", host, res.Err, string(res.Stderr))
	}
	return nil
}

func (r *OfflineRunner) importImages(ctx context.Context, host string, l *layerPaths) error {
	script := fmt.Sprintf(`set -euo pipefail
ctr -n=k8s.io images import %[1]s
ctr -n=k8s.io images import %[2]s
if [ -n "%[3]s" ]; then
  ctr -n=k8s.io images import %[3]s
fi
`, l.K8sImages, l.RegistryImage, l.CiliumImages)
	res := r.Exec.Run(ctx, host, script)
	if res.Failed() {
		return fmt.Errorf("import images on %s: %w (stderr=%s)", host, res.Err, string(res.Stderr))
	}
	return nil
}

func (r *OfflineRunner) startRegistry(ctx context.Context, host string, o offlineLayout, l *layerPaths) error {
	img := r.RegistryImage
	if img == "" {
		img = DefaultRegistryImage
	}
	// Retag the imported registry image to the local-registry address
	// before running it, so the kubelet / cilium pods (if any reference
	// the registry namespace) can resolve it. The retag is local-only.
	retag := fmt.Sprintf(`set -euo pipefail
ctr -n=k8s.io images tag docker.io/library/registry:2 %[1]s/library/registry:2 || true
ctr -n=k8s.io images tag docker.io/library/registry:2 %[1]s/registry:2 || true
`, o.localRegistry)
	if res := r.Exec.Run(ctx, host, retag); res.Failed() {
		return fmt.Errorf("retag registry image: %w (stderr=%s)", res.Err, string(res.Stderr))
	}

	script := fmt.Sprintf(`set -euo pipefail
mkdir -p /var/lib/ko-registry
# --net=host so the registry listens on the host's :5000 directly.
# --restart=always so a host reboot brings it back.
# Idempotency: delete the old container if it exists, then re-create.
nerdctl rm -f ko-registry 2>/dev/null || true
nerdctl run -d --name ko-registry --net=host --restart=always \
  -v /var/lib/ko-registry:/var/lib/registry \
  %s
# Wait for the registry to answer /v2/ (up to 30s).
for i in $(seq 1 30); do
  if curl -fsS http://127.0.0.1:%[2]d/v2/ >/dev/null 2>&1; then
    echo "registry up after ${i}s"
    exit 0
  fi
  sleep 1
done
echo "registry failed to come up within 30s" >&2
nerdctl logs ko-registry >&2 || true
exit 1
`, img, r.registryPort())
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
	for _, img := range imgs {
		// registry.k8s.io/coredns/coredns -> ko.local:5000/coredns/coredns
		// quay.io/cilium/cilium -> ko.local:5000/cilium/cilium
		path := img
		if i := strings.Index(img, "/"); i >= 0 {
			path = img[i+1:]
		}
		target := o.localRegistry + "/" + path
		fmt.Fprintf(&b, "ctr -n=k8s.io images tag %s %s\n", img, target)
		fmt.Fprintf(&b, "ctr -n=k8s.io images push --plain-http %s %s\n", o.localRegistry, target)
	}
	res := r.Exec.Run(ctx, host, b.String())
	if res.Failed() {
		return fmt.Errorf("push images on %s: %w (stderr=%s)", host, res.Err, string(res.Stderr))
	}
	return nil
}

// writeHosts appends `<master-1-IP> ko.local` to /etc/hosts on every node.
// Once kube-vip binds the VIP, the operator can swap the master-1 line
// for `<VIP> ko.local`; for now the master-1 IP is what every node can
// reach via SSH.
func (r *OfflineRunner) writeHosts(ctx context.Context, o offlineLayout) error {
	// Resolve master-1's IP from a node that can see it — pick master-1
	// itself. `getent hosts <hostname>` is glibc-only; use `hostname -I`
	// or a simple `ip -4 -o addr show`.
	res := r.Exec.Run(ctx, r.Master1, "ip -4 -o addr show scope global | awk '{print $4}' | cut -d/ -f1")
	if res.Failed() {
		return fmt.Errorf("resolve master-1 IP: %w (stderr=%s)", res.Err, string(res.Stderr))
	}
	master1IP := strings.TrimSpace(string(res.Stdout))
	if master1IP == "" {
		return fmt.Errorf("no global IPv4 on master-1 %s", r.Master1)
	}
	// Take the first IP — multi-NIC hosts are out of scope for v0.0.1.
	if i := strings.IndexByte(master1IP, '\n'); i >= 0 {
		master1IP = master1IP[:i]
	}

	hosts := append([]string{}, o.masters...)
	hosts = append(hosts, o.workers...)
	line := fmt.Sprintf("%s %s\n", master1IP, r.LocalHostOrDefault())
	for _, h := range hosts {
		script := fmt.Sprintf(`set -euo pipefail
grep -qF '%[1]s' /etc/hosts || echo '%[1]s' >> /etc/hosts
`, line)
		if r := r.Exec.Run(ctx, h, script); r.Failed() {
			return fmt.Errorf("write /etc/hosts on %s: %w (stderr=%s)", h, r.Err, string(r.Stderr))
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
