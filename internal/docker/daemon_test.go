package docker

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	execx "github.com/ko-build/ko/internal/exec"
)

func TestDefaultDaemon(t *testing.T) {
	cfg := DefaultDaemon([]string{"https://mirror.example.com"}, []string{"harbor.local"})
	assert.Contains(t, cfg, `"native.cgroupdriver=systemd"`)
	assert.Contains(t, cfg, `"https://mirror.example.com"`)
	assert.Contains(t, cfg, `"harbor.local"`)
	assert.Contains(t, cfg, `"log-driver": "json-file"`)
	assert.Contains(t, cfg, `"storage-driver": "overlay2"`)
}

func TestDefaultDaemon_EmptyMirrors(t *testing.T) {
	cfg := DefaultDaemon(nil, nil)
	assert.Contains(t, cfg, "native.cgroupdriver=systemd")
	assert.NotContains(t, cfg, "registry-mirrors")
}

func TestInstall_DetectsDebian(t *testing.T) {
	mock := execx.NewMock()
	defer mock.Close()
	mock.RunFn = func(ctx context.Context, host, command string) execx.Result {
		if strings.Contains(command, "os-release") {
			return execx.Result{Host: host, Command: command, Stdout: []byte(`ID="ubuntu"`)}
		}
		return execx.Result{Host: host, Command: command}
	}
	inst := NewInstaller(mock, "27.5.1", "stable")
	require.NoError(t, inst.Install(context.Background(), "h"))
	foundApt := false
	for _, c := range mock.Calls {
		if strings.Contains(c.Command, "apt-get install") {
			foundApt = true
		}
	}
	assert.True(t, foundApt)
}

func TestInstall_DetectsRPM(t *testing.T) {
	mock := execx.NewMock()
	defer mock.Close()
	mock.RunFn = func(ctx context.Context, host, command string) execx.Result {
		if strings.Contains(command, "os-release") {
			return execx.Result{Host: host, Command: command, Stdout: []byte(`ID="rocky"`)}
		}
		return execx.Result{Host: host, Command: command}
	}
	inst := NewInstaller(mock, "27.5.1", "stable")
	require.NoError(t, inst.Install(context.Background(), "h"))
	foundDnf := false
	for _, c := range mock.Calls {
		if strings.Contains(c.Command, "dnf -y install") {
			foundDnf = true
		}
	}
	assert.True(t, foundDnf)
}

func TestInstall_UnsupportedOS(t *testing.T) {
	mock := execx.NewMock()
	defer mock.Close()
	mock.RunFn = func(ctx context.Context, host, command string) execx.Result {
		if strings.Contains(command, "os-release") {
			return execx.Result{Host: host, Command: command, Stdout: []byte(`ID="aix"`)}
		}
		return execx.Result{Host: host, Command: command}
	}
	inst := NewInstaller(mock, "27.5.1", "stable")
	err := inst.Install(context.Background(), "h")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported OS")
}
