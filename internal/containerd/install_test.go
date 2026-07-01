package containerd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	execx "github.com/ko-build/ko/internal/exec"
)

func TestNewInstaller_Defaults(t *testing.T) {
	i := NewInstaller(nil, "", "upstream", "", "")
	assert.Equal(t, defaultVersion, i.Version)
	assert.Equal(t, "upstream", i.Source)
	assert.NotEmpty(t, i.Cache)
}

func TestInstaller_SourceGuard(t *testing.T) {
	i := NewInstaller(nil, "v2.0.5", "vendor", "amd64", "")
	err := i.Install(context.Background(), "h", "version=2")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "only upstream source")
}

func TestInstaller_ArchGuard(t *testing.T) {
	i := NewInstaller(nil, "v2.0.5", "upstream", "ppc64le", "")
	err := i.Install(context.Background(), "h", "version=2")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported arch")
}

func TestInstaller_Idempotent_ExistingVersion(t *testing.T) {
	mock := execx.NewMock()
	defer mock.Close()
	mock.RunFn = func(ctx context.Context, host, command string) execx.Result {
		switch {
		case strings.Contains(command, "containerd --version"):
			return execx.Result{Host: host, Command: command, Stdout: []byte("containerd v2.0.5")}
		case strings.HasPrefix(command, "systemctl"):
			return execx.Result{Host: host, Command: command}
		}
		return execx.Result{Host: host, Command: command}
	}

	cache := t.TempDir()
	i := NewInstaller(mock, "v2.0.5", "upstream", "amd64", cache)
	require.NoError(t, i.Install(context.Background(), "host1", "version = 2\n"))

	for _, c := range mock.Calls {
		assert.NotContains(t, c.Command, "tar -C", "should not extract on existing version")
	}
}

func TestInstaller_AllPaths_WithMock(t *testing.T) {
	host := "host1"
	ranMkdir := false
	ranSystemd := false
	mock := execx.NewMock()
	defer mock.Close()
	mock.RunFn = func(ctx context.Context, h, command string) execx.Result {
		if strings.Contains(command, "containerd --version") {
			return execx.Result{Host: h, Command: command, Err: assert.AnError}
		}
		if strings.Contains(command, "mkdir -p /etc/containerd") {
			ranMkdir = true
		}
		if strings.Contains(command, "systemctl") {
			ranSystemd = true
		}
		return execx.Result{Host: h, Command: command}
	}
	mock.ScpFn = func(ctx context.Context, h, src, dst string) error {
		if strings.HasSuffix(dst, "/etc/containerd/config.toml") {
			return assert.AnError
		}
		return nil
	}

	cache := t.TempDir()
	tarballName := "containerd-2.0.5-linux-amd64.tar.gz"
	require.NoError(t, os.WriteFile(filepath.Join(cache, tarballName), []byte("fake-tarball"), 0o644))

	i := NewInstaller(mock, "v2.0.5", "upstream", "amd64", cache)
	require.NoError(t, i.Install(context.Background(), host, "version = 2\n"))

	assert.True(t, ranMkdir, "mkdir + cat > /etc/containerd/config.toml should have run")
	assert.True(t, ranSystemd, "systemctl commands should have run")
}

func TestFetchTarball_CacheHit(t *testing.T) {
	cache := t.TempDir()
	tarball := filepath.Join(cache, "containerd-2.0.5-linux-amd64.tar.gz")
	require.NoError(t, os.WriteFile(tarball, []byte("x"), 0o644))

	i := NewInstaller(nil, "v2.0.5", "upstream", "amd64", cache)
	got, err := i.fetchTarball(context.Background())
	require.NoError(t, err)
	assert.Equal(t, tarball, got)
}

func TestFetchChecksum_Parse(t *testing.T) {
	got, err := fetchChecksum(context.Background(), "http://127.0.0.1:1/missing", "containerd-x.tar.gz")
	assert.Error(t, err)
	assert.Equal(t, "", got)
}
