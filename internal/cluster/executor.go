package cluster

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

var ErrExecutorClosed = errors.New("executor closed")

const (
	DefaultTimeout = 30 * time.Second
)

type baseExecutor struct {
	mu     sync.Mutex
	closed bool
}

func (b *baseExecutor) checkOpen() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrExecutorClosed
	}
	return nil
}

func (b *baseExecutor) markClosed() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
}
