package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kula/internal/collector"
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

// TestSystemInfoDetailsOption covers global.show_system_details: the endpoint
// keeps answering with host identity, CPU and live metrics, but the storage,
// network, connected-device and sensor inventory is left out unless the option
// is enabled.
func TestSystemInfoDetailsOption(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Web.UI = true
	cfg.Web.OS = "Test OS"
	cfg.Web.Kernel = "Test Kernel"
	cfg.Web.Arch = "amd64"
	cfg.Global.ShowSystemInfo = true
	s := NewServer(cfg.Web, cfg.Global, nil, nil, t.TempDir(), config.OllamaConfig{})
	handler := s.buildHandler()
	token, err := s.auth.CreateSession("test")
	if err != nil {
		t.Fatal(err)
	}
	fetch := func() (sysinfo.Snapshot, map[string]json.RawMessage) {
		t.Helper()
		r := httptest.NewRequest("GET", "/api/system-info", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("response: %d %s", w.Code, w.Body.String())
		}
		var info sysinfo.Snapshot
		if err := json.Unmarshal(w.Body.Bytes(), &info); err != nil {
			t.Fatal(err)
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
			t.Fatal(err)
		}
		return info, raw
	}
	summary, raw := fetch()
	if summary.System["os"] != "Test OS" || summary.System["kernel"] != "Test Kernel" ||
		summary.System["architecture"] != "amd64" {
		t.Fatalf("default response lost the host identity: %+v", summary.System)
	}
	if len(summary.Disks) != 0 || len(summary.Filesystems) != 0 || len(summary.Network) != 0 ||
		len(summary.PCI) != 0 || len(summary.USB) != 0 || len(summary.Sensors) != 0 || len(summary.Power) != 0 {
		t.Fatalf("default response exposed host details: %+v", summary)
	}
	// Hidden sections keep the array shape of a full snapshot so existing
	// clients can iterate them without a null check.
	for _, section := range []string{"disks", "filesystems", "network", "pci", "usb", "sensors", "power"} {
		if string(raw[section]) != "[]" {
			t.Fatalf("hidden %s is %s, want []", section, raw[section])
		}
	}
	if summary.Live != nil && summary.Live.Hottest != nil {
		t.Fatal("default response exposed the warmest sensor")
	}
	s.global.ShowSystemDetails = true
	detailed, _ := fetch()
	if len(detailed.Network) == 0 && len(detailed.Filesystems) == 0 {
		t.Fatalf("enabled details returned no inventory: %+v", detailed)
	}
}

func TestConfigReportsSystemDetailOption(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Web.UI = true
	cfg.Web.Auth.Enabled = true
	cfg.Global.ShowSystemDetails = true
	c := collector.New(config.GlobalConfig{}, config.CollectionConfig{}, config.ApplicationsConfig{}, t.TempDir())
	s := NewServer(cfg.Web, cfg.Global, c, nil, t.TempDir(), config.OllamaConfig{})
	w := httptest.NewRecorder()
	s.handleConfig(w, httptest.NewRequest("GET", "/api/config", nil))
	var payload map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["show_system_details"] != true {
		t.Fatalf("/api/config does not report show_system_details: %v", payload["show_system_details"])
	}
}
