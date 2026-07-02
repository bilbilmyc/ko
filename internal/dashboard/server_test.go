package dashboard

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubCluster struct{ summary map[string]any }

func (s *stubCluster) Summary() (map[string]any, error) { return s.summary, nil }

type stubNodes struct {
	list   string
	addOK  bool
	rmErr  error
	rmFail bool
}

func (n *stubNodes) List() (string, error) { return n.list, nil }
func (n *stubNodes) Add(host, role string) error {
	if !n.addOK {
		return io.EOF
	}
	return nil
}
func (n *stubNodes) Remove(host string, force bool) error {
	if n.rmFail {
		return n.rmErr
	}
	return nil
}

func newTestServer() *Server {
	return New(Config{
		Listen:   ":0",
		User:     "admin",
		Password: "secret",
		ClusterInfo: &stubCluster{summary: map[string]any{"name": "ko", "version": "1.35"}},
		Node:        &stubNodes{list: "NAME STATUS\nm1 Ready", addOK: true, rmFail: true, rmErr: io.EOF},
	})
}

func TestBasicAuth_RequiresCreds(t *testing.T) {
	s := newTestServer()
	ts := httptest.NewServer(s.basicAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.Equal(t, `Basic realm="ko"`, resp.Header.Get("WWW-Authenticate"))
}

func TestBasicAuth_AcceptsCreds(t *testing.T) {
	s := newTestServer()
	ts := httptest.NewServer(s.basicAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
	req.SetBasicAuth("admin", "secret")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestBasicAuth_RejectsBadCreds(t *testing.T) {
	s := newTestServer()
	ts := httptest.NewServer(s.basicAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
	req.SetBasicAuth("admin", "wrong")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestHealthz_NoAuth_Requires401OutsideServer(t *testing.T) {
	// /api/healthz is gated by basic auth in our middleware.
	s := newTestServer()
	ts := httptest.NewServer(s.httpSrv.Handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/healthz")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestClusterInfo_Returns(t *testing.T) {
	s := newTestServer()
	ts := httptest.NewServer(s.httpSrv.Handler)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/cluster/info", nil)
	req.SetBasicAuth("admin", "secret")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var got map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	assert.Equal(t, "ko", got["name"])
}

func TestNodeAdd_OK(t *testing.T) {
	s := newTestServer()
	ts := httptest.NewServer(s.httpSrv.Handler)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/nodes/add",
		stringReader(`{"host":"w1","role":"worker"}`))
	req.SetBasicAuth("admin", "secret")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestNodeRemove_BadHost(t *testing.T) {
	s := newTestServer()
	ts := httptest.NewServer(s.httpSrv.Handler)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/nodes/remove",
		stringReader(`{"host":"unknown"}`))
	req.SetBasicAuth("admin", "secret")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
}

func stringReader(s string) io.Reader { return ioReaderFunc(func(p []byte) (int, error) {
	return copy(p, s), io.EOF
}) }

type ioReaderFunc func([]byte) (int, error)

func (f ioReaderFunc) Read(p []byte) (int, error) { return f(p) }

func TestRateLimit_BurstThen429(t *testing.T) {
	// Very slow refill (1 req / 1000s) so the burst is the only thing
	// that gets us through. Burst=2 → first two requests OK, third 429.
	s := New(Config{
		Listen:   ":0",
		User:     "admin",
		Password: "secret",
		// 0.001 tokens/sec, burst 2 → 2 free then 429 until refill
		RateLimit: 0.001,
		RateBurst: 2,
	})
	handler := s.rateLimit(s.basicAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))
	ts := httptest.NewServer(handler)
	defer ts.Close()

	for i := 0; i < 2; i++ {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
		req.SetBasicAuth("admin", "secret")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode, "burst request %d should pass", i)
	}

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
	req.SetBasicAuth("admin", "secret")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	assert.Equal(t, "1", resp.Header.Get("Retry-After"))
}

func TestRateLimit_Disabled(t *testing.T) {
	s := New(Config{
		Listen:    ":0",
		User:      "admin",
		Password:  "secret",
		RateLimit: 0, // disabled
	})
	// When disabled, rateLimit must be a pass-through (same handler).
	handler := s.rateLimit(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	ts := httptest.NewServer(handler)
	defer ts.Close()

	// Send way more than any reasonable burst; none should be limited.
	for i := 0; i < 100; i++ {
		resp, err := http.Get(ts.URL + "/")
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode, "request %d", i)
	}
}

func TestAudit_Disabled_UsesDiscard(t *testing.T) {
	s := New(Config{
		Listen:   ":0",
		User:     "admin",
		Password: "secret",
		AuditLog: "", // disabled
	})
	assert.Equal(t, io.Discard, s.auditOut)

	ts := httptest.NewServer(s.httpSrv.Handler)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/healthz", nil)
	req.SetBasicAuth("admin", "secret")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestAudit_RecordsSuccessfulRequest(t *testing.T) {
	dir := t.TempDir()
	auditPath := filepath.Join(dir, "audit.log")
	s := New(Config{
		Listen:   ":0",
		User:     "admin",
		Password: "secret",
		ClusterInfo: &stubCluster{summary: map[string]any{"name": "ko"}},
		AuditLog: auditPath,
	})

	ts := httptest.NewServer(s.httpSrv.Handler)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/healthz", nil)
	req.SetBasicAuth("admin", "secret")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = s.Shutdown(shutdownCtx)

	data, err := os.ReadFile(auditPath)
	require.NoError(t, err)
	line := string(data)
	assert.Contains(t, line, "user=admin")
	assert.Contains(t, line, "method=GET")
	assert.Contains(t, line, "path=/api/healthz")
	assert.Contains(t, line, "status=200")
	assert.Contains(t, line, "remote=")
	// First non-comment line in the file should be the audit record.
	assert.True(t, strings.HasPrefix(strings.TrimSpace(line), "20"),
		"audit line must start with RFC3339Nano timestamp, got %q", line)
}

func TestAudit_Records401WithDashUser(t *testing.T) {
	dir := t.TempDir()
	auditPath := filepath.Join(dir, "audit.log")
	s := New(Config{
		Listen:    ":0",
		User:      "admin",
		Password:  "secret",
		AuditLog:  auditPath,
		RateLimit: 0, // don't subject 401 test to rate limit noise
	})

	ts := httptest.NewServer(s.httpSrv.Handler)
	defer ts.Close()

	// No creds → basicAuth 401 → audit must still record (with user=-).
	resp, err := http.Get(ts.URL + "/api/healthz")
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = s.Shutdown(shutdownCtx)

	data, err := os.ReadFile(auditPath)
	require.NoError(t, err)
	line := string(data)
	assert.Contains(t, line, "user=-")
	assert.Contains(t, line, "status=401")
	assert.Contains(t, line, "path=/api/healthz")
}

func TestAudit_RecordsRateLimited429(t *testing.T) {
	dir := t.TempDir()
	auditPath := filepath.Join(dir, "audit.log")
	s := New(Config{
		Listen:    ":0",
		User:      "admin",
		Password:  "secret",
		AuditLog:  auditPath,
		RateLimit: 0.001,
		RateBurst: 1,
	})

	ts := httptest.NewServer(s.httpSrv.Handler)
	defer ts.Close()

	// First request burns the burst token.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/healthz", nil)
	req.SetBasicAuth("admin", "secret")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Second request gets 429 from rateLimit (which sits between basicAuth
	// and the handler — so it still goes through basicAuth first).
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/api/healthz", nil)
	req.SetBasicAuth("admin", "secret")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = s.Shutdown(shutdownCtx)

	data, err := os.ReadFile(auditPath)
	require.NoError(t, err)
	content := string(data)
	assert.Contains(t, content, "status=200")
	assert.Contains(t, content, "status=429")
}

func TestAuditingResponseWriter_CapturesStatus(t *testing.T) {
	// Direct unit test on the wrapper — no http server needed.
	rec := httptest.NewRecorder()
	aw := &auditingResponseWriter{ResponseWriter: rec}

	aw.WriteHeader(http.StatusTeapot)
	_, err := aw.Write([]byte("hello"))
	require.NoError(t, err)

	assert.Equal(t, http.StatusTeapot, aw.status)
	assert.Equal(t, 5, aw.bytes)
	assert.Equal(t, http.StatusTeapot, rec.Code)
	assert.Equal(t, "hello", rec.Body.String())
}