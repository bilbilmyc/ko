package cli

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ko-build/ko/internal/image"
	"github.com/ko-build/ko/internal/logger"
	"github.com/ko-build/ko/internal/version"
)

// defaultBundleName returns the default `--version` value used when
// the operator doesn't pass one. Format:
//
//	bundle-k8s<MAJOR.MINOR.PATCH>-cilium<MAJOR.MINOR.PATCH>-YYYYMMDD
//
// Examples:
//
//	bundle-k8s1.32.0-cilium1.16.1-20260702  (used with --arch amd64 → ...-amd64.oci.tar.gz)
//	bundle-k8s1.32.0-cilium1.16.1-20260702  (used with --arch all   → ...-multi.oci.tar.gz)
//
// The k8s / cilium version strings may carry a leading `v` (kubeadm
// convention); strip it for the filename so `v1.32.0` and `1.32.0` both
// render as `k8s1.32.0`.
func defaultBundleName(k8sVer, ciliumVer string, t time.Time) string {
	strip := func(s string) string { return strings.TrimPrefix(s, "v") }
	return fmt.Sprintf("bundle-k8s%s-cilium%s-%s",
		strip(k8sVer), strip(ciliumVer),
		t.UTC().Format("20060102"))
}

func newPackCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pack",
		Short: "Build / push / inspect offline bundles",
	}
	cmd.AddCommand(newPackBuildCmd())
	cmd.AddCommand(newPackInspectCmd())
	return cmd
}

