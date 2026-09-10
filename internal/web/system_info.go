package web

import (
	"encoding/json"
	"net/http"

	"kula/internal/collector"
)

func (s *Server) handleSystemInfo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.global.ShowSystemInfo {
		jsonError(w, "System information is disabled", http.StatusNotFound)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		jsonError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodHead {
		return
	}
	var latest *collector.Sample
	if s.collector != nil {
		latest = s.collector.Latest()
	}
	info := s.systemInfo.Current(latest, s.cfg.OS, s.cfg.Kernel, s.cfg.Arch, s.global.Hostname)
	_ = json.NewEncoder(w).Encode(info)
}
