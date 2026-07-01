// Package image builds, inspects, and pushes the offline OCI bundle that
// `ko init --offline` consumes. The bundle is a single OCI image (one per
// arch, or one image index for multi-arch) with several layers — one layer
// per artifact type (containerd tarball, docker debs/rpms, helm charts,
// k8s images, etc.). Custom mediaType tags each layer so ko can recognise
// it on the way back out.
package image

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"sort"
)

// Media types ko uses for its bundle layers.
const (
	MediaTypeKoBundle        = "application/vnd.ko.bundle.v1+json"
	MediaTypeKoContainerdTar = "application/vnd.ko.layer.containerd.tar.gzip.v1"
	MediaTypeKoDockerDeb     = "application/vnd.ko.layer.docker.deb.v1"
	MediaTypeKoDockerRPM     = "application/vnd.ko.layer.docker.rpm.v1"
	MediaTypeKoHelmChart     = "application/vnd.ko.layer.helm.chart.tgz.v1"
	MediaTypeKoK8sImagesTar  = "application/vnd.ko.layer.k8s.images.tar.v1"

	MediaTypeOCIImageIndex   = "application/vnd.oci.image.index.v1+json"
	MediaTypeOCIManifest     = "application/vnd.oci.image.manifest.v1+json"
	MediaTypeOCIConfig       = "application/vnd.oci.image.config.v1+json"
	MediaTypeOCILayerGzip    = "application/vnd.oci.image.layer.v1.tar+gzip"
)

// LayerSource is a file (already on disk) plus its mediaType. Build turns it
// into a tar+gzip layer with an entry under /ko/<basename>.
type LayerSource struct {
	SrcPath   string
	MediaType string
	// Annotations are OCI annotations attached to the layer descriptor.
	Annotations map[string]string
}

// BuildOpts is the input to Build.
type BuildOpts struct {
	Arch      string // "amd64" | "arm64"
	Version   string // ko version (becomes tag)
	Layers    []LayerSource
	OutputDir string // where to write the .oci.tar.gz
}

// Build produces an OCI image layout tar.gz with the given layers. Returns
// the path to the .oci.tar.gz and a list of layer descriptors for the caller.
func Build(opts BuildOpts) (string, []LayerDescriptor, error) {
	if opts.Arch == "" {
		return "", nil, fmt.Errorf("arch is required")
	}
	if len(opts.Layers) == 0 {
		return "", nil, fmt.Errorf("at least one layer is required")
	}
	if err := os.MkdirAll(opts.OutputDir, 0o755); err != nil {
		return "", nil, fmt.Errorf("mkdir output: %w", err)
	}
	out := filepath.Join(opts.OutputDir, fmt.Sprintf("ko-%s-%s.oci.tar.gz", opts.Version, opts.Arch))

	// 1. Hash each layer source + build layer descriptors.
	layers := make([]LayerDescriptor, 0, len(opts.Layers))
	diffIDs := make([]string, 0, len(opts.Layers))
	for i, l := range opts.Layers {
		digest, diffID, size, err := hashFile(l.SrcPath)
		if err != nil {
			return "", nil, fmt.Errorf("hash %s: %w", l.SrcPath, err)
		}
		desc := LayerDescriptor{
			MediaType: l.MediaType,
			Digest:    digest,
			Size:      size,
			Annotations: map[string]string{
				"io.kubernetes.cri.image.layers": basename(l.SrcPath),
				"ko.build/source":                basename(l.SrcPath),
			},
		}
		maps.Copy(desc.Annotations, l.Annotations)
		layers = append(layers, desc)
		diffIDs = append(diffIDs, diffID)
		_ = i
	}

	// 2. Build config (empty image config — ko doesn't run the image).
	cfgJSON, cfgDigest, cfgSize, err := emptyConfig(opts.Arch, diffIDs)
	if err != nil {
		return "", nil, fmt.Errorf("config: %w", err)
	}

	// 3. Build manifest.
	manifest := Manifest{
		SchemaVersion: 2,
		MediaType:     MediaTypeOCIManifest,
		Config: Descriptor{
			MediaType: MediaTypeOCIConfig,
			Digest:    cfgDigest,
			Size:      cfgSize,
		},
		Layers: layers,
	}
	manifestJSON, manifestDigest, manifestSize, err := jsonMarshal(manifest)
	if err != nil {
		return "", nil, fmt.Errorf("manifest: %w", err)
	}

	// 4. Build image index (single manifest).
	index := ImageIndex{
		SchemaVersion: 2,
		MediaType:     MediaTypeOCIImageIndex,
		Manifests: []Descriptor{
			{
				MediaType: MediaTypeOCIManifest,
				Digest:    manifestDigest,
				Size:      manifestSize,
				Platform:  &Platform{Architecture: opts.Arch, OS: "linux"},
			},
		},
	}
	indexJSON, _, _, err := jsonMarshal(index)
	if err != nil {
		return "", nil, fmt.Errorf("index: %w", err)
	}

	// 5. Pack everything into a tar.gz using OCI image layout.
	if err := writeOCITar(out, indexJSON, manifestJSON, cfgJSON, opts.Layers); err != nil {
		return "", nil, fmt.Errorf("write tar: %w", err)
	}
	return out, layers, nil
}

