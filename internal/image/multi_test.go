package image

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildMulti_BasicTwoArch(t *testing.T) {
	tmp := t.TempDir()

	ctdAmd64 := filepath.Join(tmp, "ctd-amd64.tar.gz")
	ctdArm64 := filepath.Join(tmp, "ctd-arm64.tar.gz")
	require.NoError(t, os.WriteFile(ctdAmd64, []byte("amd64 containerd payload"), 0o644))
	require.NoError(t, os.WriteFile(ctdArm64, []byte("arm64 containerd payload"), 0o644))

	res, err := BuildMulti(MultiBuildOpts{
		Version:   "v0.0.1",
		OutputDir: tmp,
		Images: []ArchImage{
			{Arch: "amd64", Layers: []LayerSource{{SrcPath: ctdAmd64, MediaType: MediaTypeKoContainerdTar}}},
			{Arch: "arm64", Layers: []LayerSource{{SrcPath: ctdArm64, MediaType: MediaTypeKoContainerdTar}}},
		},
	})
	require.NoError(t, err)
	assert.FileExists(t, res.OutputPath)
	require.Len(t, res.Manifests, 2)
	assert.Equal(t, "amd64", res.Manifests[0].Platform.Architecture)
	assert.Equal(t, "arm64", res.Manifests[1].Platform.Architecture)

	// Open tar.gz and inspect index.json — must point at two manifests.
	f, err := os.Open(res.OutputPath)
	require.NoError(t, err)
	defer f.Close()
	gz, err := gzip.NewReader(f)
	require.NoError(t, err)
	defer gz.Close()
	tr := tar.NewReader(gz)

	files := map[string][]byte{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if h.Typeflag != tar.TypeReg || h.Size == 0 {
			continue
		}
		body := make([]byte, h.Size)
		_, err = io.ReadFull(tr, body)
		require.NoError(t, err)
		files[h.Name] = body
	}

	idxRaw, ok := files["index.json"]
	require.True(t, ok, "index.json missing")
	var idx ImageIndex
	require.NoError(t, json.Unmarshal(idxRaw, &idx))
	assert.Equal(t, MediaTypeOCIImageIndex, idx.MediaType)
	require.Len(t, idx.Manifests, 2)
	for _, m := range idx.Manifests {
		require.NotNil(t, m.Platform)
		mfRaw, ok := files["blobs/sha256/"+trimDigest(m.Digest)]
		require.True(t, ok, "manifest blob missing for %s", m.Digest)
		var mf Manifest
		require.NoError(t, json.Unmarshal(mfRaw, &mf))
		require.Len(t, mf.Layers, 1)
		assert.Equal(t, MediaTypeKoContainerdTar, mf.Layers[0].MediaType)
		// Layer blob exists in the tar.
		_, ok = files["blobs/sha256/"+trimDigest(mf.Layers[0].Digest)]
		assert.True(t, ok, "layer blob missing")
	}
}

func TestBuildMulti_DedupsIdenticalBlobs(t *testing.T) {
	tmp := t.TempDir()

	// Same payload for both arches → should appear in tar once.
	shared := filepath.Join(tmp, "shared.tar.gz")
	require.NoError(t, os.WriteFile(shared, []byte("identical bytes"), 0o644))

	res, err := BuildMulti(MultiBuildOpts{
		Version:   "v0.0.1",
		OutputDir: tmp,
		Images: []ArchImage{
			{Arch: "amd64", Layers: []LayerSource{{SrcPath: shared, MediaType: MediaTypeKoHelmChart}}},
			{Arch: "arm64", Layers: []LayerSource{{SrcPath: shared, MediaType: MediaTypeKoHelmChart}}},
		},
	})
	require.NoError(t, err)
	assert.FileExists(t, res.OutputPath)

	// Count occurrences of the layer blob path in the tar.
	f, err := os.Open(res.OutputPath)
	require.NoError(t, err)
	defer f.Close()
	gz, err := gzip.NewReader(f)
	require.NoError(t, err)
	defer gz.Close()
	tr := tar.NewReader(gz)

	hits := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if h.Name != "blobs/sha256/placeholder" && len(h.Name) > len("blobs/sha256/") && h.Name[:len("blobs/sha256/")] == "blobs/sha256/" {
			hits++
		}
	}
	// 2 cfgs + 2 manifests + 1 dedup'd layer = 5 blobs (placeholder excluded).
	assert.Equal(t, 5, hits)
}

func TestBuildMulti_RejectsEmpty(t *testing.T) {
	tmp := t.TempDir()
	_, err := BuildMulti(MultiBuildOpts{Version: "v0.0.1", OutputDir: tmp})
	assert.Error(t, err)
}

func TestBuildMulti_RejectsEmptyLayers(t *testing.T) {
	tmp := t.TempDir()
	_, err := BuildMulti(MultiBuildOpts{
		Version:   "v0.0.1",
		OutputDir: tmp,
		Images:    []ArchImage{{Arch: "amd64"}},
	})
	assert.Error(t, err)
}