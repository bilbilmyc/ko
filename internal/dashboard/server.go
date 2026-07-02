// Package dashboard exposes the ko Web Dashboard: an HTTP server with
// basic-auth middleware and a small REST API for cluster / node / tune /
// certs operations.
package dashboard

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ko-build/ko/internal/logger"
)

// Config holds runtime settings for the HTTP server.
type Config struct {
	Listen      string
	User        string
	Password    string
	StaticDir   string
	Cluster     ClusterAPI
	Node        NodeAPI
	Tune        TuneAPI
	Certs       CertsAPI
	ClusterInfo ClusterInfoAPI
	Etcd        EtcdAPI
}

// Server wraps the http.Server + handler.
type Server struct {
	cfg Config
	httpSrv *http.Server
}

// New returns a server bound to cfg.Listen. Caller must Run() then Shutdown().
func New(cfg Config) *Server {
	mux := http.NewServeMux()
	s := &Server{cfg: cfg}
	s.routes(mux)
	s.httpSrv = &http.Server{
		Addr:              cfg.Listen,
		Handler:           s.recoverer(s.basicAuth(mux)),
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s
}

// Run blocks until the server stops.
func (s *Server) Run() error {
	logger.Info("dashboard listening", "addr", s.cfg.Listen)
	if err := s.httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpSrv.Shutdown(ctx)
}

func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("/api/cluster/info", s.handleClusterInfo)
	mux.HandleFunc("/api/nodes", s.handleNodes)
	mux.HandleFunc("/api/nodes/add", s.handleNodeAdd)
	mux.HandleFunc("/api/nodes/remove", s.handleNodeRemove)
	mux.HandleFunc("/api/tune/apply", s.handleTuneApply)
	mux.HandleFunc("/api/certs", s.handleCerts)
	mux.HandleFunc("/api/etcd/status", s.handleEtcdStatus)
	mux.HandleFunc("/api/etcd/backups", s.handleEtcdBackups)
	mux.HandleFunc("/api/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	if s.cfg.StaticDir != "" {
		mux.Handle("/", http.FileServer(http.Dir(s.cfg.StaticDir)))
	} else {
		mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, defaultHTML)
		})
	}
}

// basicAuth gates every request with HTTP Basic Auth.
func (s *Server) basicAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok ||
			subtle.ConstantTimeCompare([]byte(u), []byte(s.cfg.User)) != 1 ||
			subtle.ConstantTimeCompare([]byte(p), []byte(s.cfg.Password)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="ko"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// recoverer turns panics into 500s instead of dropping the connection.
func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				logger.Error("dashboard panic", "err", rec, "path", r.URL.Path)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// defaultHTML is the embedded landing page when no static dir is configured.
const defaultHTML = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>ko dashboard</title>
<style>
body { font-family: -apple-system, system-ui, sans-serif; max-width: 720px; margin: 40px auto; padding: 0 16px; }
h1 { font-size: 24px; }
code { background: #f4f4f4; padding: 2px 6px; border-radius: 4px; }
ul { line-height: 1.7; }
</style></head><body>
<h1>ko dashboard</h1>
<p>This is a minimal ko dashboard. For the full React UI, place a built
   frontend under <code>--static-dir</code>.</p>
<h2>API</h2>
<ul>
  <li><code>GET /api/healthz</code></li>
  <li><code>GET /api/cluster/info</code></li>
  <li><code>GET /api/nodes</code></li>
  <li><code>POST /api/nodes/add</code>  body: {"host":"w1","role":"worker"}</li>
  <li><code>POST /api/nodes/remove</code> body: {"host":"w1","force":false}</li>
  <li><code>POST /api/tune/apply</code>  body: {"profile":"production"}</li>
  <li><code>GET /api/certs</code></li>
  <li><code>GET /api/etcd/status</code></li>
  <li><code>GET /api/etcd/backups</code></li>
</ul>
</body></html>`

// trimPath is a placeholder for future path-based logging; currently unused
// but kept to make the public API stable across refactors.
var _ = trimPath

func trimPath(p string) string {
	if p, _, ok := strings.Cut(p, "?"); ok {
		return p
	}
	return p
}