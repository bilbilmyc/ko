// Package dashboard exposes the ko Web Dashboard: an HTTP server with
// basic-auth middleware and a small REST API for cluster / node / tune /
// certs operations.
package dashboard

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"golang.org/x/time/rate"

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

	// RateLimit gates incoming requests with a token bucket. 0 disables
	// rate limiting (use for local-only deployments). Default: 1.0 req/s.
	RateLimit rate.Limit
	// RateBurst is the max number of requests allowed in a single burst.
	// 0 picks a default of 20.
	RateBurst int

	// AuditLog is the path to the append-only audit log file. Empty
	// disables audit logging. The file is opened with mode 0600 in
	// append-create mode on New(); failures are logged but do not block
	// the server (audit is best-effort).
	AuditLog string
}

// Server wraps the http.Server + handler.
type Server struct {
	cfg Config
	httpSrv *http.Server

	// auditOut is the sink audit lines are written to. It's either an
	// *os.File (when AuditLog is configured and openable) or io.Discard.
	auditOut io.Writer
	auditMu  sync.Mutex // serialize WriteString calls; the underlying file is shared
}

// New returns a server bound to cfg.Listen. Caller must Run() then Shutdown().
func New(cfg Config) *Server {
	s := &Server{cfg: cfg, auditOut: io.Discard}

	if cfg.AuditLog != "" {
		f, err := os.OpenFile(cfg.AuditLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			logger.Error("dashboard audit log open failed; auditing disabled",
				"path", cfg.AuditLog, "err", err)
		} else {
			s.auditOut = f
		}
	}

	mux := http.NewServeMux()
	s.routes(mux)

	// Middleware chain (outermost first):
	//   recoverer → audit → rateLimit → basicAuth → mux
	//
	// Order rationale:
	//   - recoverer outermost so panic-recovered 500s are still audited.
	//   - audit next so it observes the final status written by rateLimit
	//     (429) and basicAuth (401) rejections without the overhead of
	//     re-reading the wrapped ResponseWriter.
	//   - rateLimit before basicAuth so a brute-force scanner can't burn
	//     CPU on constant-time compares once it's already past the limiter.
	s.httpSrv = &http.Server{
		Addr:              cfg.Listen,
		Handler:           s.recoverer(s.audit(s.rateLimit(s.basicAuth(mux)))),
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s
}

// Run blocks until the server stops.
func (s *Server) Run() error {
	logger.Info("dashboard listening",
		"addr", s.cfg.Listen,
		"rate_limit", fmt.Sprint(s.cfg.RateLimit),
		"rate_burst", s.cfg.RateBurst,
		"audit_log", s.cfg.AuditLog,
	)
	if err := s.httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// Shutdown gracefully stops the server and closes the audit log file (if any).
func (s *Server) Shutdown(ctx context.Context) error {
	err := s.httpSrv.Shutdown(ctx)
	if f, ok := s.auditOut.(*os.File); ok {
		_ = f.Close()
	}
	return err
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

// rateLimit enforces a global token bucket. Disabled when RateLimit == 0.
// We use a single bucket (not per-IP) to keep memory bounded; the default
// of 1 req/s with burst 20 is loose enough for an ops dashboard shared by
// a small team. Use rate=0 to disable.
func (s *Server) rateLimit(next http.Handler) http.Handler {
	if s.cfg.RateLimit <= 0 {
		return next
	}
	burst := s.cfg.RateBurst
	if burst <= 0 {
		burst = 20
	}
	lim := rate.NewLimiter(s.cfg.RateLimit, burst)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !lim.Allow() {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// auditingResponseWriter captures the response status and byte count so
// the audit middleware can record what actually happened.
type auditingResponseWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (a *auditingResponseWriter) WriteHeader(code int) {
	if a.status == 0 {
		a.status = code
	}
	a.ResponseWriter.WriteHeader(code)
}

func (a *auditingResponseWriter) Write(p []byte) (int, error) {
	if a.status == 0 {
		a.status = http.StatusOK
	}
	n, err := a.ResponseWriter.Write(p)
	a.bytes += n
	return n, err
}

// audit logs one line per request. The "user" is the BasicAuth user (or
// "-" for unauthenticated requests). Failures to open the audit log are
// non-fatal (logged at server start); failures to write a single line are
// logged but do not block the response.
func (s *Server) audit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		user, _, _ := r.BasicAuth()
		if user == "" {
			user = "-"
		}
		aw := &auditingResponseWriter{ResponseWriter: w}
		next.ServeHTTP(aw, r)

		status := aw.status
		if status == 0 {
			status = http.StatusOK
		}

		line := fmt.Sprintf("%s remote=%s user=%s method=%s path=%s status=%d bytes=%d dur=%s\n",
			time.Now().UTC().Format(time.RFC3339Nano),
			r.RemoteAddr,
			user,
			r.Method,
			r.URL.Path,
			status,
			aw.bytes,
			time.Since(start).Round(time.Microsecond),
		)
		s.auditMu.Lock()
		_, err := s.auditOut.Write([]byte(line))
		s.auditMu.Unlock()
		if err != nil {
			logger.Error("dashboard audit write failed", "err", err, slog.Any("remote", r.RemoteAddr))
		}
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