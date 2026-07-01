package cluster

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ko-build/ko/pkg/config"
)

func TestKubectl_Builds(t *testing.T) {
	assert.Equal(t, "kubectl get nodes", kubectl("", "get nodes"))
	assert.Equal(t, "kubectl --kubeconfig=/tmp/admin.conf get nodes",
		kubectl("/tmp/admin.conf", "get nodes"))
}

func TestNodeLifecycle_Remove_BuildsCommands(t *testing.T) {
	mock := NewMockExecutor()
	defer mock.Close()
	n := &NodeLifecycle{
		Cfg: &config.File{
			Cluster: config.ClusterBlock{Version: "1.35.0"},
			Nodes:   config.NodesBlock{Masters: []string{"m1"}},
		},
		Exec:            mock,
		Kubeadm:         NewKubeadm(mock),
		KubeConfigPath:  "/tmp/admin.conf",
	}
	require.NoError(t, n.Remove(context.Background(), "w1", RemoveOptions{Force: false}))

	var drain, del, reset string
	for _, c := range mock.Calls {
		switch {
		case strings.Contains(c.Command, "drain"):
			drain = c.Command
		case strings.Contains(c.Command, "delete node"):
			del = c.Command
		case strings.Contains(c.Command, "kubeadm reset"):
			reset = c.Command
		}
	}
	assert.Contains(t, drain, "kubectl --kubeconfig=/tmp/admin.conf drain w1")
	assert.Contains(t, drain, "--ignore-daemonsets")
	assert.Contains(t, del, "delete node w1")
	assert.Contains(t, reset, "kubeadm reset")
	assert.Contains(t, reset, "--cri-socket=unix:///run/containerd/containerd.sock")
}

func TestNodeLifecycle_Remove_DockerCRI(t *testing.T) {
	mock := NewMockExecutor()
	defer mock.Close()
	n := &NodeLifecycle{
		Cfg: &config.File{
			Cluster: config.ClusterBlock{Version: "1.35.0"},
			Nodes:   config.NodesBlock{Masters: []string{"m1"}},
			NodesOverride: []config.NodesOverrideBlock{
				{Host: "w1", Runtime: "docker"},
			},
		},
		Exec:            mock,
		Kubeadm:         NewKubeadm(mock),
		KubeConfigPath:  "/tmp/admin.conf",
	}
	require.NoError(t, n.Remove(context.Background(), "w1", RemoveOptions{Force: true}))
	for _, c := range mock.Calls {
		if strings.Contains(c.Command, "kubeadm reset") {
			assert.Contains(t, c.Command, "unix:///var/run/docker.sock")
		}
	}
}

func TestNodeLifecycle_Label(t *testing.T) {
	mock := NewMockExecutor()
	defer mock.Close()
	n := &NodeLifecycle{
		Cfg:            &config.File{},
		Exec:           mock,
		KubeConfigPath: "/tmp/admin.conf",
	}
	require.NoError(t, n.Label(context.Background(), "w1", "role", "worker"))
	require.NotEmpty(t, mock.Calls)
	assert.Contains(t, mock.Calls[0].Command, "kubectl --kubeconfig=/tmp/admin.conf label node w1 role=worker --overwrite")
}