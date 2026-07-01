package dashboard

import (
	"encoding/json"
	"net/http"
)

// API interfaces — concrete implementations live in cluster/, node lifecycle
// etc. The dashboard Server takes them as dependencies so it's testable
// without running an actual cluster.

type ClusterInfoAPI interface {
	Summary() (map[string]any, error)
}

type ClusterAPI interface {
	Cleanup() error
}

type NodeAPI interface {
	List() (string, error)
	Add(host, role string) error
	Remove(host string, force bool) error
}

type TuneAPI interface {
	Apply(profile string) error
}

type CertsAPI interface {
	ListCerts() ([]CertInfo, error)
}

// CertInfo is a JSON-friendly copy of cluster.CertificateInfo.
type CertInfo struct {
	Host     string `json:"host"`
	Path     string `json:"path"`
	NotAfter string `json:"not_after"`
	Subject  string `json:"subject"`
}

func (s *Server) handleClusterInfo(w http.ResponseWriter, _ *http.Request) {
	if s.cfg.ClusterInfo == nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "no cluster api wired"})
		return
	}
	out, err := s.cfg.ClusterInfo.Summary()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleNodes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, errMethodNotAllowed)
		return
	}
	if s.cfg.Node == nil {
		writeJSON(w, http.StatusOK, []string{})
		return
	}
	out, err := s.cfg.Node.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"output": out})
}

type addNodeReq struct {
	Host string `json:"host"`
	Role string `json:"role"` // "worker" | "master"
}

func (s *Server) handleNodeAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, errMethodNotAllowed)
		return
	}
	var req addNodeReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if s.cfg.Node == nil {
		writeErr(w, http.StatusServiceUnavailable, errAPINotWired)
		return
	}
	if err := s.cfg.Node.Add(req.Host, req.Role); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "host": req.Host})
}

type removeNodeReq struct {
	Host  string `json:"host"`
	Force bool   `json:"force"`
}

func (s *Server) handleNodeRemove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, errMethodNotAllowed)
		return
	}
	var req removeNodeReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if s.cfg.Node == nil {
		writeErr(w, http.StatusServiceUnavailable, errAPINotWired)
		return
	}
	if err := s.cfg.Node.Remove(req.Host, req.Force); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "host": req.Host})
}

type tuneReq struct {
	Profile string `json:"profile"`
}

func (s *Server) handleTuneApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, errMethodNotAllowed)
		return
	}
	var req tuneReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if s.cfg.Tune == nil {
		writeErr(w, http.StatusServiceUnavailable, errAPINotWired)
		return
	}
	if err := s.cfg.Tune.Apply(req.Profile); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "profile": req.Profile})
}

func (s *Server) handleCerts(w http.ResponseWriter, _ *http.Request) {
	if s.cfg.Certs == nil {
		writeJSON(w, http.StatusOK, []CertInfo{})
		return
	}
	out, err := s.cfg.Certs.ListCerts()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

var (
	errMethodNotAllowed = stringError("method not allowed")
	errAPINotWired      = stringError("API not wired")
)

type stringError string

func (e stringError) Error() string { return string(e) }