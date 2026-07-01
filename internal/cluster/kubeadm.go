package cluster

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"

	"github.com/ko-build/ko/internal/logger"
	"github.com/ko-build/ko/pkg/config"
)

// KubeadmOptions configures a kubeadm init/join.
type KubeadmOptions struct {
	KubernetesVersion string // e.g. "1.35.0"
	ControlPlane      bool   // true for master
	APIServerEndpoint string // e.g. "10.0.0.100:6443" (for HA)
	Token             string
	DiscoveryTokenCAHash string
	CertKey           string // for upload-certs on HA masters
	PodCIDR           string
	ServiceCIDR       string
	NodeName          string
	SkipPhases        []string // e.g. ["addon/kube-proxy"]
	ExtraArgs         []string // appended to kubeadm init/join
	NodeIP            string
	ImageRepository   string
}

type Kubeadm struct {
	Exec Executor
}

func NewKubeadm(exec Executor) *Kubeadm { return &Kubeadm{Exec: exec} }

// Init runs `kubeadm init` on the given host. The flag set matches
// sealos-style offline init:
//   - skip addon/kube-proxy (replaced by Cilium in S2)
//   - write-kubeconfig for the host's root user
//   - skip install kubelet (we ship our own)
//   - skip mark-control-plane (we don't manage node labels via kubeadm)
func (k *Kubeadm) Init(ctx context.Context, host string, opts KubeadmOptions) (Result, error) {
	args := []string{
		"kubeadm init",
		"--kubernetes-version=" + trimV(opts.KubernetesVersion),
		"--pod-network-cidr=" + opts.PodCIDR,
		"--service-cidr=" + opts.ServiceCIDR,
		"--skip-phases=addon/kube-proxy",
		"--upload-certs",
		"--skip-token-print",
	}
	if opts.ImageRepository != "" {
		args = append(args, "--image-repository="+opts.ImageRepository)
	}
	if opts.APIServerEndpoint != "" {
		args = append(args, "--control-plane-endpoint="+opts.APIServerEndpoint)
	}
	if opts.NodeIP != "" {
		args = append(args, "--node-ip="+opts.NodeIP)
	}
	if opts.CertKey != "" {
		args = append(args, "--certificate-key="+opts.CertKey)
	}
	args = append(args, opts.ExtraArgs...)
	cmd := joinShellCmd(args)
	logger.Info("running kubeadm init", "host", host)
	res := k.Exec.Run(ctx, host, cmd)
	if res.Failed() {
		return res, fmt.Errorf("kubeadm init failed: %w", res.Err)
	}
	return res, nil
}

// Join runs `kubeadm join` on a worker/master node.
func (k *Kubeadm) Join(ctx context.Context, host string, opts KubeadmOptions) (Result, error) {
	args := []string{
		"kubeadm join",
		"--token=" + opts.Token,
		"--discovery-token-ca-cert-hash=" + opts.DiscoveryTokenCAHash,
		"--skip-phases=addon/kube-proxy",
	}
	if opts.APIServerEndpoint != "" {
		args = append(args, "--control-plane", "--certificate-key="+opts.CertKey)
	}
	if opts.NodeIP != "" {
		args = append(args, "--node-ip="+opts.NodeIP)
	}
	args = append(args, opts.ExtraArgs...)
	res := k.Exec.Run(ctx, host, joinShellCmd(args))
	if res.Failed() {
		return res, fmt.Errorf("kubeadm join failed: %w", res.Err)
	}
	return res, nil
}

// Reset runs `kubeadm reset --cleanup-tmp-dir --cri-socket ...`.
// caller passes the active runtime's CRI socket path.
func (k *Kubeadm) Reset(ctx context.Context, host, criSocket string) (Result, error) {
	args := []string{
		"kubeadm reset",
		"--cleanup-tmp-dir",
		"--force",
	}
	if criSocket != "" {
		args = append(args, "--cri-socket="+criSocket)
	}
	res := k.Exec.Run(ctx, host, joinShellCmd(args))
	return res, nil
}

// JoinToken reads the bootstrap token from a freshly-initialised master.
// It looks for the kubeadm join line in /var/log/kubeadm-init.log or
// re-runs `kubeadm token create --print-join-command` to get a fresh one.
func (k *Kubeadm) JoinToken(ctx context.Context, host string) (string, error) {
	res := k.Exec.Run(ctx, host, "kubeadm token create --print-join-command --ttl 24h")
	if res.Failed() {
		return "", fmt.Errorf("kubeadm token create: %w", res.Err)
	}
	return string(trimNewlineBytes(res.Stdout)), nil
}

// CertKey returns a new upload-certs key (used by HA master join).
func (k *Kubeadm) CertKey(ctx context.Context, host string) (string, error) {
	res := k.Exec.Run(ctx, host, "kubeadm init phase upload-certs --upload-certs --skip-certificate-key-print=false 2>&1 | tail -1")
	if res.Failed() {
		return "", fmt.Errorf("upload-certs: %w", res.Err)
	}
	return string(trimNewlineBytes(res.Stdout)), nil
}