func newPackBuildCmd() *cobra.Command {
	var (
		arch      string
		outputDir string
		ver       string
		vendorDir string
	)
	cmd := &cobra.Command{
		Use:   "build",
		Short: "Build an offline OCI bundle (single-arch or multi-arch)",
		Long: `build reads pre-vendored assets from third_party/ (binaries, image
archives, helm charts) and packs them into a sealos-style OCI image-layout
tar.gz. ZERO network at build time — every binary and image was placed in
third_party/ by ` + "`ko vendor fetch`" + `.

  --arch amd64    Single-arch bundle (default)
  --arch arm64    Single-arch bundle
  --arch all      Multi-arch image index (amd64 + arm64 in one tar.gz)

Multi-arch dedups identical layer blobs across arches. The output filename
embeds the arch (single-arch) or ends in -multi.oci.tar.gz (multi-arch).

The bundle is versioned independently of ko itself. Default --version is

  bundle-k8s<X.Y.Z>-cilium<X.Y.Z>-<YYYYMMDD>

(format: defaultBundleName). Override via --version to bake an explicit
name; the value becomes the filename prefix verbatim.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if arch == "" {
				arch = "amd64"
			}
			if outputDir == "" {
				outputDir = filepath.Join(homeDir(), ".ko", "bundles")
			}
			if ver == "" {
				ver = defaultBundleName(version.KubeVersion, version.CiliumVersion, time.Now())
			}
			if vendorDir == "" {
				vendorDir = vendorHome()
			}

			if arch == "all" {
				return buildMultiArch(cmd, vendorDir, outputDir, ver)
			}
			return buildSingleArch(cmd, vendorDir, arch, outputDir, ver)
		},
	}
	cmd.Flags().StringVar(&arch, "arch", "amd64", "target architecture: amd64 | arm64 | all")
	cmd.Flags().StringVar(&outputDir, "output", "", "output directory (default ~/.ko/bundles)")
	cmd.Flags().StringVar(&ver, "version", "",
		"bundle version tag (default: bundle-k8s<>-cilium<>-<YYYYMMDD>)")
	cmd.Flags().StringVar(&vendorDir, "vendor-dir", "",
		"path to third_party/ (default: project root / third_party); populated by `ko vendor fetch`")
	return cmd
}

func buildSingleArch(cmd *cobra.Command, vendorDir, arch, outputDir, ver string) error {
	dl := image.NewUpstream(vendorDir)
	layers, err := gatherLayers(cmd, dl, arch)
	if err != nil {
		return err
	}
	if len(layers) == 0 {
		return fmt.Errorf("no layers produced — run `ko vendor fetch` to populate third_party/")
	}
	out, descs, err := image.Build(image.BuildOpts{
		Arch: arch, Version: ver, Layers: layers, OutputDir: outputDir,
	})
	if err != nil {
		return err
	}
	cmd.Printf("✓ bundle: %s\n", out)
	for _, d := range descs {
		cmd.Printf("  layer: %s (%d bytes)\n", d.Digest, d.Size)
	}
	return nil
}

func buildMultiArch(cmd *cobra.Command, vendorDir, outputDir, ver string) error {
	dl := image.NewUpstream(vendorDir)
	images := make([]image.ArchImage, 0, 2)
	for _, arch := range []string{"amd64", "arm64"} {
		layers, err := gatherLayers(cmd, dl, arch)
		if err != nil {
			return err
		}
		if len(layers) == 0 {
			return fmt.Errorf("arch %s: no layers produced", arch)
		}
		images = append(images, image.ArchImage{Arch: arch, Layers: layers})
	}
	res, err := image.BuildMulti(image.MultiBuildOpts{
		Version: ver, OutputDir: outputDir, Images: images,
	})
	if err != nil {
		return err
	}
	cmd.Printf("✓ multi-arch bundle: %s\n", res.OutputPath)
	for _, m := range res.Manifests {
		cmd.Printf("  manifest: %s  %s/%s  (%d bytes)\n",
			m.Digest, m.Platform.OS, m.Platform.Architecture, m.Size)
	}
	return nil
}

// gatherLayers reads pre-vendored assets from third_party/ via dl (no
// network) and turns each into a LayerSource for the bundle. Errors
// fetching a single source are downgraded to warnings so a missing
// optional layer (e.g. docker .deb dropped manually) doesn't block the
// whole pack; the caller validates len(layers) > 0.
//
// All asset versions are pinned via internal/version (mirrored in
// vendor-versions.env for the fetch script).
func gatherLayers(cmd *cobra.Command, dl *image.UpstreamDownloader, arch string) ([]image.LayerSource, error) {
	layers := []image.LayerSource{}
	add := func(srcPath, mediaType, label string) {
		if srcPath == "" {
			return
		}
		logger.Info("layer", "kind", label, "arch", arch, "src", srcPath)
		layers = append(layers, image.LayerSource{SrcPath: srcPath, MediaType: mediaType})
	}
	mustOK := func(err error) bool {
		if err != nil {
			logger.Warn("vendored asset missing (skipping)", "err", err)
			return false
		}
		return true
	}

	// containerd (binary tarball)
	if p, err := dl.Containerd(version.ContainerdVersion, arch); mustOK(err) {
		add(p, image.MediaTypeKoContainerdTar, "containerd")
	}
	// kubeadm
	if p, err := dl.Kubeadm(version.KubeVersion, arch); mustOK(err) {
		add(p, image.MediaTypeKoKubeadmBinary, "kubeadm")
	}
	// kubelet
	if p, err := dl.Kubelet(version.KubeVersion, arch); mustOK(err) {
		add(p, image.MediaTypeKoKubeletBinary, "kubelet")
	}
	// cri-dockerd
	if p, err := dl.CRIDockerd(version.CRIDockerdVersion, arch); mustOK(err) {
		add(p, image.MediaTypeKoCRIDockerdBinary, "cri-dockerd")
	}
	// docker static tgz (always; needed for manual `dockerd` install on
	// distros without working apt/dnf in the airgap env)
	if p, err := dl.DockerStatic(version.DockerVersion, arch); mustOK(err) {
		add(p, "application/vnd.ko.layer.docker.static.tgz.v1", "docker-static")
	}
	// docker .deb / .rpm (optional — only present if operator dropped
	// the package into third_party/docker/{deb,rpm}/<arch>/). Not
	// required for the airgap path (static tgz + manual install works);
	// emit the layer if present, skip silently otherwise.
	if p, err := dl.DockerDeb(arch); err == nil {
		add(p, image.MediaTypeKoDockerDeb, "docker-deb")
	}
	if p, err := dl.DockerRPM(arch); err == nil {
		add(p, image.MediaTypeKoDockerRPM, "docker-rpm")
	}
	// k8s control plane image tar
	if p, err := dl.K8sImagesTar(version.KubeVersion, arch); mustOK(err) {
		add(p, image.MediaTypeKoK8sImagesTar, "k8s-images")
	}
	// cilium images + chart
	if p, err := dl.CiliumImagesTar(version.CiliumVersion); mustOK(err) {
		add(p, image.MediaTypeKoCiliumImagesTar, "cilium-images")
	}
	if p, err := dl.CiliumChartTGZ(version.CiliumVersion); mustOK(err) {
		add(p, image.MediaTypeKoHelmChart, "cilium-chart")
	}
	// prometheus stack images + chart
	if p, err := dl.PrometheusImagesTar(version.PrometheusStackVersion); mustOK(err) {
		add(p, image.MediaTypeKoPrometheusImagesTar, "prometheus-images")
	}
	if p, err := dl.PrometheusChartTGZ(version.PrometheusStackVersion); mustOK(err) {
		add(p, image.MediaTypeKoPrometheusChart, "prometheus-chart")
	}
	// registry (static Go binary, runs as ko-registry.service on master-1)
	if p, err := dl.RegistryBinary(version.RegistryVersion, arch); mustOK(err) {
		add(p, image.MediaTypeKoRegistryBinary, "registry-binary")
	}
	return layers, nil
}

func newPackInspectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "inspect <bundle.tar.gz>",
		Short: "Print the contents of a ko bundle (index, manifests, layers)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return inspectBundle(cmd.OutOrStdout(), args[0])
		},
	}
	return cmd
}

func inspectBundle(w io.Writer, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	files := map[string][]byte{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if h.Typeflag != tar.TypeReg || h.Size == 0 {
			continue
		}
		body := make([]byte, h.Size)
		if _, err := io.ReadFull(tr, body); err != nil {
			return err
		}
		files[h.Name] = body
	}
	idxRaw, ok := files["index.json"]
	if !ok {
		return fmt.Errorf("not a ko bundle: index.json missing")
	}
	var idx image.ImageIndex
	if err := json.Unmarshal(idxRaw, &idx); err != nil {
		return fmt.Errorf("index.json: %w", err)
	}
	fmt.Fprintln(w, "image index:")
	fmt.Fprintf(w, "  schemaVersion: %d\n", idx.SchemaVersion)
	fmt.Fprintf(w, "  mediaType:     %s\n", idx.MediaType)
	fmt.Fprintln(w, "  manifests:")
	for _, m := range idx.Manifests {
		fmt.Fprintf(w, "    - %s  %s  (%d bytes)\n", m.MediaType, m.Digest, m.Size)
		if m.Platform != nil {
			fmt.Fprintf(w, "      platform: %s/%s\n", m.Platform.OS, m.Platform.Architecture)
		}
		manifestBody, ok := files["blobs/sha256/"+trimDigest(m.Digest)]
		if !ok {
			continue
		}
		var mf image.Manifest
		if err := json.Unmarshal(manifestBody, &mf); err != nil {
			continue
		}
		fmt.Fprintln(w, "      layers:")
		ls := append([]image.LayerDescriptor(nil), mf.Layers...)
		sort.Slice(ls, func(i, j int) bool { return ls[i].MediaType < ls[j].MediaType })
		for _, l := range ls {
			fmt.Fprintf(w, "        - %s  %s  (%d bytes)\n", l.MediaType, l.Digest, l.Size)
		}
	}
	return nil
}

func trimDigest(d string) string {
	const p = "sha256:"
	if len(d) > len(p) && d[:len(p)] == p {
		return d[len(p):]
	}
	return d
}

// cacheHome is the per-source download cache. KO_CACHE_HOME overrides the
// default ~/.ko/cache, useful for test sandboxes that don't want to write
// into the user's real cache directory.
func cacheHome() string {
	if h := os.Getenv("KO_CACHE_HOME"); h != "" {
		return h
	}
	return filepath.Join(homeDir(), ".ko", "cache")
}

// vendorHome returns the default third_party/ root used by `ko pack
// build` and `ko vendor fetch`. KO_VENDOR_DIR overrides it for CI /
// sandbox runs. Default is the project's third_party/ (resolved from
// the current working directory) — fetch-vendor.sh populates it.
func vendorHome() string {
	if h := os.Getenv("KO_VENDOR_DIR"); h != "" {
		return h
	}
	return filepath.Join("third_party")
}