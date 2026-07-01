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

	"github.com/spf13/cobra"

	"github.com/ko-build/ko/internal/image"
	"github.com/ko-build/ko/internal/logger"
)

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
		Short: "Build an offline OCI bundle for the given arch",
		RunE: func(cmd *cobra.Command, args []string) error {
			if arch == "" {
				arch = "amd64"
			}
			if outputDir == "" {
				outputDir = filepath.Join(homeDir(), ".ko", "bundles")
			}
			if version == "" {
				version = "v0.0.1"
			}
			cacheDir := filepath.Join(homeDir(), ".ko", "cache")
			dl := image.NewUpstream(cacheDir)

			layers := []image.LayerSource{}
			ctd, err := dl.Containerd(cmd.Context(), "v2.0.5", arch)
			if err != nil {
				logger.Warn("containerd download failed (skipping)", "err", err)
			} else {
				layers = append(layers, image.LayerSource{
					SrcPath: ctd, MediaType: image.MediaTypeKoContainerdTar,
				})
			}
			chartDir := filepath.Join(cacheDir, "helm")
			if charts, err := image.HelmPullDefault(cmd.Context(), "", "1.16.1", chartDir); err != nil {
				logger.Warn("helm pull failed (skipping)", "err", err)
			} else {
				for name, p := range charts {
					_ = name
					layers = append(layers, image.LayerSource{
						SrcPath: p, MediaType: image.MediaTypeKoHelmChart,
					})
				}
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
		},
	}
	cmd.Flags().StringVar(&arch, "arch", "amd64", "target architecture: amd64 | arm64")
	cmd.Flags().StringVar(&outputDir, "output", "", "output directory (default ~/.ko/bundles)")
	cmd.Flags().StringVar(&version, "version", "v0.0.1", "ko version tag")
	return cmd
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
		// Stable order for readability.
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