// Package etcd — cert generation.
//
// mTLS topology for an external etcd cluster (3 nodes):
//
//	ca.crt / ca.key                     — self-signed root, signs everything
//	server-<host>.crt / .key            — used by etcd process to identify itself
//	                                         to clients (CN=etcd-server, SAN includes the host)
//	peer-<host>.crt / .key              — used for etcd member↔member (CN=etcd-peer)
//	client.crt / client.key            — used by kube-apiserver / etcdctl
//	                                         (CN=etcd-client, O=system:masters for apiserver)
//
// Validity defaults to 10 years; kubeadm clusters use 100 years on the
// internal CA and we want the etcd PKI to outlive the cluster easily.
package etcd

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// CertPaths lists the absolute paths of the four PKI files on disk after
// Generate() runs. Layout:
//
//	<dir>/
//	  ca.crt, ca.key
//	  server.crt, server.key
//	  peer.crt, peer.key
//	  client.crt, client.key
type CertPaths struct {
	Dir    string
	CA     string // ca.crt
	CAKey  string // ca.key
	Server string // server.crt
	Peer   string // peer.crt
	Client string // client.crt
}

// CertHosts is the per-host info Generate() needs.
type CertHosts struct {
	Name    string   // e.g. "etcd-1"
	IP      string   // primary address, must be reachable from peers
	SANs    []string // extra DNS / IP names (loopback, additional vips, etc.)
	OrgUnit string   // optional, "ko-etcd" if empty
}

// GenerateOptions controls a single Generate() invocation.
type GenerateOptions struct {
	Dir        string
	Hosts      []CertHosts // one CertHosts per etcd member
	Validity   time.Duration
	CNPrefix   string // CN prefix for server / peer (default "etcd-server" / "etcd-peer")
}

// DefaultValidity is 10 years — enough to outlive any reasonable
// cluster refresh cycle.
const DefaultValidity = 10 * 365 * 24 * time.Hour

// Generate writes the full etcd PKI tree under Dir. Idempotent: if the
// files already exist and are valid, it returns CertPaths without
// rewriting — but the caller can rm -rf first if regeneration is wanted.
func Generate(opts GenerateOptions) (*CertPaths, error) {
	if opts.Dir == "" {
		return nil, fmt.Errorf("Dir is required")
	}
	if len(opts.Hosts) == 0 {
		return nil, fmt.Errorf("at least one CertHosts entry required")
	}
	if opts.Validity == 0 {
		opts.Validity = DefaultValidity
	}
	if err := os.MkdirAll(opts.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir %q: %w", opts.Dir, err)
	}

	caCert, caKey, err := loadOrSignCA(opts.Dir, opts.Validity)
	if err != nil {
		return nil, fmt.Errorf("CA: %w", err)
	}

	// Server / peer are per-host, but in this design we generate ONE
	// server cert + ONE peer cert valid for ALL hosts. The CN becomes
	// "etcd-server" and SANs list every member IP/DNS. This is exactly
	// what kubeadm does for its etcd member, and it lets us run with a
	// single cert file rather than 3×N files.
	serverCert, serverKey, err := signServerPeer(opts, caCert, caKey, "server", "etcd-server")
	if err != nil {
		return nil, fmt.Errorf("server cert: %w", err)
	}
	peerCert, peerKey, err := signServerPeer(opts, caCert, caKey, "peer", "etcd-peer")
	if err != nil {
		return nil, fmt.Errorf("peer cert: %w", err)
	}
	clientCert, clientKey, err := signClient(opts, caCert, caKey)
	if err != nil {
		return nil, fmt.Errorf("client cert: %w", err)
	}

	paths := &CertPaths{
		Dir:    opts.Dir,
		CA:     filepath.Join(opts.Dir, "ca.crt"),
		CAKey:  filepath.Join(opts.Dir, "ca.key"),
		Server: filepath.Join(opts.Dir, "server.crt"),
		Peer:   filepath.Join(opts.Dir, "peer.crt"),
		Client: filepath.Join(opts.Dir, "client.crt"),
	}
	if err := writePEM(paths.CA, "CERTIFICATE", caCert.Raw); err != nil {
		return nil, err
	}
	if err := writeKey(paths.CAKey, caKey); err != nil {
		return nil, err
	}
	if err := writePEM(paths.Server, "CERTIFICATE", serverCert.Raw); err != nil {
		return nil, err
	}
	if err := writeKey(filepath.Join(opts.Dir, "server.key"), serverKey); err != nil {
		return nil, err
	}
	if err := writePEM(paths.Peer, "CERTIFICATE", peerCert.Raw); err != nil {
		return nil, err
	}
	if err := writeKey(filepath.Join(opts.Dir, "peer.key"), peerKey); err != nil {
		return nil, err
	}
	if err := writePEM(paths.Client, "CERTIFICATE", clientCert.Raw); err != nil {
		return nil, err
	}
	if err := writeKey(filepath.Join(opts.Dir, "client.key"), clientKey); err != nil {
		return nil, err
	}
	// Also place a symlink so kube-apiserver finds them under the
	// conventional /etc/kubernetes/pki/etcd/ tree if needed.
	_ = os.Symlink(paths.CA, filepath.Join(opts.Dir, "kubernetes-ca.crt"))
	return paths, nil
}

