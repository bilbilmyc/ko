// Package cluster exposes the SSH/Local/Mock executors and the orchestration
// types (Kubeadm, Cilium, kube-vip, init flow) that compose them. The Executor
// interface itself lives in package exec to avoid import cycles with the
// runtime installers (containerd, docker, etc.).
package cluster

import execx "github.com/ko-build/ko/internal/exec"

// Executor re-exports exec.Executor under the cluster namespace so existing
// callers (and tests) can keep using cluster.Executor.
type Executor = execx.Executor

// Result re-exports exec.Result.
type Result = execx.Result

// ErrExecutorClosed re-exports exec.ErrClosed.
var ErrExecutorClosed = execx.ErrClosed
