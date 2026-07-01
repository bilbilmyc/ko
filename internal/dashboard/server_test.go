package dashboard

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

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