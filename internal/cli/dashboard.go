package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/ko-build/ko/internal/cluster"
	"github.com/ko-build/ko/internal/dashboard"
	"github.com/ko-build/ko/internal/etcd"
	"github.com/ko-build/ko/internal/logger"
	"github.com/ko-build/ko/pkg/config"
)

func newDashboardCmd() *cobra.Command {
	var (
		staticDir string
		user      string
		password  string
	)
	cmd := &cobra.Command{
		Use:   "dashboard",
		Short: "Launch the Web Dashboard (HTTP + basic auth + REST + minimal static UI)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfgPath := flags.ConfigPath
			if cfgPath == "" {
				return fmt.Errorf("--config is required")
			}
			cfg, err := config.ParseFile(cfgPath)
			if err != nil {
				return fmt.Errorf("parse %q: %w", cfgPath, err)
			}
			cfg.ApplyDefaults()

			if user == "" {
				user = cfg.Dashboard.BasicAuth.User
			}
			if user == "" {
				user = "admin"
			}
			if password == "" {
				password = os.Getenv("KO_DASHBOARD_PASSWORD")
			}
			if password == "" {
				return fmt.Errorf("set password via --password or KO_DASHBOARD_PASSWORD env var")
			}

			listen := cfg.Dashboard.Listen
			if listen == "" {
				listen = "127.0.0.1:8080"
			}

			api := newAPIAdapter(cfg)

			srv := dashboard.New(dashboard.Config{
				Listen:      listen,
				User:        user,
				Password:    password,
				StaticDir:   staticDir,
				ClusterInfo: api,
				Node:        api,
				Certs:       api,
				Etcd:        api,
			})
			cmd.Printf("ko dashboard: listening on http://%s (user=%s)\n", listen, user)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() {
				if err := srv.Run(); err != nil {
					logger.Error("dashboard stopped", "err", err)
				}
			}()
			<-ctx.Done()
			shutdownCtx, sc := context.WithTimeout(context.Background(), 5*time.Second)
			defer sc()
			return srv.Shutdown(shutdownCtx)
		},
	}
	cmd.Flags().StringVar(&staticDir, "static-dir", "", "directory with built frontend (default: embedded minimal page)")
	cmd.Flags().StringVar(&user, "user", "", "basic auth user (default from cluster.hcl dashboard.basic_auth.user)")
	cmd.Flags().StringVar(&password, "password", "", "basic auth password (default $KO_DASHBOARD_PASSWORD)")
	return cmd
}

// apiAdapter wires the dashboard Server's interfaces to the actual cluster
// operations. The wiring is intentionally thin — each method builds an SSH
// executor from cfg, performs the op, and returns a string/result.
type apiAdapter struct {
	cfg *config.File
}

func newAPIAdapter(cfg *config.File) *apiAdapter {
	return &apiAdapter{cfg: cfg}
}

func (a *apiAdapter) sshExec() (cluster.Executor, func(), error) {
	sshCfg := cluster.SSHConfig{
		User:     a.cfg.Nodes.SSH.User,
		Port:     a.cfg.Nodes.SSH.Port,
		KeyFile:  a.cfg.Nodes.SSH.KeyFile,
		Password: a.cfg.Nodes.SSH.Password,
		Timeout:  60 * time.Second,
	}
	ex, err := cluster.NewSSHExecutor(sshCfg)
	if err != nil {
		return nil, nil, err
	}
	return ex, func() { _ = ex.Close() }, nil
}

func (a *apiAdapter) Summary() (map[string]any, error) {
	masters := a.cfg.Nodes.Masters
	workers := a.cfg.Nodes.Workers
	return map[string]any{
		"name":    a.cfg.Cluster.Name,
		"version": a.cfg.Cluster.Version,
		"podCIDR": a.cfg.Cluster.CIDR,
		"svcCIDR": a.cfg.Cluster.SVCCIDR,
		"masters": masters,
		"workers": workers,
		"vip":     a.cfg.HA.VIP,
		"runtime": a.cfg.Runtime.Default,
		"cni":     a.cfg.CNI.Plugin,
	}, nil
}

