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
)

// Default k8s / cilium versions baked into the bundle. These are the
// source of truth for both `gatherLayers` (which pulls images at these
// versions) and the default bundle name (`defaultBundleName`).
const (
	defaultK8sVersion    = "v1.32.0"
	defaultCiliumVersion = "1.16.1"
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
		version   string
	)
	cmd := &cobra.Command{
		Use:   "build",
		Short: "Build an offline OCI bundle (single-arch or multi-arch)",
		Long: `build fetches vendor artifacts (containerd tarball, helm charts, k8s
images) and packs them into a sealos-style OCI image-layout tar.gz.

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
			if version == "" {
				version = defaultBundleName(defaultK8sVersion, defaultCiliumVersion, time.Now())
			}
			cacheDir := cacheHome()

			if arch == "all" {
				return buildMultiArch(cmd, cacheDir, outputDir, version)
			}
			return buildSingleArch(cmd, cacheDir, arch, outputDir, version)
		},
	}
	cmd.Flags().StringVar(&arch, "arch", "amd64", "target architecture: amd64 | arm64 | all")
	cmd.Flags().StringVar(&outputDir, "output", "", "output directory (default ~/.ko/bundles)")
	cmd.Flags().StringVar(&version, "version", "",
		"bundle version tag (default: bundle-k8s<>-cilium<>-<YYYYMMDD>)")
	return cmd
}

func buildSingleArch(cmd *cobra.Command, cacheDir, arch, outputDir, version string) error {
	dl := image.NewUpstream(cacheDir)
	layers, err := gatherLayers(cmd, dl, cacheDir, arch)
	if err != nil {
		return err
	}
	if len(layers) == 0 {
		return fmt.Errorf("no layers produced — check connectivity and try again")
	}
	out, descs, err := image.Build(image.BuildOpts{
		Arch: arch, Version: version, Layers: layers, OutputDir: outputDir,
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

func buildMultiArch(cmd *cobra.Command, cacheDir, outputDir, version string) error {
	dl := image.NewUpstream(cacheDir)
	images := make([]image.ArchImage, 0, 2)
	for _, arch := range []string{"amd64", "arm64"} {
		layers, err := gatherLayers(cmd, dl, cacheDir, arch)
		if err != nil {
			return err
		}
		if len(layers) == 0 {
			return fmt.Errorf("arch %s: no layers produced", arch)
		}
		images = append(images, image.ArchImage{Arch: arch, Layers: layers})
	}
	res, err := image.BuildMulti(image.MultiBuildOpts{
		Version: version, OutputDir: outputDir, Images: images,
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

// gatherLayers fetches containerd, kubeadm, k8s images, registry image and
// the cilium helm chart for a single arch. Errors fetching a single source
// are downgraded to warnings so a flaky network doesn't block the whole
// pack; the caller validates len(layers) > 0. Defaults are pinned to the
// versions ko's other components expect (defaultK8sVersion, defaultCiliumVersion).
//
// containerd and docker versions are NOT pinned — we ask the GitHub API
// for the latest non-prerelease tag at pack time, so airgap installs track
// upstream stable automatically. The latest-tag lookup is cached under
// ~/.ko/cache for 24h so repeat builds don't hammer the GitHub API.
// Operator can still pin via HCL or --containerd-version / --docker-version.
func gatherLayers(cmd *cobra.Command, dl *image.UpstreamDownloader, cacheDir, arch string) ([]image.LayerSource, error) {
	layers := []image.LayerSource{}

	ctdVersion, err := dl.LatestContainerdVersion(cmd.Context())
	if err != nil {
		logger.Warn("containerd latest-tag lookup failed; falling back to v2.1.0", "err", err)
		ctdVersion = "v2.1.0"
	}
	logger.Info("containerd version for bundle", "version", ctdVersion)
	ctd, err := dl.Containerd(cmd.Context(), ctdVersion, arch)
	if err != nil {
		logger.Warn("containerd download failed (skipping)", "arch", arch, "version", ctdVersion, "err", err)
	} else {
		layers = append(layers, image.LayerSource{
			SrcPath: ctd, MediaType: image.MediaTypeKoContainerdTar,
		})
	}

	kub, err := dl.Kubeadm(cmd.Context(), defaultK8sVersion, arch)
	if err != nil {
		logger.Warn("kubeadm download failed (skipping)", "arch", arch, "err", err)
	} else {
		layers = append(layers, image.LayerSource{
			SrcPath: kub, MediaType: image.MediaTypeKoKubeadmBinary,
		})
	}

	k8sTar, err := dl.K8sImagesTar(cmd.Context(), defaultK8sVersion, arch)
	if err != nil {
		logger.Warn("k8s images pack failed (skipping)", "arch", arch, "err", err)
	} else {
		layers = append(layers, image.LayerSource{
			SrcPath: k8sTar, MediaType: image.MediaTypeKoK8sImagesTar,
		})
	}

	reg, err := dl.RegistryImage(cmd.Context(), arch)
	if err != nil {
		logger.Warn("registry image pack failed (skipping)", "arch", arch, "err", err)
	} else {
		layers = append(layers, image.LayerSource{
			SrcPath: reg, MediaType: image.MediaTypeKoRegistryImage,
		})
	}

	regBin, err := dl.RegistryBinary(cmd.Context(), image.DefaultRegistryVersion, arch)
	if err != nil {
		logger.Warn("registry binary pack failed (skipping)", "arch", arch, "err", err)
	} else {
		layers = append(layers, image.LayerSource{
			SrcPath: regBin, MediaType: image.MediaTypeKoRegistryBinary,
		})
	}

	chart, err := dl.CiliumChartTGZ(cmd.Context(), defaultCiliumVersion)
	if err != nil {
		logger.Warn("cilium chart download failed (skipping)", "arch", arch, "err", err)
	} else {
		layers = append(layers, image.LayerSource{
			SrcPath: chart, MediaType: image.MediaTypeKoHelmChart,
		})
	}

	ciliumImgs, err := dl.CiliumImagesTar(cmd.Context(), defaultCiliumVersion)
	if err != nil {
		logger.Warn("cilium images pack failed (skipping)", "arch", arch, "err", err)
	} else {
		layers = append(layers, image.LayerSource{
			SrcPath: ciliumImgs, MediaType: image.MediaTypeKoCiliumImagesTar,
		})
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