// BootstrapKubeadm downloads kubeadm/kubelet/kubectl to the host. In S2 we use
// the OS package manager (matches the docker installer pattern). Offline mode
// (S7) pre-stages the debs/rpms in the bundle.
func (k *Kubeadm) BootstrapKubeadm(ctx context.Context, host, version string) error {
	v := trimV(version)
	script := fmt.Sprintf(`set -euo pipefail
if ! command -v kubeadm >/dev/null 2>&1 || ! kubeadm version -o short | grep -q "%[1]s"; then
  if [ -f /etc/os-release ] && grep -qE 'ID=(ubuntu|debian)' /etc/os-release; then
    apt-get update
    apt-get install -y apt-transport-https ca-certificates curl gpg
    curl -fsSL https://pkgs.k8s.io/core:/stable:/v%[1]s/deb/Release.key | gpg --dearmor -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg
    echo "deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/v%[1]s/deb/ /" > /etc/apt/sources.list.d/kubernetes.list
    apt-get update
    apt-get install -y kubelet=%[1]s-1.1 kubeadm=%[1]s-1.1 kubectl=%[1]s-1.1
    apt-mark hold kubelet kubeadm kubectl
  else
    set -e
    cat > /etc/yum.repos.d/kubernetes.repo <<'KO_REPO_EOF'
[kubernetes]
name=Kubernetes
baseurl=https://pkgs.k8s.io/core:/stable:/v%[1]s/rpm/
enabled=1
gpgcheck=1
gpgkey=https://pkgs.k8s.io/core:/stable:/v%[1]s/rpm/repodata/repomd.xml.key
KO_REPO_EOF
    dnf install -y kubelet-%[1]s kubeadm-%[1]s kubectl-%[1]s
    dnf versionlock add kubelet kubeadm kubectl
  fi
fi
systemctl enable --now kubelet
`, v)
	res := k.Exec.Run(ctx, host, script)
	if res.Failed() {
		return fmt.Errorf("bootstrap kubeadm: %w", res.Err)
	}
	return nil
}

// Generate100YearCA creates a self-signed CA and root cert/key on the LOCAL
// filesystem, returning their paths. S3 will consume these to build kubeadm's
// --rootfs. For S2 the default kubeadm CA is used; this is here for testing
// and to make the 100-year policy explicit.
func Generate100YearCA(commonName string) (certPath, keyPath string, err error) {
	key, err := rsa.GenerateKey(rand.Reader, 4096)
	if err != nil {
		return "", "", err
	}
	notBefore := time.Now().Add(-1 * time.Hour)
	notAfter := notBefore.Add(100 * 365 * 24 * time.Hour)
	serialMax := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, _ := rand.Int(rand.Reader, serialMax)
	tpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: commonName, Organization: []string{"ko"}},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		IsCA:         true,
		KeyUsage:     x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return "", "", err
	}
	dir, err := os.MkdirTemp("", "ko-ca-")
	if err != nil {
		return "", "", err
	}
	certPath = filepath.Join(dir, "ca.crt")
	keyPath = filepath.Join(dir, "ca.key")
	certOut, _ := os.Create(certPath)
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		return "", "", err
	}
	certOut.Close()
	keyOut, _ := os.Create(keyPath)
	keyBytes, _ := x509.MarshalPKCS8PrivateKey(key)
	if err := pem.Encode(keyOut, &pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes}); err != nil {
		return "", "", err
	}
	keyOut.Close()
	return certPath, keyPath, nil
}

func joinShellCmd(parts []string) string {
	var b bytes.Buffer
	for i, p := range parts {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(quoteIfNeeded(p))
	}
	return b.String()
}

func quoteIfNeeded(s string) string {
	for _, r := range s {
		if r == ' ' || r == '"' || r == '\'' || r == '$' || r == '`' || r == '\\' {
			return "\"" + escapeForDoubleQuote(s) + "\""
		}
	}
	return s
}

func escapeForDoubleQuote(s string) string {
	var b bytes.Buffer
	for _, r := range s {
		if r == '"' || r == '\\' || r == '$' || r == '`' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func trimNewlineBytes(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

func trimV(s string) string {
	if len(s) > 0 && s[0] == 'v' {
		return s[1:]
	}
	return s
}

// FromConfig is a convenience to build KubeadmOptions from a parsed config.
func (o KubeadmOptions) FromConfig(cfg *config.File) KubeadmOptions {
	o.KubernetesVersion = cfg.Cluster.Version
	o.PodCIDR = cfg.Cluster.CIDR
	o.ServiceCIDR = cfg.Cluster.SVCCIDR
	o.ImageRepository = cfg.Image.Registry + "/" + cfg.Image.Repository
	o.SkipPhases = []string{"addon/kube-proxy"}
	return o
}
