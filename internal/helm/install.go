// Package helm wraps the Helm 3 SDK for installing cluster-level components
// (Cilium, kube-vip, Flannel, etc.). The wrapper hides chart-pull mechanics
// so that offline mode (S7) can swap the chart source without touching callers.
package helm

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/repo"
)

type Installer struct {
	// KubeConfig is the path to a kubeconfig that can reach the target cluster
	// (typically /etc/kubernetes/admin.conf on the init master, scp'd locally).
	KubeConfig string
	// ChartSource: "repo" (online) or "tgz" (offline, S7)
	ChartSource string
	// RepoFile / RepoCache: for "repo" mode
	RepoFile  string
	RepoCache string
	// LocalDir: directory containing <name>-<ver>.tgz files for "tgz" mode
	LocalDir string
}

func New(kubeConfig string) *Installer {
	home, _ := os.UserHomeDir()
	return &Installer{
		KubeConfig:  kubeConfig,
		ChartSource: "repo",
		RepoFile:    filepath.Join(home, ".config", "helm", "repositories.yaml"),
		RepoCache:   filepath.Join(home, ".cache", "helm", "repository"),
	}
}

// InstallOptions configures a single chart install.
type InstallOptions struct {
	ReleaseName string
	Namespace   string
	Chart       string // e.g. "cilium/cilium" or path/to/cilium-1.16.tgz
	Version     string // e.g. "1.16.1" (empty = latest)
	Values      map[string]any
	Wait        bool
	Timeout     string // e.g. "5m"
	CreateNS    bool
}

// Install runs `helm install <release> <chart>`. Returns the release on success.
func (i *Installer) Install(ctx context.Context, opts InstallOptions) (*release.Release, error) {
	cfg, err := i.actionConfig(opts.Namespace)
	if err != nil {
		return nil, err
	}
	action := action.NewInstall(cfg)
	action.ReleaseName = opts.ReleaseName
	action.Namespace = opts.Namespace
	action.Wait = opts.Wait
	action.CreateNamespace = opts.CreateNS
	if opts.Timeout != "" {
		action.Timeout = parseTimeout(opts.Timeout)
	}
	action.ChartPathOptions.RepoURL = "" // we'll resolve ourselves
	action.ChartPathOptions.Version = opts.Version

	cp, err := i.locateChart(opts)
	if err != nil {
		return nil, fmt.Errorf("locate chart %q: %w", opts.Chart, err)
	}
	ch, err := loader.Load(cp)
	if err != nil {
		return nil, fmt.Errorf("load chart %q: %w", cp, err)
	}
	return action.Run(ch, opts.Values)
}

// Upgrade runs `helm upgrade --install` to bring a release to a new state.
func (i *Installer) Upgrade(ctx context.Context, opts InstallOptions) (*release.Release, error) {
	cfg, err := i.actionConfig(opts.Namespace)
	if err != nil {
		return nil, err
	}
	action := action.NewUpgrade(cfg)
	action.Namespace = opts.Namespace
	action.Wait = opts.Wait
	if opts.Timeout != "" {
		action.Timeout = parseTimeout(opts.Timeout)
	}
	action.ChartPathOptions.Version = opts.Version

	cp, err := i.locateChart(opts)
	if err != nil {
		return nil, err
	}
	ch, err := loader.Load(cp)
	if err != nil {
		return nil, err
	}
	return action.Run(opts.ReleaseName, ch, opts.Values)
}

func (i *Installer) actionConfig(namespace string) (*action.Configuration, error) {
	settings := cli.New()
	settings.KubeConfig = i.KubeConfig
	cfg := &action.Configuration{}
	if err := cfg.Init(settings.RESTClientGetter(), namespace, "secret", nil); err != nil {
		return nil, fmt.Errorf("init helm action config: %w", err)
	}
	return cfg, nil
}

func (i *Installer) locateChart(opts InstallOptions) (string, error) {
	if i.ChartSource == "tgz" && i.LocalDir != "" {
		// expect <chart>-<ver>.tgz
		if opts.Version == "" {
			return "", fmt.Errorf("offline mode requires explicit Version")
		}
		candidate := filepath.Join(i.LocalDir, fmt.Sprintf("%s-%s.tgz", filepath.Base(opts.Chart), opts.Version))
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
		// fallback: any .tgz matching chart basename
		matches, _ := filepath.Glob(filepath.Join(i.LocalDir, filepath.Base(opts.Chart)+"-*.tgz"))
		if len(matches) == 0 {
			return "", fmt.Errorf("no tgz found for %q in %s", opts.Chart, i.LocalDir)
		}
		return matches[0], nil
	}
	// online: rely on action.ChartPathOptions via the chart's repository
	// For now: assume opts.Chart is a local path or a "<repo>/<chart>" reference
	// and let the action layer resolve it.
	if _, err := os.Stat(opts.Chart); err == nil {
		return opts.Chart, nil
	}
	// Use a minimal pull to resolve "<repo>/<chart>" + version
	pull := action.NewPull()
	pull.Settings = cli.New()
	pull.Settings.KubeConfig = i.KubeConfig
	pull.RepoURL = "" // caller is expected to have added the repo
	pull.Version = opts.Version
	pull.DestDir = i.LocalDir
	return "", fmt.Errorf("online chart resolution not yet implemented; use ChartSource=tgz with a vendored chart, or set RepoURL+Version")
}

// EnsureRepo adds a helm repo to the local repo file (idempotent).
func EnsureRepo(name, url, repoFile string) error {
	rf := repoFile
	if rf == "" {
		home, _ := os.UserHomeDir()
		rf = filepath.Join(home, ".config", "helm", "repositories.yaml")
	}
	if err := os.MkdirAll(filepath.Dir(rf), 0o755); err != nil {
		return err
	}
	entries, err := repo.LoadFile(rf)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if entries == nil {
		entries = repo.NewFile()
	}
	if existing := entries.Get(name); existing != nil && existing.URL == url {
		return nil
	}
	entries.Add(&repo.Entry{Name: name, URL: url})
	return entries.WriteFile(rf, 0o644)
}

func parseTimeout(s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0
	}
	return d
}
