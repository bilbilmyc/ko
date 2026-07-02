package cluster

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"time"

	execx "github.com/ko-build/ko/internal/exec"
)

type LocalExecutor struct {
	execx.Base
	workdir string
}

func NewLocalExecutor() *LocalExecutor {
	wd, _ := os.Getwd()
	return &LocalExecutor{workdir: wd}
}

func (l *LocalExecutor) Run(ctx context.Context, host, command string) execx.Result {
	res := execx.Result{Host: host, Command: command}
	if err := l.CheckOpen(); err != nil {
		res.Err = err
		return res
	}
	if host != "" && host != "localhost" && host != "127.0.0.1" {
		res.Err = fmt.Errorf("local executor cannot reach host %q", host)
		return res
	}

	timeout := execx.DefaultTimeout
	if deadline, ok := ctx.Deadline(); ok {
		if d := time.Until(deadline); d > 0 && d < timeout {
			timeout = d
		}
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(cctx, "cmd", "/C", command)
	} else {
		cmd = exec.CommandContext(cctx, "sh", "-c", command)
	}
	cmd.Dir = l.workdir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		res.Err = fmt.Errorf("run: %w (stderr: %s)", err, stderr.String())
	}
	res.Stdout = stdout.Bytes()
	res.Stderr = stderr.Bytes()
	return res
}

func (l *LocalExecutor) Scp(ctx context.Context, host, src, dst string) error {
	if err := l.CheckOpen(); err != nil {
		return err
	}
	if host != "" && host != "localhost" && host != "127.0.0.1" {
		return fmt.Errorf("local executor cannot scp to host %q", host)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("read src %q: %w", src, err)
	}
	if err := os.MkdirAll(parentDir(dst), 0o755); err != nil {
		return fmt.Errorf("mkdir parent of %q: %w", dst, err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return fmt.Errorf("write dst %q: %w", dst, err)
	}
	return nil
}

func (l *LocalExecutor) Close() error { l.MarkClosed(); return nil }

func parentDir(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' || p[i] == '\\' {
			return p[:i]
		}
	}
	return "."
}
