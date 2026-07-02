package cluster

import (
	"context"
	"errors"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMockExecutor_Run(t *testing.T) {
	m := NewMockExecutor()
	defer m.Close()

	called := false
	m.RunFn = func(ctx context.Context, host, command string) Result {
		called = true
		return Result{Host: host, Command: command, Stdout: []byte("hello")}
	}

	res := m.Run(context.Background(), "host1", "echo hello")
	require.False(t, res.Failed(), res.Error())
	assert.Equal(t, []byte("hello"), res.Stdout)
	assert.True(t, called)
	assert.Equal(t, "Run", m.Calls[0].Method)
	assert.Equal(t, "host1", m.Calls[0].Host)
	assert.Equal(t, "echo hello", m.Calls[0].Command)
}

func TestMockExecutor_AfterClose(t *testing.T) {
	m := NewMockExecutor()
	m.Close()

	res := m.Run(context.Background(), "h", "true")
	assert.True(t, res.Failed())
	assert.ErrorIs(t, res.Err, ErrExecutorClosed)
}

func TestMockExecutor_Scp(t *testing.T) {
	m := NewMockExecutor()
	defer m.Close()

	called := false
	m.ScpFn = func(ctx context.Context, host, src, dst string) error {
		called = true
		assert.Equal(t, "h", host)
		assert.Equal(t, "/src", src)
		assert.Equal(t, "/dst", dst)
		return nil
	}
	require.NoError(t, m.Scp(context.Background(), "h", "/src", "/dst"))
	assert.True(t, called)
}

func TestLocalExecutor_Localhost(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skip on windows for CI")
	}
	le := NewLocalExecutor()
	defer le.Close()

	res := le.Run(context.Background(), "localhost", "echo hello")
	require.False(t, res.Failed(), res.Error())
	assert.Contains(t, string(res.Stdout), "hello")
}

func TestLocalExecutor_RejectsRemote(t *testing.T) {
	le := NewLocalExecutor()
	defer le.Close()

	res := le.Run(context.Background(), "10.0.0.1", "true")
	assert.True(t, res.Failed())
	assert.Contains(t, res.Error(), "local executor cannot reach")
}

func TestLocalExecutor_PropagatesExitError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skip on windows for CI")
	}
	le := NewLocalExecutor()
	defer le.Close()

	res := le.Run(context.Background(), "", `sh -c 'echo ko-stderr-test >&2; exit 1'`)
	assert.True(t, res.Failed())
	assert.NotEmpty(t, res.Stderr)
}

func TestLocalExecutor_Scp(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skip on windows for CI")
	}
	le := NewLocalExecutor()
	defer le.Close()

	src := t.TempDir() + "/src.txt"
	dst := t.TempDir() + "/sub/dst.txt"
	require.NoError(t, writeFile(src, "data"))
	require.NoError(t, le.Scp(context.Background(), "", src, dst))
	got, err := readFile(dst)
	require.NoError(t, err)
	assert.Equal(t, "data", string(got))
}

func TestSSHExecutor_AuthMethods_None(t *testing.T) {
	_, err := NewSSHExecutor(SSHConfig{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no ssh auth method")
}

func TestSSHExecutor_KeyFile(t *testing.T) {
	dir := t.TempDir()
	keyPath := dir + "/id_rsa"
	require.NoError(t, writeFile(keyPath, dummyKey(t)))
	_, err := NewSSHExecutor(SSHConfig{KeyFile: keyPath})
	require.NoError(t, err)
}

func TestSSHExecutor_HostKeyCallback(t *testing.T) {
	dir := t.TempDir()
	kh := dir + "/known_hosts"
	require.NoError(t, writeFile(kh, ""))
	keyPath := dir + "/id_rsa"
	require.NoError(t, writeFile(keyPath, dummyKey(t)))
	_, err := NewSSHExecutor(SSHConfig{KeyFile: keyPath, KnownHosts: kh})
	require.NoError(t, err)
}

func TestResult_Failed(t *testing.T) {
	assert.False(t, Result{}.Failed())
	assert.True(t, Result{Err: errors.New("x")}.Failed())
}

func writeFile(path, data string) error {
	return writeBytes(path, []byte(data))
}

func readFile(path string) ([]byte, error) {
	return readBytes(path)
}

func writeBytes(path string, b []byte) error { return writeFileOS(path, b) }
func readBytes(path string) ([]byte, error)  { return readFileOS(path) }