func writeOCITar(out string, indexJSON, manifestJSON, cfgJSON []byte, layers []LayerSource) error {
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	must := func(name string, body []byte, mode int64) {
		hdr := &tar.Header{
			Name:     name,
			Mode:     mode,
			Size:     int64(len(body)),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			panic(err)
		}
		if _, err := tw.Write(body); err != nil {
			panic(err)
		}
	}

	must("oci-layout", []byte(`{"imageLayoutVersion":"1.0.0"}`), 0o644)
	must("index.json", indexJSON, 0o644)
	must("blobs/sha256/placeholder", []byte(""), 0o644) // ensures blobs/ dir exists
	must("blobs/sha256/"+trimDigest(digestOfBytes(manifestJSON)), manifestJSON, 0o644)
	must("blobs/sha256/"+trimDigest(digestOfBytes(cfgJSON)), cfgJSON, 0o644)

	// Layer blobs: re-use the file's existing digest if its name matches,
	// otherwise re-write under the digest name.
	sorted := append([]LayerSource(nil), layers...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].SrcPath < sorted[j].SrcPath })
	for _, l := range sorted {
		body, err := os.ReadFile(l.SrcPath)
		if err != nil {
			return err
		}
		digest, _, _, err := hashBytes(body)
		if err != nil {
			return err
		}
		must("blobs/sha256/"+trimDigest(digest), body, 0o644)
	}
	return nil
}

// LayerDescriptor mirrors the OCI descriptor with annotations.
type LayerDescriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// Descriptor is the OCI descriptor type.
type Descriptor struct {
	MediaType string            `json:"mediaType"`
	Digest    string            `json:"digest"`
	Size      int64             `json:"size"`
	Platform  *Platform         `json:"platform,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// Platform mirrors OCI platform info.
type Platform struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
}

// Manifest is an OCI image manifest.
type Manifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType"`
	Config        Descriptor        `json:"config"`
	Layers        []LayerDescriptor `json:"layers"`
}

// ImageIndex is an OCI image index (multi-arch).
type ImageIndex struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Manifests     []Descriptor `json:"manifests"`
}

// EmptyConfig is a minimal OCI image config JSON.
type EmptyConfig struct {
	Architecture string   `json:"architecture"`
	OS           string   `json:"os"`
	RootFS       RootFS   `json:"rootfs"`
}

type RootFS struct {
	Type    string   `json:"type"`
	DiffIDs []string `json:"diff_ids"`
}

func emptyConfig(arch string, diffIDs []string) ([]byte, string, int64, error) {
	c := EmptyConfig{
		Architecture: arch,
		OS:           "linux",
		RootFS:       RootFS{Type: "layers", DiffIDs: diffIDs},
	}
	b, err := json.Marshal(c)
	if err != nil {
		return nil, "", 0, err
	}
	digest, size, err := digestAndSize(b)
	if err != nil {
		return nil, "", 0, err
	}
	return b, digest, size, nil
}

func jsonMarshal(v any) ([]byte, string, int64, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, "", 0, err
	}
	digest, size, err := digestAndSize(b)
	if err != nil {
		return nil, "", 0, err
	}
	return b, digest, size, nil
}

// helper: write a JSON of any object via standard lib.
func init() {
	_ = io.Discard
}

func basename(p string) string { return filepath.Base(p) }