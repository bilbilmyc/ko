package cluster

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/ko-build/ko/internal/etcd"
	"github.com/ko-build/ko/internal/logger"
	"github.com/ko-build/ko/pkg/config"
)

// IsExternalEtcd reports whether the config wants us to deploy (or assume
// an existing) external etcd cluster.
func IsExternalEtcd(cfg *config.File) bool {
	return strings.EqualFold(cfg.Etcd.Mode, "external")
}

// EtcdExternalMembers converts the HCL members list into the etcd pkg
// shape, applying defaults.
func EtcdExternalMembers(cfg *config.File) []etcd.Member {
	if len(cfg.Etcd.Members) == 0 {
		return nil
	}
	out := make([]etcd.Member, 0, len(cfg.Etcd.Members))
	for _, m := range cfg.Etcd.Members {
		host := m.Host
		if host == "" {
			host = m.Name
		}
		out = append(out, etcd.Member{
			Name:    m.Name,
			Host:    host,
			DataDir: "/var/lib/etcd/" + m.Name,
		})
	}
	return out
}

// ProvisionExternalEtcd runs the full external etcd flow on the supplied
// executor: download, PKI generation, install + start, backup timer,
// health wait, master client cert distribution. Idempotent — re-runs
// re-use the existing CA and only refresh service units.
func ProvisionExternalEtcd(ctx context.Context, cfg *config.File, exec Executor, masters []string) error {
	members := EtcdExternalMembers(cfg)
	if len(members) == 0 {
		return fmt.Errorf("etcd.mode=external but no members declared")
	}
	logger.Info("etcd external: provisioning", "members", len(members), "masters", len(masters))

	cacheDir := filepath.Join(cacheHome(), "etcd")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return fmt.Errorf("mkdir cache: %w", err)
	}
	tarball, err := etcd.Download(etcd.DownloadOptions{
		Dir:     cacheDir,
		Version: "v" + strings.TrimPrefix(etcd.DefaultVersion, "v"),
		Arch:    runtime.GOARCH,
	})
	if err != nil {
		return fmt.Errorf("download etcd: %w", err)
	}

	hosts := make([]etcd.CertHosts, 0, len(members))
	for _, m := range members {
		hosts = append(hosts, etcd.CertHosts{Name: m.Name, IP: m.Host})
	}
	pkiDir := filepath.Join(cacheDir, "pki")
	paths, err := etcd.Generate(etcd.GenerateOptions{Dir: pkiDir, Hosts: hosts})
	if err != nil {
		return fmt.Errorf("generate pki: %w", err)
	}
	logger.Info("etcd external: PKI generated", "dir", pkiDir)

	cc := etcd.ClusterConfig{
		Members:      members,
		ClusterToken: "ko-etcd-" + cfg.Cluster.Name,
		PKIDir:       cfg.Etcd.PKIDir,
		InitialState: "new",
	}
	svc := etcd.NewService(exec, tarball, "v"+etcd.DefaultVersion, cc)
	for _, m := range members {
		if err := svc.Install(ctx, m, paths); err != nil {
			return fmt.Errorf("install etcd on %s: %w", m.Host, err)
		}
		backup := etcd.NewBackupService(exec)
		backup.PKIDir = cfg.Etcd.PKIDir
		if err := backup.Install(ctx, m.Host); err != nil {
			logger.Warn("etcd backup install failed", "host", m.Host, "err", err)
		}
	}

	if err := WaitForEtcdHealthy(ctx, exec, members, cfg.Etcd.PKIDir); err != nil {
		return fmt.Errorf("etcd not healthy: %w", err)
	}
	for _, m := range masters {
		if err := InstallEtcdClientOnMaster(ctx, exec, m, cfg.Etcd.PKIDir, paths); err != nil {
			return fmt.Errorf("install etcd client on master %s: %w", m, err)
		}
	}
	logger.Info("etcd external: ready", "members", len(members), "pki", cfg.Etcd.PKIDir)
	return nil
}

// UninstallExternalEtcd stops + removes etcd on every member. Backups are
// left in place unless purgeBackups is true.
func UninstallExternalEtcd(ctx context.Context, cfg *config.File, exec Executor, purgeBackups bool) error {
	members := EtcdExternalMembers(cfg)
	if len(members) == 0 {
		return fmt.Errorf("etcd.mode=external but no members declared")
	}
	svc := etcd.NewService(exec, "", "v"+etcd.DefaultVersion, etcd.ClusterConfig{
		Members:      members,
		ClusterToken: "ko-etcd-" + cfg.Cluster.Name,
		PKIDir:       cfg.Etcd.PKIDir,
		InitialState: "existing",
	})
	for _, m := range members {
		if err := svc.Uninstall(ctx, m); err != nil {
			return fmt.Errorf("uninstall %s: %w", m.Host, err)
		}
		if purgeBackups {
			backup := etcd.NewBackupService(exec)
			backup.PKIDir = cfg.Etcd.PKIDir
			_ = backup.Uninstall(ctx, m.Host)
			_ = exec.Run(ctx, m.Host, "rm -rf /var/backups/etcd")
		}
	}
	return nil
}

// WaitForEtcdHealthy probes /health on every member and returns nil once
// all of them reply healthy. Bounded by 2m timeout.
func WaitForEtcdHealthy(ctx context.Context, ex Executor, members []etcd.Member, pkiDir string) error {
	logger.Info("etcd external: waiting for cluster health", "members", len(members))
	probe := func() bool {
		for _, m := range members {
			script := fmt.Sprintf(
				`curl -sk --max-time 4 --cacert %s/ca.crt --cert %s/client.crt --key %s/client.key https://%s:2379/health 2>/dev/null`,
				pkiDir, pkiDir, pkiDir, m.Host)
			r := ex.Run(ctx, m.Host, script)
			if r.Failed() {
				return false
			}
			if !strings.Contains(string(r.Stdout), `"health":"true"`) {
				return false
			}
		}
		return true
	}
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		if probe() {
			return nil
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("etcd cluster did not become healthy within 2m")
}

// InstallEtcdClientOnMaster rsyncs the ca + client cert + key onto a
// single master, into the on-host PKI dir. kubeadm reads them when
// building apiserver flags.
func InstallEtcdClientOnMaster(ctx context.Context, ex Executor, host, dst string, paths *etcd.CertPaths) error {
	mkdir := fmt.Sprintf("mkdir -p %s", dst)
	if r := ex.Run(ctx, host, mkdir); r.Failed() {
		return fmt.Errorf("mkdir: %w", r.Err)
	}
	pairs := []struct{ src, dst string }{
		{paths.CA, dst + "/ca.crt"},
		{paths.Client, dst + "/client.crt"},
		{filepath.Join(paths.Dir, "client.key"), dst + "/client.key"},
	}
	for _, p := range pairs {
		if err := ex.Scp(ctx, host, p.src, p.dst); err != nil {
			return fmt.Errorf("scp %s: %w", p.src, err)
		}
	}
	perm := fmt.Sprintf("chmod 0644 %s/ca.crt %s/client.crt && chmod 0600 %s/client.key", dst, dst, dst)
	if r := ex.Run(ctx, host, perm); r.Failed() {
		return fmt.Errorf("chmod: %w", r.Err)
	}
	return nil
}

