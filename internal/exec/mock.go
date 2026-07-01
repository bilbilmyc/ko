package exec

import (
	"context"
	"sync"
)

// MockExecutor is a minimal in-memory executor for unit tests. It records
// every call and lets the test inject behaviour via RunFn / ScpFn.
type MockExecutor struct {
	Base
	mu sync.Mutex

	RunFn func(ctx context.Context, host, command string) Result
	ScpFn func(ctx context.Context, host, src, dst string) error

	Calls []Call
}

type Call struct {
	Method  string
	Host    string
	Command string
	Src     string
	Dst     string
}

func NewMock() *MockExecutor { return &MockExecutor{} }

func (m *MockExecutor) Run(ctx context.Context, host, command string) Result {
	m.mu.Lock()
	m.Calls = append(m.Calls, Call{Method: "Run", Host: host, Command: command})
	m.mu.Unlock()
	if err := m.CheckOpen(); err != nil {
		return Result{Host: host, Command: command, Err: err}
	}
	if m.RunFn != nil {
		return m.RunFn(ctx, host, command)
	}
	return Result{Host: host, Command: command}
}

func (m *MockExecutor) Scp(ctx context.Context, host, src, dst string) error {
	m.mu.Lock()
	m.Calls = append(m.Calls, Call{Method: "Scp", Host: host, Src: src, Dst: dst})
	m.mu.Unlock()
	if err := m.CheckOpen(); err != nil {
		return err
	}
	if m.ScpFn != nil {
		return m.ScpFn(ctx, host, src, dst)
	}
	return nil
}

func (m *MockExecutor) Close() error { m.MarkClosed(); return nil }
