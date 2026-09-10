package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kula/internal/config"
	"kula/internal/sysinfo"
)

func TestSystemInfoRoute(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Web.UI = true
	cfg.Web.BasePath = "/kula"
	cfg.Web.OS = "Test OS"
	cfg.Web.Auth.Enabled = true
	cfg.Web.Auth.SessionTimeout = time.Hour
	cfg.Global.ShowSystemInfo = true
	s := NewServer(cfg.Web, cfg.Global, nil, nil, t.TempDir(), config.OllamaConfig{})
	handler := s.buildHandler()
	request := func(token, method string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/kula/api/system-info?from=2000-01-01T00:00:00Z", nil)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if w := request("", "GET"); w.Code != http.StatusUnauthorized {
		t.Fatalf("unprotected inventory: %d", w.Code)
	}
	token, err := s.auth.CreateSession("test")
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	w := request(token, "GET")
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("response: %d %s", w.Code, w.Body.String())
	}
	var info sysinfo.Snapshot
	if err := json.Unmarshal(w.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info.Timestamp.Before(before) || info.System["os"] != "Test OS" || info.Live != nil {
		t.Fatal("endpoint did not return current inventory independently of history and storage")
	}
	if w := request(token, "HEAD"); w.Code != http.StatusOK || w.Body.Len() != 0 {
		t.Fatal("HEAD returned a body")
	}
	s.global.ShowSystemInfo = false
	w = request(token, "GET")
	if w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "Test OS") {
		t.Fatal("disabled inventory exposed data")
	}
}

func TestSystemInfoMethods(t *testing.T) {
	s := &Server{global: config.GlobalConfig{ShowSystemInfo: true}}
	w := httptest.NewRecorder()
	s.handleSystemInfo(w, httptest.NewRequest("POST", "/api/system-info", nil))
	if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "GET, HEAD" {
		t.Fatal("unsupported method was accepted")
	}
}
