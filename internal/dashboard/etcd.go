package dashboard

import (
	"net/http"
	"time"
)

// EtcdStatus is the JSON shape returned by GET /api/etcd/status.
type EtcdStatus struct {
	Mode      string         `json:"mode"` // "external" | "stacked" | "n/a"
	Members   []EtcdMember   `json:"members,omitempty"`
	Backups   []EtcdBackup   `json:"backups,omitempty"`
	GeneratedAt time.Time    `json:"generated_at"`
}

// EtcdMember is the JSON shape for one member row in /api/etcd/status.
type EtcdMember struct {
	Name           string `json:"name"`
	Host           string `json:"host"`
	Active         string `json:"active"`
	EndpointHealth string `json:"endpoint_health"`
}

// EtcdBackup is the JSON shape for one backup row in /api/etcd/backups.
type EtcdBackup struct {
	Host     string    `json:"host"`
	Name     string    `json:"name"`
	Filename string    `json:"filename"`
	Size     int64     `json:"size"`
	ModTime  time.Time `json:"mod_time"`
}

// EtcdAPI is the dependency the dashboard needs to surface etcd state.
// Implementations live in internal/cli/etcd_dashboard.go.
type EtcdAPI interface {
	// Status returns member + health info, or an error if etcd is not in
	// external mode (status then returns nil + a sentinel so the handler
	// can render a "not configured" message).
	Status() (*EtcdStatus, error)
	// ListBackups returns all backups across all members, sorted by ModTime desc.
	ListBackups() ([]EtcdBackup, error)
}

func (s *Server) handleEtcdStatus(w http.ResponseWriter, _ *http.Request) {
	if s.cfg.Etcd == nil {
		writeJSON(w, http.StatusOK, EtcdStatus{Mode: "n/a", GeneratedAt: time.Now().UTC()})
		return
	}
	st, err := s.cfg.Etcd.Status()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if st == nil {
		writeJSON(w, http.StatusOK, EtcdStatus{Mode: "stacked", GeneratedAt: time.Now().UTC()})
		return
	}
	st.GeneratedAt = time.Now().UTC()
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleEtcdBackups(w http.ResponseWriter, _ *http.Request) {
	if s.cfg.Etcd == nil {
		writeJSON(w, http.StatusOK, []EtcdBackup{})
		return
	}
	out, err := s.cfg.Etcd.ListBackups()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