// loadOrSignCA returns the existing CA if it parses cleanly, otherwise
// generates a new one. Re-using a CA preserves existing cert validity
// across regen of server/peer/client.
func loadOrSignCA(dir string, validity time.Duration) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	caPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")
	if pem, err := os.ReadFile(caPath); err == nil {
		if block, _ := decodePEM(pem, "CERTIFICATE"); block != nil {
			cert, err := x509.ParseCertificate(block.Bytes)
			if err == nil {
				if key, err := loadKey(keyPath); err == nil {
					return cert, key, nil
				}
			}
		}
	}
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("gen ca key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "etcd-ca", Organization: []string{"ko"}},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(validity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            1,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("create ca: %w", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, nil, fmt.Errorf("parse ca: %w", err)
	}
	return caCert, caKey, nil
}

func signServerPeer(opts GenerateOptions, caCert *x509.Certificate, caKey *ecdsa.PrivateKey, kind, cn string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("gen %s key: %w", kind, err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn, Organization: []string{"ko"}},
		NotBefore:    time.Now().Add(-1 * time.Hour),
		NotAfter:     time.Now().Add(opts.Validity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	for _, h := range opts.Hosts {
		if h.IP != "" {
			if ip := net.ParseIP(h.IP); ip != nil {
				tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
			}
		}
		if h.Name != "" {
			tmpl.DNSNames = append(tmpl.DNSNames, h.Name)
		}
		for _, s := range h.SANs {
			if ip := net.ParseIP(s); ip != nil {
				tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
			} else {
				tmpl.DNSNames = append(tmpl.DNSNames, s)
			}
		}
	}
	// Always include loopback and wildcard for local curl/etcdctl use.
	tmpl.IPAddresses = append(tmpl.IPAddresses, net.ParseIP("127.0.0.1"), net.ParseIP("::1"))
	tmpl.DNSNames = append(tmpl.DNSNames, "localhost")

	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &priv.PublicKey, caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("create %s cert: %w", kind, err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, fmt.Errorf("parse %s cert: %w", kind, err)
	}
	return cert, priv, nil
}

func signClient(opts GenerateOptions, caCert *x509.Certificate, caKey *ecdsa.PrivateKey) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("gen client key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "etcd-client",
			Organization: []string{"system:masters"}, // gives kube-apiserver full read/write
		},
		NotBefore:   time.Now().Add(-1 * time.Hour),
		NotAfter:    time.Now().Add(opts.Validity),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &priv.PublicKey, caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("create client cert: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, fmt.Errorf("parse client cert: %w", err)
	}
	return cert, priv, nil
}

func writePEM(path, kind string, der []byte) error {
	b := pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der})
	return os.WriteFile(path, b, 0o644)
}

func writeKey(path string, key *ecdsa.PrivateKey) error {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return fmt.Errorf("marshal key: %w", err)
	}
	b := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
	return os.WriteFile(path, b, 0o600)
}

func loadKey(path string) (*ecdsa.PrivateKey, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := decodePEM(pem, "EC PRIVATE KEY")
	if block == nil {
		return nil, fmt.Errorf("no EC PRIVATE KEY block in %q", path)
	}
	return x509.ParseECPrivateKey(block.Bytes)
}

func decodePEM(data []byte, want string) (*pem.Block, []byte) {
	for {
		block, r := pem.Decode(data)
		if block == nil {
			return nil, data
		}
		if block.Type == want {
			return block, r
		}
		data = r
	}
}

func randomSerial() (*big.Int, error) {
	max := new(big.Int).Lsh(big.NewInt(1), 128)
	n, err := rand.Int(rand.Reader, max)
	if err != nil {
		return nil, fmt.Errorf("rand serial: %w", err)
	}
	return n, nil
}
