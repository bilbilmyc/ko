package image

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuild_Basic(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "fake-containerd.tar.gz")
	require.NoError(t, os.WriteFile(src, []byte("fake containerd payload"), 0o644))

	out, layers, err := Build(BuildOpts{
		Arch:      "amd64",
		Version:   "v0.0.1",
		Layers:    []LayerSource{{SrcPath: src, MediaType: MediaTypeKoContainerdTar}},
		OutputDir: tmp,
	})
	require.NoError(t, err)
	assert.FileExists(t, out)
	require.Len(t, layers, 1)
	assert.Equal(t, MediaTypeKoContainerdTar, layers[0].MediaType)
	assert.True(t, strings.HasPrefix(layers[0].Digest, "sha256:"))

	// Open the tar.gz and check expected entries.
	f, err := os.Open(out)
	require.NoError(t, err)
	defer f.Close()
	gz, err := gzip.NewReader(f)
	require.NoError(t, err)
	defer gz.Close()
	tr := tar.NewReader(gz)

	gotIndex := false
	gotLayout := false
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		switch h.Name {
		case "index.json":
			gotIndex = true
			var idx ImageIndex
			require.NoError(t, json.NewDecoder(tr).Decode(&idx))
			assert.Equal(t, MediaTypeOCIImageIndex, idx.MediaType)
			require.Len(t, idx.Manifests, 1)
			assert.Equal(t, MediaTypeOCIManifest, idx.Manifests[0].MediaType)
		case "oci-layout":
			gotLayout = true
		}
	}
	assert.True(t, gotIndex, "index.json missing")
	assert.True(t, gotLayout, "oci-layout missing")
}

func TestBuild_MultipleLayers(t *testing.T) {
	tmp := t.TempDir()
	ctd := filepath.Join(tmp, "ctd.tar.gz")
	docker := filepath.Join(tmp, "docker.deb")
	chart := filepath.Join(tmp, "cilium.tgz")
	require.NoError(t, os.WriteFile(ctd, []byte("ctd"), 0o644))
	require.NoError(t, os.WriteFile(docker, []byte("docker"), 0o644))
	require.NoError(t, os.WriteFile(chart, []byte("chart"), 0o644))

	_, layers, err := Build(BuildOpts{
		Arch:    "amd64",
		Version: "v0.0.1",
		Layers: []LayerSource{
			{SrcPath: ctd, MediaType: MediaTypeKoContainerdTar},
			{SrcPath: docker, MediaType: MediaTypeKoDockerDeb},
			{SrcPath: chart, MediaType: MediaTypeKoHelmChart},
		},
		OutputDir: tmp,
	})
	require.NoError(t, err)
	assert.Len(t, layers, 3)
}

func TestBuild_RejectsEmpty(t *testing.T) {
	tmp := t.TempDir()
	_, _, err := Build(BuildOpts{Arch: "amd64", Version: "v0.0.1", OutputDir: tmp})
	assert.Error(t, err)
}