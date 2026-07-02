package etcd

import (
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerate_WritesAllFiles(t *testing.T) {
	dir := t.TempDir()
	paths, err := Generate(GenerateOptions{
		Dir: dir,
		Hosts: []CertHosts{
			{Name: "etcd-1", IP: "10.0.0.31"},
			{Name: "etcd-2", IP: "10.0.0.32"},
			{Name: "etcd-3", IP: "10.0.0.33"},
		},
	})
	require.NoError(t, err)

	for _, p := range []string{paths.CA, paths.CAKey, paths.Server, paths.Peer, paths.Client} {
		_, err := os.Stat(p)
		assert.NoError(t, err, "expected %s to exist", p)
	}
	for _, p := range []string{
		filepath.Join(paths.Dir, "server.key"),
		filepath.Join(paths.Dir, "peer.key"),
		filepath.Join(paths.Dir, "client.key"),
	} {
		_, err := os.Stat(p)
		assert.NoError(t, err, "expected key file %s to exist", p)
	}
}

func TestGenerate_CertChainIsValid(t *testing.T) {
	dir := t.TempDir()
	paths, err := Generate(GenerateOptions{
		Dir:   dir,
		Hosts: []CertHosts{{Name: "etcd-1", IP: "10.0.0.31"}},
	})
	require.NoError(t, err)

	caPool := x509.NewCertPool()
	caCert := mustParseCertFile(t, paths.CA)
	caPool.AddCert(caCert)

	verify := func(path, cn string, usages []x509.ExtKeyUsage) {
		cert := mustParseCertFile(t, path)
		assert.Equal(t, cn, cert.Subject.CommonName)
		assert.True(t, cert.NotAfter.After(time.Now().Add(5*365*24*time.Hour)),
			"cert should be valid for >5y (default 10y)")
		chains, err := cert.Verify(x509.VerifyOptions{
			Roots:     caPool,
			KeyUsages: usages,
		})
		require.NoError(t, err, "cert %s should verify against CA", path)
		require.NotEmpty(t, chains)
	}
	verify(paths.Server, "etcd-server", []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth})
	verify(paths.Peer, "etcd-peer", []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth})
	verify(paths.Client, "etcd-client", []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth})
}

func TestGenerate_ServerCertCoversAllHosts(t *testing.T) {
	dir := t.TempDir()
	paths, err := Generate(GenerateOptions{
		Dir: dir,
		Hosts: []CertHosts{
			{Name: "etcd-1", IP: "10.0.0.31", SANs: []string{"etcd-1.internal"}},
			{Name: "etcd-2", IP: "10.0.0.32"},
			{Name: "etcd-3", IP: "10.0.0.33"},
		},
	})
	require.NoError(t, err)
	cert := mustParseCertFile(t, paths.Server)

	// Every member IP must be in the SAN. The cert stores IPs in
	// 16-byte form (IPv4-mapped IPv6); compare via To4() to handle both.
	wantIPs := []net.IP{
		net.IPv4(10, 0, 0, 31).To4(),
		net.IPv4(10, 0, 0, 32).To4(),
		net.IPv4(10, 0, 0, 33).To4(),
		net.IPv4(127, 0, 0, 1).To4(),
		net.ParseIP("::1"),
	}
	for _, ip := range wantIPs {
		assert.Contains(t, cert.IPAddresses, ip, "missing SAN IP %s", ip)
	}
	// DNS names from explicit SANs + loopback defaults
	wantNames := []string{"etcd-1", "etcd-2", "etcd-3", "etcd-1.internal", "localhost"}
	for _, n := range wantNames {
		assert.Contains(t, cert.DNSNames, n, "missing SAN DNS %s", n)
	}
}

func TestGenerate_ClientCertHasSystemMastersOrg(t *testing.T) {
	dir := t.TempDir()
	paths, err := Generate(GenerateOptions{
		Dir:   dir,
		Hosts: []CertHosts{{Name: "etcd-1", IP: "10.0.0.31"}},
	})
	require.NoError(t, err)
	cert := mustParseCertFile(t, paths.Client)
	assert.Equal(t, "etcd-client", cert.Subject.CommonName)
	assert.Contains(t, cert.Subject.Organization, "system:masters",
		"client cert must be in system:masters so kube-apiserver gets full access")
}

func TestGenerate_Idempotent_CAReused(t *testing.T) {
	dir := t.TempDir()
	opts := GenerateOptions{
		Dir:   dir,
		Hosts: []CertHosts{{Name: "etcd-1", IP: "10.0.0.31"}},
	}
	first, err := Generate(opts)
	require.NoError(t, err)
	caBytes1, _ := os.ReadFile(first.CA)

	second, err := Generate(opts)
	require.NoError(t, err)
	caBytes2, _ := os.ReadFile(second.CA)

	assert.Equal(t, caBytes1, caBytes2, "CA bytes should be byte-identical on re-Generate")
}

func TestGenerate_KeyFilePermsAre0600(t *testing.T) {
	dir := t.TempDir()
	paths, err := Generate(GenerateOptions{
		Dir:   dir,
		Hosts: []CertHosts{{Name: "etcd-1", IP: "10.0.0.31"}},
	})
	require.NoError(t, err)
	for _, p := range []string{paths.CAKey,
		filepath.Join(paths.Dir, "server.key"),
		filepath.Join(paths.Dir, "peer.key"),
		filepath.Join(paths.Dir, "client.key")} {
		fi, err := os.Stat(p)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm(), "%s must be 0600", p)
	}
}

func mustParseCertFile(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	block, _ := pem.Decode(b)
	require.NotNil(t, block, "PEM decode failed for %s", path)
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	return cert
}
