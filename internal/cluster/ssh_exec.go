package cluster

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

type SSHConfig struct {
	User       string
	Port       int
	KeyFile    string
	Password   string
	Timeout    time.Duration
	KnownHosts string
}

func (c SSHConfig) withDefaults() SSHConfig {
	if c.Port == 0 {
		c.Port = 22
	}
	if c.Timeout == 0 {
		c.Timeout = DefaultTimeout
	}
	if c.User == "" {
		c.User = "root"
	}
	return c
}

type SSHExecutor struct {
	*baseExecutor
	config  SSHConfig
	clients sync.Map // host -> *ssh.Client
	dialFn  func(host string, config *ssh.ClientConfig) (*ssh.Client, error)
}

func NewSSHExecutor(cfg SSHConfig) (*SSHExecutor, error) {
	cfg = cfg.withDefaults()
	e := &SSHExecutor{
		baseExecutor: &baseExecutor{},
		config:       cfg,
	}
	if _, err := e.authMethods(); err != nil {
		return nil, err
	}
	if e.dialFn == nil {
		e.dialFn = e.defaultDial
	}
	return e, nil
}

func (s *SSHExecutor) defaultDial(host string, cfg *ssh.ClientConfig) (*ssh.Client, error) {
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", s.config.Port))
	return ssh.Dial("tcp", addr, cfg)
}

func (s *SSHExecutor) clientConfig() (*ssh.ClientConfig, error) {
	authMethods, err := s.authMethods()
	if err != nil {
		return nil, err
	}
	cfg := &ssh.ClientConfig{
		User:            s.config.User,
		Auth:            authMethods,
		Timeout:         s.config.Timeout,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}
	if s.config.KnownHosts != "" {
		callback, err := knownhosts.New(s.config.KnownHosts)
		if err != nil {
			return nil, fmt.Errorf("load known_hosts %q: %w", s.config.KnownHosts, err)
		}
		cfg.HostKeyCallback = callback
	}
	return cfg, nil
}

func (s *SSHExecutor) authMethods() ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod
	if s.config.KeyFile != "" {
		expanded, err := expandPath(s.config.KeyFile)
		if err != nil {
			return nil, err
		}
		key, err := os.ReadFile(expanded)
		if err != nil {
			return nil, fmt.Errorf("read ssh key %q: %w", expanded, err)
		}
		signer, err := ssh.ParsePrivateKey(key)
		if err != nil {
			return nil, fmt.Errorf("parse ssh key: %w", err)
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}
	if s.config.Password != "" {
		methods = append(methods, ssh.Password(s.config.Password))
	}
	if len(methods) == 0 {
		return nil, errors.New("no ssh auth method configured (set KeyFile or Password)")
	}
	return methods, nil
}

func (s *SSHExecutor) getClient(ctx context.Context, host string) (*ssh.Client, error) {
	if v, ok := s.clients.Load(host); ok {
		c := v.(*ssh.Client)
		_, _, err := c.SendRequest("keepalive@ko", true, nil)
		if err == nil {
			return c, nil
		}
		_ = c.Close()
		s.clients.Delete(host)
	}

	cfg, err := s.clientConfig()
	if err != nil {
		return nil, err
	}

	dialer := &net.Dialer{Timeout: s.config.Timeout}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, fmt.Sprintf("%d", s.config.Port)))
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", host, err)
	}

	ncc, chans, reqs, err := ssh.NewClientConn(conn, net.JoinHostPort(host, fmt.Sprintf("%d", s.config.Port)), cfg)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ssh handshake %s: %w", host, err)
	}
	client := ssh.NewClient(ncc, chans, reqs)
	s.clients.Store(host, client)
	return client, nil
}

func (s *SSHExecutor) Run(ctx context.Context, host, command string) Result {
	res := Result{Host: host, Command: command}
	if err := s.checkOpen(); err != nil {
		res.Err = err
		return res
	}
	client, err := s.getClient(ctx, host)
	if err != nil {
		res.Err = err
		return res
	}
	sess, err := client.NewSession()
	if err != nil {
		res.Err = fmt.Errorf("new session: %w", err)
		return res
	}
	defer sess.Close()

	var stdout, stderr bytes.Buffer
	sess.Stdout = &stdout
	sess.Stderr = &stderr

	type runResult struct {
		err error
	}
	done := make(chan runResult, 1)
	go func() {
		done <- runResult{err: sess.Run(command)}
	}()

	select {
	case r := <-done:
		res.Stdout = stdout.Bytes()
		res.Stderr = stderr.Bytes()
		if r.err != nil {
			res.Err = fmt.Errorf("ssh run: %w (stderr: %s)", r.err, stderr.String())
		}
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGKILL)
		res.Err = fmt.Errorf("ssh run timeout: %w", ctx.Err())
	}
	return res
}

func (s *SSHExecutor) Scp(ctx context.Context, host, src, dst string) error {
	if err := s.checkOpen(); err != nil {
		return err
	}
	client, err := s.getClient(ctx, host)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("read src %q: %w", src, err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("mkdir parent of %q: %w", dst, err)
	}
	sess, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("new session: %w", err)
	}
	defer sess.Close()
	go func() {
		_, _ = sess.StdinPipe()
	}()
	w, err := sess.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	cmd := fmt.Sprintf("cat > %s && chmod 0644 %s", shellQuote(dst), shellQuote(dst))
	if err := sess.Start(cmd); err != nil {
		return fmt.Errorf("start remote write: %w", err)
	}
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("write to remote: %w", err)
	}
	_ = w.Close()
	if err := sess.Wait(); err != nil {
		return fmt.Errorf("remote write: %w", err)
	}
	return nil
}

func (s *SSHExecutor) Close() error {
	s.markClosed()
	s.clients.Range(func(k, v any) bool {
		_ = v.(*ssh.Client).Close()
		s.clients.Delete(k)
		return true
	})
	return nil
}

func expandPath(p string) (string, error) {
	if len(p) == 0 || p[0] != '~' {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, p[1:]), nil
}

func shellQuote(s string) string {
	out := `"`
	for _, r := range s {
		if r == '"' || r == '\\' || r == '$' || r == '`' {
			out += `\`
		}
		out += string(r)
	}
	return out + `"`
}
