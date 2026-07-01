package image

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"maps"
	"os"
	"path/filepath"
)

// ArchImage is one arch's worth of layers inside a multi-arch bundle.
// Layers may differ across arches — for example amd64 containerd tarball
// vs arm64 containerd tarball. Layer blobs that happen to have identical
// contents across arches are deduplicated by digest.
type ArchImage struct {
	Arch   string
	Layers []LayerSource
}

// MultiBuildOpts is the input to BuildMulti.
type MultiBuildOpts struct {
	Version   string
	OutputDir string
	Images    []ArchImage
}

// MultiResult summarises what BuildMulti produced.
type MultiResult struct {
	OutputPath string
	Manifests  []Descriptor // image-index manifest descriptors, one per arch
}

// BuildMulti produces a single OCI image-layout tar.gz whose image index
// points at one manifest per arch. It is the multi-arch counterpart to
// Build — the per-arch case still uses Build directly.
//
// Returns the path to the .oci.tar.gz and the image-index manifest
// descriptors for the caller to log or further process.
func BuildMulti(opts MultiBuildOpts) (MultiResult, error) {
	if len(opts.Images) == 0 {
		return MultiResult{}, fmt.Errorf("at least one arch image is required")
	}
	if opts.OutputDir == "" {
		return MultiResult{}, fmt.Errorf("output dir is required")
	}
	if err := os.MkdirAll(opts.OutputDir, 0o755); err != nil {
		return MultiResult{}, fmt.Errorf("mkdir output: %w", err)
	}

	archs, blobCache, err := buildPerArch(opts.Images)
	if err != nil {
		return MultiResult{}, err
	}

	// Image index: one entry per arch manifest.
	idx := ImageIndex{
		SchemaVersion: 2,
		MediaType:     MediaTypeOCIImageIndex,
		Manifests:     make([]Descriptor, 0, len(archs)),
	}
	for _, a := range archs {
		idx.Manifests = append(idx.Manifests, a.desc)
	}
	idxJSON, _, _, err := jsonMarshal(idx)
	if err != nil {
		return MultiResult{}, fmt.Errorf("index: %w", err)
	}

	out := filepath.Join(opts.OutputDir, fmt.Sprintf("ko-%s-multi.oci.tar.gz", opts.Version))
	if err := writeMultiOCITar(out, idxJSON, archs, blobCache); err != nil {
		return MultiResult{}, fmt.Errorf("write tar: %w", err)
	}
	return MultiResult{OutputPath: out, Manifests: idx.Manifests}, nil
}

// archBuilt is the per-arch result of hashing layers and serializing
// manifest + config. Used by both BuildMulti and writeMultiOCITar.
type archBuilt struct {
	arch      string
	cfgJSON   []byte
	cfgDigest string
	mfJSON    []byte
	mfDigest  string
	mfSize    int64
	desc      Descriptor
}

// layerBlob is the cached payload for a unique layer digest.
type layerBlob struct {
	digest string
	size   int64
	body   []byte
}

func buildPerArch(images []ArchImage) ([]archBuilt, map[string]layerBlob, error) {
	archs := make([]archBuilt, 0, len(images))
	blobCache := map[string]layerBlob{}

	for _, ai := range images {
		if ai.Arch == "" {
			return nil, nil, fmt.Errorf("arch image with empty arch")
		}
		if len(ai.Layers) == 0 {
			return nil, nil, fmt.Errorf("arch %s: at least one layer is required", ai.Arch)
		}

		layers := make([]LayerDescriptor, 0, len(ai.Layers))
		diffIDs := make([]string, 0, len(ai.Layers))
		for _, l := range ai.Layers {
			body, err := os.ReadFile(l.SrcPath)
			if err != nil {
				return nil, nil, fmt.Errorf("arch %s: read %s: %w", ai.Arch, l.SrcPath, err)
			}
			digest, diffID, size, err := hashBytes(body)
			if err != nil {
				return nil, nil, fmt.Errorf("arch %s: hash: %w", ai.Arch, err)
			}
			if _, ok := blobCache[digest]; !ok {
				blobCache[digest] = layerBlob{digest: digest, size: size, body: body}
			}
			ann := map[string]string{
				"io.kubernetes.cri.image.layers": basename(l.SrcPath),
				"ko.build/source":                basename(l.SrcPath),
				"ko.build/arch":                  ai.Arch,
			}
			maps.Copy(ann, l.Annotations)
			layers = append(layers, LayerDescriptor{
				MediaType:   l.MediaType,
				Digest:      digest,
				Size:        size,
				Annotations: ann,
			})
			diffIDs = append(diffIDs, diffID)
		}

		cfgJSON, cfgDigest, cfgSize, err := emptyConfig(ai.Arch, diffIDs)
		if err != nil {
			return nil, nil, fmt.Errorf("arch %s: config: %w", ai.Arch, err)
		}

		mf := Manifest{
			SchemaVersion: 2,
			MediaType:     MediaTypeOCIManifest,
			Config: Descriptor{
				MediaType: MediaTypeOCIConfig,
				Digest:    cfgDigest,
				Size:      cfgSize,
			},
			Layers: layers,
		}
		mfJSON, mfDigest, mfSize, err := jsonMarshal(mf)
		if err != nil {
			return nil, nil, fmt.Errorf("arch %s: manifest: %w", ai.Arch, err)
		}

		archs = append(archs, archBuilt{
			arch:      ai.Arch,
			cfgJSON:   cfgJSON,
			cfgDigest: cfgDigest,
			mfJSON:    mfJSON,
			mfDigest:  mfDigest,
			mfSize:    mfSize,
			desc: Descriptor{
				MediaType: MediaTypeOCIManifest,
				Digest:    mfDigest,
				Size:      mfSize,
				Platform:  &Platform{Architecture: ai.Arch, OS: "linux"},
			},
		})
	}
	return archs, blobCache, nil
}

func writeMultiOCITar(out string, idxJSON []byte, archs []archBuilt, blobs map[string]layerBlob) error {
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
	must("index.json", idxJSON, 0o644)
	must("blobs/sha256/placeholder", []byte(""), 0o644)

	for _, a := range archs {
		must("blobs/sha256/"+trimDigest(a.cfgDigest), a.cfgJSON, 0o644)
		must("blobs/sha256/"+trimDigest(a.mfDigest), a.mfJSON, 0o644)
	}

	written := map[string]bool{}
	for _, b := range blobs {
		if written[b.digest] {
			continue
		}
		written[b.digest] = true
		must("blobs/sha256/"+trimDigest(b.digest), b.body, 0o644)
	}
	return nil
}