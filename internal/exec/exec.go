// Package exec defines the Executor interface used by ko to run commands
// on remote nodes (and the local host). It is the lowest-level capability
// that all install/orchestration packages depend on, so it lives in its own
// package to avoid import cycles between cluster orchestrators and runtime
// installers (containerd, docker, etc.).
package exec

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

type Result struct {
	Host    string
	Command string
	Stdout  []byte
	Stderr  []byte
	Err     error
}

func (r Result) Failed() bool { return r.Err != nil }

func (r Result) Error() string {
	if r.Err == nil {
		return ""
	}
	return fmt.Sprintf("host=%s cmd=%q: %v", r.Host, r.Command, r.Err)
}

type Executor interface {
	Run(ctx context.Context, host, command string) Result
	Scp(ctx context.Context, host, src, dst string) error
	Close() error
}

var ErrClosed = errors.New("executor closed")

const DefaultTimeout = 30 * time.Second

type Base struct {
	mu     sync.Mutex
	closed bool
}

func (b *Base) CheckOpen() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrClosed
	}
	return nil
}

func (b *Base) MarkClosed() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
}
