package image

import (
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