func (a *apiAdapter) List() (string, error) {
	ex, closer, err := a.sshExec()
	if err != nil {
		return "", err
	}
	defer closer()
	nl := cluster.NewNodeLifecycle(a.cfg, ex, cluster.NewKubeadm(ex),
		filepath.Join(homeDir(), ".ko", "kube", "admin.conf"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return nl.List(ctx)
}

func (a *apiAdapter) Add(host, role string) error {
	ex, closer, err := a.sshExec()
	if err != nil {
		return err
	}
	defer closer()
	nl := cluster.NewNodeLifecycle(a.cfg, ex, cluster.NewKubeadm(ex),
		filepath.Join(homeDir(), ".ko", "kube", "admin.conf"))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	if role == "master" {
		return nl.AddMaster(ctx, host)
	}
	return nl.AddWorker(ctx, host)
}

func (a *apiAdapter) Remove(host string, force bool) error {
	ex, closer, err := a.sshExec()
	if err != nil {
		return err
	}
	defer closer()
	nl := cluster.NewNodeLifecycle(a.cfg, ex, cluster.NewKubeadm(ex),
		filepath.Join(homeDir(), ".ko", "kube", "admin.conf"))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	return nl.Remove(ctx, host, cluster.RemoveOptions{Force: force})
}

func (a *apiAdapter) ListCerts() ([]dashboard.CertInfo, error) {
	ex, closer, err := a.sshExec()
	if err != nil {
		return nil, err
	}
	defer closer()
	td := cluster.NewTeardown(ex)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	certs, err := td.ListCertificates(ctx, a.cfg.Nodes.Masters)
	if err != nil {
		return nil, err
	}
	out := make([]dashboard.CertInfo, 0, len(certs))
	for _, c := range certs {
		out = append(out, dashboard.CertInfo{
			Host:     c.Host,
			Path:     c.Path,
			NotAfter: c.NotAfter.Format("2006-01-02"),
			Subject:  c.Subject,
		})
	}
	return out, nil
}

// EtcdAPI implementation on apiAdapter — returns nil/empty when the
// config is in stacked etcd mode (so the dashboard renders "stacked"
// rather than failing).
func (a *apiAdapter) Status() (*dashboard.EtcdStatus, error) {
	if a.cfg.Etcd.Mode != "external" {
		return nil, nil
	}
	ex, closer, err := a.sshExec()
	if err != nil {
		return nil, err
	}
	defer closer()
	members := cluster.EtcdExternalMembers(a.cfg)
	svc := etcd.NewService(ex, "", "v"+trimV(etcd.DefaultVersion), etcd.ClusterConfig{
		Members:      members,
		ClusterToken: "ko-etcd-" + a.cfg.Cluster.Name,
		PKIDir:       a.cfg.Etcd.PKIDir,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	statuses, err := svc.Status(ctx, members)
	if err != nil {
		return nil, err
	}
	st := &dashboard.EtcdStatus{Mode: "external"}
	for _, s := range statuses {
		st.Members = append(st.Members, dashboard.EtcdMember{
			Name:           s.Name,
			Host:           s.Host,
			Active:         s.Active,
			EndpointHealth: s.EndpointHealth,
		})
	}
	return st, nil
}

func (a *apiAdapter) ListBackups() ([]dashboard.EtcdBackup, error) {
	if a.cfg.Etcd.Mode != "external" {
		return nil, nil
	}
	ex, closer, err := a.sshExec()
	if err != nil {
		return nil, err
	}
	defer closer()
	backup := etcd.NewBackupService(ex)
	backup.PKIDir = a.cfg.Etcd.PKIDir
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var out []dashboard.EtcdBackup
	for _, m := range a.cfg.Etcd.Members {
		list, err := backup.ListBackups(ctx, m.Host)
		if err != nil {
			continue
		}
		for _, b := range list {
			out = append(out, dashboard.EtcdBackup{
				Host:     b.Host,
				Name:     b.Name,
				Filename: b.Filename,
				Size:     b.Size,
				ModTime:  b.ModTime,
			})
		}
	}
	return out, nil
}

// io.Discard keep the import alive if not used directly.
var _ = io.Discard