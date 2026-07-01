package cluster

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTrimV(t *testing.T) {
	assert.Equal(t, "1.35.0", trimV("v1.35.0"))
	assert.Equal(t, "1.35.0", trimV("1.35.0"))
	assert.Equal(t, "", trimV("v"))
}

func TestQuoteIfNeeded(t *testing.T) {
	assert.Equal(t, "simple", quoteIfNeeded("simple"))
	assert.Equal(t, `"with space"`, quoteIfNeeded("with space"))
	assert.Equal(t, `"a\"b"`, quoteIfNeeded(`a"b`))
}

func TestJoinShellCmd(t *testing.T) {
	got := joinShellCmd([]string{"kubeadm", "init", "--kubernetes-version=v1.35.0"})
	assert.Equal(t, "kubeadm init --kubernetes-version=v1.35.0", got)
}

func TestGenerate100YearCA(t *testing.T) {
	cert, key, err := Generate100YearCA("ko-test")
	require.NoError(t, err)
	assert.NotEmpty(t, cert)
	assert.NotEmpty(t, key)
	assert.FileExists(t, cert)
	assert.FileExists(t, key)

	// parse the cert and verify 100-year NotAfter
	data, err := readFileOS(cert)
	require.NoError(t, err)
	block := pemDecode(data)
	require.NotNil(t, block)
	c, err := x509Parse(block.Bytes)
	require.NoError(t, err)
	dur := c.NotAfter.Sub(c.NotBefore)
	// 100 * 365 * 24h, with some clock-skew tolerance on the negative side
	assert.InDelta(t, float64(100*365*24), dur.Hours(), 24.0)
}

func TestKubeadm_Init_BuildsArgs(t *testing.T) {
	mock := NewMockExecutor()
	defer mock.Close()
	k := NewKubeadm(mock)
	_, err := k.Init(context.Background(), "h", KubeadmOptions{
		KubernetesVersion: "v1.35.0",
		PodCIDR:           "10.244.0.0/16",
		ServiceCIDR:       "10.96.0.0/12",
		APIServerEndpoint: "10.0.0.100:6443",
		ImageRepository:   "registry.example.com/ko",
		CertKey:           "abc123",
	})
	require.NoError(t, err)
	require.NotEmpty(t, mock.Calls)
	cmd := mock.Calls[0].Command
	assert.Contains(t, cmd, "kubeadm init")
	assert.Contains(t, cmd, "--kubernetes-version=1.35.0")
	assert.Contains(t, cmd, "--pod-network-cidr=10.244.0.0/16")
	assert.Contains(t, cmd, "--control-plane-endpoint=10.0.0.100:6443")
	assert.Contains(t, cmd, "--image-repository=registry.example.com/ko")
	assert.Contains(t, cmd, "--skip-phases=addon/kube-proxy")
	assert.Contains(t, cmd, "--certificate-key=abc123")
}

func TestKubeadm_Join_ControlPlane(t *testing.T) {
	mock := NewMockExecutor()
	defer mock.Close()
	k := NewKubeadm(mock)
	_, err := k.JoinControlPlane(context.Background(), "h", KubeadmOptions{
		Token:                "abcdef.1234567890abcdef",
		DiscoveryTokenCAHash: "sha256:deadbeef",
		APIServerEndpoint:    "10.0.0.100:6443",
		CertKey:              "certkey",
	})
	require.NoError(t, err)
	require.NotEmpty(t, mock.Calls)
	cmd := mock.Calls[0].Command
	assert.Contains(t, cmd, "kubeadm join")
	assert.Contains(t, cmd, "--token=abcdef.1234567890abcdef")
	assert.Contains(t, cmd, "10.0.0.100:6443")
	assert.Contains(t, cmd, "--control-plane")
	assert.Contains(t, cmd, "--certificate-key=certkey")
}

func TestKubeadm_Init_CertificateValidity(t *testing.T) {
	mock := NewMockExecutor()
	defer mock.Close()
	k := NewKubeadm(mock)
	_, err := k.Init(context.Background(), "h", KubeadmOptions{
		KubernetesVersion:   "v1.35.0",
		PodCIDR:             "10.244.0.0/16",
		ServiceCIDR:         "10.96.0.0/12",
		CertificateValidity: "876000h",
	})
	require.NoError(t, err)
	cmd := mock.Calls[0].Command
	assert.Contains(t, cmd, "--certificate-validity=876000h")
}

func TestKubeadm_Join_ControlPlane_RequiresEndpoint(t *testing.T) {
	mock := NewMockExecutor()
	defer mock.Close()
	k := NewKubeadm(mock)
	_, err := k.Join(context.Background(), "h", KubeadmOptions{
		ControlPlane: true,
		Token:        "t", DiscoveryTokenCAHash: "h", CertKey: "c",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "APIServerEndpoint")
}

func TestKubeadm_Join_Worker(t *testing.T) {
	mock := NewMockExecutor()
	defer mock.Close()
	k := NewKubeadm(mock)
	_, err := k.Join(context.Background(), "h", KubeadmOptions{
		Token:                "abcdef.1234567890abcdef",
		DiscoveryTokenCAHash: "sha256:deadbeef",
	})
	require.NoError(t, err)
	cmd := mock.Calls[0].Command
	assert.NotContains(t, cmd, "--control-plane")
}

func TestKubeadm_Reset_BuildsArgs(t *testing.T) {
	mock := NewMockExecutor()
	defer mock.Close()
	k := NewKubeadm(mock)
	_, err := k.Reset(context.Background(), "h", "unix:///run/containerd/containerd.sock")
	require.NoError(t, err)
	cmd := mock.Calls[0].Command
	assert.Contains(t, cmd, "kubeadm reset")
	assert.Contains(t, cmd, "--cri-socket=unix:///run/containerd/containerd.sock")
}

// sanity: keep lint happy about the unused import
var _ = strings.ToLower
var _ = time.Second
