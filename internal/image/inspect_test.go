package image

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInspectBundle(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "fake.tar.gz")
	require.NoError(t, os.WriteFile(src, []byte("data"), 0o644))
	out, _, err := Build(BuildOpts{
		Arch: "amd64", Version: "v0.0.1",
		Layers:    []LayerSource{{SrcPath: src, MediaType: MediaTypeKoContainerdTar}},
		OutputDir: tmp,
	})
	require.NoError(t, err)

	// Inspect through the same code path that `ko pack inspect` uses.
	var buf bytes.Buffer
	err = inspectBundleImpl(&buf, out)
	require.NoError(t, err)
	got := buf.String()
	assert.Contains(t, got, "image index")
	assert.Contains(t, got, "manifests:")
	assert.Contains(t, got, "platform: linux/amd64")
	assert.Contains(t, got, MediaTypeKoContainerdTar)
}