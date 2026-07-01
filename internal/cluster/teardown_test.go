package cluster

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseCertLine_OK(t *testing.T) {
	line := "FILE=/etc/kubernetes/pki/ca.crt|notAfter=Jul  1 12:34:56 2126 GMT|subject=CN=k8s"
	info := parseCertLine("m1", line)
	if assert.NotNil(t, info) {
		assert.Equal(t, "/etc/kubernetes/pki/ca.crt", info.Path)
		assert.Equal(t, "m1", info.Host)
		assert.Equal(t, "CN=k8s", info.Subject)
		assert.Equal(t, 2126, info.NotAfter.Year())
	}
}

func TestParseCertLine_Bad(t *testing.T) {
	assert.Nil(t, parseCertLine("m1", ""))
	assert.Nil(t, parseCertLine("m1", "junk line"))
}

func TestTeardown_ResetAll_Order(t *testing.T) {
	var order []string
	mock := NewMockExecutor()
	defer mock.Close()
	mock.RunFn = func(_ context.Context, h, _ string) Result {
		order = append(order, h)
		return Result{Host: h, Command: "ok"}
	}
	t0 := NewTeardown(mock)
	t0.CRI = "unix:///run/containerd/containerd.sock"
	err := t0.ResetAll(context.Background(),
		[]string{"m1", "m2"}, []string{"w1", "w2"})
	assert.NoError(t, err)
	// workers come first, then masters
	assert.Equal(t, []string{"w1", "w2", "m1", "m2"}, order)
}

func TestParseCertLine_TimeIn100Years(t *testing.T) {
	line := "FILE=/etc/kubernetes/pki/ca.crt|notAfter=Jul  1 12:34:56 2126 GMT|subject=CN=k"
	info := parseCertLine("m1", line)
	if assert.NotNil(t, info) {
		assert.Equal(t, 2126, info.NotAfter.Year())
	}
}