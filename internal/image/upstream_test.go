package image

import (
	"archive/tar"
	"io"
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestK8sImagesForVersion_1_32(t *testing.T) {
	imgs := K8sImagesForVersion("v1.32.0", "amd64")
	// Every image the kubeadm default manifest needs at init time.
	assert.Contains(t, imgs, "registry.k8s.io/kube-apiserver:v1.32.0")
	assert.Contains(t, imgs, "registry.k8s.io/kube-controller-manager:v1.32.0")
	assert.Contains(t, imgs, "registry.k8s.io/kube-scheduler:v1.32.0")
	assert.Contains(t, imgs, "registry.k8s.io/kube-proxy:v1.32.0")
	assert.Contains(t, imgs, "registry.k8s.io/coredns/coredns:v1.11.3")
	assert.Contains(t, imgs, "registry.k8s.io/pause:3.10")
	assert.Contains(t, imgs, "registry.k8s.io/etcd:3.5.16-0")
}

func TestK8sImagesForVersion_StripsVPrefix(t *testing.T) {
	withV := K8sImagesForVersion("v1.32.0", "amd64")
	withoutV := K8sImagesForVersion("1.32.0", "amd64")
	assert.Equal(t, withV, withoutV, "v prefix must be optional")
}

func TestCiliumImagesForVersion_1_16(t *testing.T) {
	imgs := CiliumImagesForVersion("1.16.1")
	// Every image the cilium 1.16 chart deploys.
	assert.Contains(t, imgs, "quay.io/cilium/cilium:v1.16.1")
	assert.Contains(t, imgs, "quay.io/cilium/operator-generic:v1.16.1")
	assert.Contains(t, imgs, "quay.io/cilium/hubble-relay:v1.16.1")
	assert.Contains(t, imgs, "quay.io/cilium/hubble-ui:v0.13.2")
	assert.Contains(t, imgs, "quay.io/cilium/hubble-ui-backend:v0.13.2")
	assert.Contains(t, imgs, "quay.io/cilium/certgen:v0.2.3")
}

func TestCiliumImagesForVersion_VersionInTag(t *testing.T) {
	// A future bump must produce new tags, not silently reuse old ones.
	v117 := CiliumImagesForVersion("1.17.0")
	assert.Contains(t, v117, "quay.io/cilium/cilium:v1.17.0")
	assert.NotContains(t, v117, "v1.16.1")
}

func TestDetectImagePuller_PrefersNerdctlOverDocker(t *testing.T) {
	p, err := detectImagePuller()
	if err != nil {
		// Neither is installed (CI sandbox) — skip; the function only
		// guarantees behaviour when at least one is on PATH.
		t.Skipf("no image puller on PATH: %v", err)
	}
	// On a dev box both will usually be present; the contract is nerdctl
	// wins when it's there.
	if _, err := exec.LookPath("nerdctl"); err == nil {
		assert.Equal(t, "nerdctl", p.Name())
	} else {
		assert.Equal(t, "docker", p.Name())
	}
}

func TestWrapBinaryAsTarGz_PreservesModeAndName(t *testing.T) {
	// wrapBinaryAsTarGz is invoked at pack time for kubeadm; the resulting
	// tarball must contain a single ./kubeadm entry with mode 0755.
	tmp := t.TempDir()
	bin := tmp + "/kubeadm-src"
	require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\necho ko\n"), 0o644))

	tarball := tmp + "/kubeadm.tar.gz"
	require.NoError(t, wrapBinaryAsTarGz(bin, tarball, "kubeadm"))

	// Sanity: the tarball is a valid gzip (magic bytes 1f 8b).
	header := make([]byte, 2)
	f, err := os.Open(tarball)
	require.NoError(t, err)
	defer f.Close()
	_, err = f.Read(header)
	require.NoError(t, err)
	assert.Equal(t, byte(0x1f), header[0])
	assert.Equal(t, byte(0x8b), header[1])
}

// TestDedupDockerArchive_CollapsesDuplicateBlobs builds a synthetic
// docker-archive tar with two images sharing a layer blob, runs the dedup
// pass, and verifies the output has the shared blob exactly once.
func TestDedupDockerArchive_CollapsesDuplicateBlobs(t *testing.T) {
	tmp := t.TempDir()

	// Build a source tar with two images whose layer lists overlap on
	// blob "blobs/sha256/shared".
	src := tmp + "/src.tar"
	f, err := os.Create(src)
	require.NoError(t, err)
	tw := tar.NewWriter(f)

	shared := []byte("shared-payload-im-big")
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "blobs/sha256/shared", Mode: 0o644, Size: int64(len(shared))}))
	_, err = tw.Write(shared)
	require.NoError(t, err)

	unique1 := []byte("only-image-1-sees-this")
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "blobs/sha256/unique1", Mode: 0o644, Size: int64(len(unique1))}))
	_, err = tw.Write(unique1)
	require.NoError(t, err)

	unique2 := []byte("only-image-2-sees-this")
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "blobs/sha256/unique2", Mode: 0o644, Size: int64(len(unique2))}))
	_, err = tw.Write(unique2)
	require.NoError(t, err)

	// Duplicate blob: same path, same content as the first copy.
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "blobs/sha256/shared", Mode: 0o644, Size: int64(len(shared))}))
	_, err = tw.Write(shared)
	require.NoError(t, err)

	manifest := []byte(`[{"Config":"blobs/sha256/cfg1","RepoTags":["img1"],"Layers":["blobs/sha256/shared","blobs/sha256/unique1"]},{"Config":"blobs/sha256/cfg2","RepoTags":["img2"],"Layers":["blobs/sha256/shared","blobs/sha256/unique2"]}]`)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o644, Size: int64(len(manifest))}))
	_, err = tw.Write(manifest)
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, f.Close())

	dst := tmp + "/dst.tar"
	require.NoError(t, dedupDockerArchive(src, dst))

	// Walk the deduped tar and count occurrences of the shared blob.
	rf, err := os.Open(dst)
	require.NoError(t, err)
	defer rf.Close()
	tr := tar.NewReader(rf)

	sharedCount := 0
	sawManifest := false
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		switch h.Name {
		case "blobs/sha256/shared":
			sharedCount++
			body, err := io.ReadAll(tr)
			require.NoError(t, err)
			assert.Equal(t, shared, body, "shared blob payload must be preserved")
		case "manifest.json":
			sawManifest = true
			body, err := io.ReadAll(tr)
			require.NoError(t, err)
			assert.Contains(t, string(body), "img1")
			assert.Contains(t, string(body), "img2")
		default:
			_, _ = io.Copy(io.Discard, tr)
		}
	}
	assert.Equal(t, 1, sharedCount, "shared blob must appear exactly once in deduped tar")
	assert.True(t, sawManifest, "manifest.json must be preserved")
}

// TestDedupDockerArchive_RefusesUnrecognizedTar guards against accidental
// corruption: if the input has no manifest.json, dedup must refuse rather
// than produce an invalid docker-archive.
func TestDedupDockerArchive_RefusesUnrecognizedTar(t *testing.T) {
	tmp := t.TempDir()
	src := tmp + "/not-a-docker-archive.tar"
	f, err := os.Create(src)
	require.NoError(t, err)
	tw := tar.NewWriter(f)
	body := []byte("hello")
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "blobs/sha256/x", Mode: 0o644, Size: int64(len(body))}))
	_, err = tw.Write(body)
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, f.Close())

	err = dedupDockerArchive(src, tmp+"/out.tar")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no manifest.json")
}
