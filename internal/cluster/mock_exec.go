package cluster

import (
	"context"
	"sync"
)

type MockExecutor struct {
	*baseExecutor
	mu sync.Mutex

	RunFn  func(ctx context.Context, host, command string) Result
	ScpFn  func(ctx context.Context, host, src, dst string) error

	Calls  []MockCall
}

type MockCall struct {
	Method  string
	Host    string
	Command string
	Src     string
	Dst     string
}

func NewMockExecutor() *MockExecutor {
	return &MockExecutor{baseExecutor: &baseExecutor{}}
}

func (m *MockExecutor) Run(ctx context.Context, host, command string) Result {
	m.mu.Lock()
	m.Calls = append(m.Calls, MockCall{Method: "Run", Host: host, Command: command})
	m.mu.Unlock()
	if err := m.checkOpen(); err != nil {
		return Result{Host: host, Command: command, Err: err}
	}
	if m.RunFn != nil {
		return m.RunFn(ctx, host, command)
	}
	return Result{Host: host, Command: command}
}

func (m *MockExecutor) Scp(ctx context.Context, host, src, dst string) error {
	m.mu.Lock()
	m.Calls = append(m.Calls, MockCall{Method: "Scp", Host: host, Src: src, Dst: dst})
	m.mu.Unlock()
	if err := m.checkOpen(); err != nil {
		return err
	}
	if m.ScpFn != nil {
		return m.ScpFn(ctx, host, src, dst)
	}
	return nil
}

func (m *MockExecutor) Close() error {
	m.markClosed()
	return nil
}
