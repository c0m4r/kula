package web

import (
	"bytes"
	"encoding/json"
	"html"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"kula/internal/collector"
	"kula/internal/config"
	"kula/internal/storage"
)

func TestTemplateInjection(t *testing.T) {
	s := NewServer(config.WebConfig{Security: config.SecurityConfig{Headers: true, OriginValidation: true}}, config.GlobalConfig{}, nil, nil, t.TempDir(), config.OllamaConfig{})

	// Create a recorder to capture the response
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)

	// Wrap with securityMiddleware to get the nonce
	handler := s.securityMiddleware(http.HandlerFunc(s.handleIndex))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("Expected status 200, got %d", rec.Code)
	}

	body := html.UnescapeString(rec.Body.String())

	// Verify nonce is in CSP header
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "nonce-") {
		t.Errorf("CSP header missing nonce: %s", csp)
	}

	// Extract nonce from CSP
	parts := strings.Split(csp, "'nonce-")
	if len(parts) < 2 {
		t.Fatalf("Could not parse nonce from CSP: %s", csp)
	}
	nonce := strings.Split(parts[1], "'")[0]

	// Verify nonce is injected into HTML
	if !strings.Contains(body, `nonce="`+nonce+`"`) {
		t.Errorf("HTML body missing injected nonce %s", nonce)
	}

	// Verify SRI is injected into HTML
	sri := s.sriHashes["js/app/main.js"]
	if sri == "" {
		t.Error("SRI hash for js/app/main.js is empty in server")
	}
	if !strings.Contains(body, `integrity="`+sri+`"`) {
		t.Errorf("HTML body missing injected SRI %s", sri)
	}
}

func TestGameTemplateInjection(t *testing.T) {
	s := NewServer(config.WebConfig{Security: config.SecurityConfig{Headers: true, OriginValidation: true}}, config.GlobalConfig{}, nil, nil, t.TempDir(), config.OllamaConfig{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/game.html", nil)

	handler := s.securityMiddleware(http.HandlerFunc(s.handleGame))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("Expected status 200, got %d", rec.Code)
	}

	body := html.UnescapeString(rec.Body.String())

	// Verify SRI for game.js
	sri := s.sriHashes["game.js"]
	if sri == "" {
		t.Error("SRI hash for game.js is empty in server")
	}
	if !strings.Contains(body, `integrity="`+sri+`"`) {
		t.Errorf("Game HTML body missing injected SRI %s", sri)
	}
}

func TestCreateUnixListener(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "kula.sock")

	ln, err := createUnixListener(sock, "0660")
	if err != nil {
		t.Fatalf("createUnixListener: %v", err)
	}
	defer func() { _ = ln.Close() }()

	if ln.Addr().Network() != "unix" {
		t.Fatalf("expected unix network, got %s", ln.Addr().Network())
	}

	info, err := os.Stat(sock)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("path is not a socket")
	}
	if perm := info.Mode().Perm(); perm != 0660 {
		t.Fatalf("expected mode 0660, got %#o", perm)
	}
}

func TestCreateUnixListenerInvalidMode(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "kula.sock")

	if _, err := createUnixListener(sock, "not-octal"); err == nil {
		t.Fatalf("expected error for invalid mode")
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatalf("socket file should not be left behind after mode error, stat err=%v", err)
	}
}

func TestCreateUnixListenerRequiresAbsolute(t *testing.T) {
	if _, err := createUnixListener("relative.sock", "0660"); err == nil {
		t.Fatalf("expected error for relative path")
	}
}

func TestCreateUnixListenerRemovesStale(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "kula.sock")

	// Create a stale socket file (no listener attached).
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("seed listen: %v", err)
	}
	_ = ln.Close()
	// Close removes the file on most platforms; recreate to simulate a stale leftover.
	if _, err := os.Stat(sock); os.IsNotExist(err) {
		f, err := os.Create(sock + ".tmp")
		if err != nil {
			t.Fatalf("create tmp: %v", err)
		}
		_ = f.Close()
		// Bind a fresh listener and immediately close it without removing.
		ln2, err := net.Listen("unix", sock)
		if err != nil {
			t.Fatalf("seed listen 2: %v", err)
		}
		// Disable unlink-on-close so the file persists as a stale socket.
		if ul, ok := ln2.(*net.UnixListener); ok {
			ul.SetUnlinkOnClose(false)
		}
		_ = ln2.Close()
	}

	ln3, err := createUnixListener(sock, "0660")
	if err != nil {
		t.Fatalf("createUnixListener over stale: %v", err)
	}
	_ = ln3.Close()
}

func TestCreateUnixListenerRefusesLive(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "kula.sock")

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("seed listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	if _, err := createUnixListener(sock, "0660"); err == nil {
		t.Fatalf("expected error when another process is listening")
	}
}

func TestMountWithBasePath(t *testing.T) {
	hit := func(path string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != path {
				t.Errorf("inner saw URL.Path = %q, want %q", r.URL.Path, path)
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok:" + path))
		}
	}

	inner := http.NewServeMux()
	inner.HandleFunc("/api/current", hit("/api/current"))
	inner.HandleFunc("/health", hit("/health"))
	inner.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("root:" + r.URL.Path))
	})

	t.Run("empty base path is pass-through", func(t *testing.T) {
		h := mountWithBasePath(inner, "")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/current", nil))
		if rec.Code != http.StatusOK || rec.Body.String() != "ok:/api/current" {
			t.Fatalf("got code=%d body=%q", rec.Code, rec.Body.String())
		}
	})

	t.Run("with base path: routed via prefix", func(t *testing.T) {
		h := mountWithBasePath(inner, "/kula")
		for _, tc := range []struct {
			path, want string
		}{
			{"/kula/api/current", "ok:/api/current"},
			{"/kula/health", "ok:/health"},
			{"/kula/", "root:/"},
		} {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if rec.Code != http.StatusOK {
				t.Errorf("%s: code=%d body=%q", tc.path, rec.Code, rec.Body.String())
				continue
			}
			if rec.Body.String() != tc.want {
				t.Errorf("%s: body=%q want %q", tc.path, rec.Body.String(), tc.want)
			}
		}
	})

	t.Run("with base path: root paths return 404", func(t *testing.T) {
		h := mountWithBasePath(inner, "/kula")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/current", nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("expected 404 for unprefixed request, got %d", rec.Code)
		}
	})

	t.Run("with base path: bare prefix redirects to prefix/", func(t *testing.T) {
		h := mountWithBasePath(inner, "/kula")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/kula", nil))
		if rec.Code != http.StatusMovedPermanently {
			t.Fatalf("expected 301 redirect, got %d", rec.Code)
		}
		if loc := rec.Header().Get("Location"); loc != "/kula/" {
			t.Errorf("Location = %q, want %q", loc, "/kula/")
		}
	})

	t.Run("with nested base path", func(t *testing.T) {
		h := mountWithBasePath(inner, "/monitoring/kula")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/monitoring/kula/api/current", nil))
		if rec.Code != http.StatusOK || rec.Body.String() != "ok:/api/current" {
			t.Fatalf("got code=%d body=%q", rec.Code, rec.Body.String())
		}
	})
}

func TestCookiePath(t *testing.T) {
	if got := cookiePath(""); got != "/" {
		t.Errorf("cookiePath(\"\") = %q, want /", got)
	}
	if got := cookiePath("/kula"); got != "/kula/" {
		t.Errorf("cookiePath(\"/kula\") = %q, want /kula/", got)
	}
}

func TestTemplateBasePathInjection(t *testing.T) {
	s := NewServer(
		config.WebConfig{
			BasePath: "/kula",
			Security: config.SecurityConfig{Headers: true, OriginValidation: true},
		},
		config.GlobalConfig{}, nil, nil, t.TempDir(), config.OllamaConfig{},
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	handler := s.securityMiddleware(http.HandlerFunc(s.handleIndex))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got code=%d body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `<base href="/kula/">`) {
		t.Errorf("HTML missing <base href=\"/kula/\">; body excerpt:\n%s", body[:min(len(body), 600)])
	}
	if !strings.Contains(body, `window.KULA_BASE_PATH = "\/kula"`) &&
		!strings.Contains(body, `window.KULA_BASE_PATH = "/kula"`) {
		t.Errorf("HTML missing window.KULA_BASE_PATH literal; body excerpt:\n%s", body[:min(len(body), 800)])
	}
}

func TestTemplateBasePathEmpty(t *testing.T) {
	s := NewServer(
		config.WebConfig{Security: config.SecurityConfig{Headers: true, OriginValidation: true}},
		config.GlobalConfig{}, nil, nil, t.TempDir(), config.OllamaConfig{},
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	handler := s.securityMiddleware(http.HandlerFunc(s.handleIndex))
	handler.ServeHTTP(rec, req)
	body := rec.Body.String()
	if strings.Contains(body, "<base href=") {
		t.Errorf("HTML must not contain <base href> when base path is empty")
	}
	if !strings.Contains(body, `window.KULA_BASE_PATH = ""`) {
		t.Errorf("expected empty KULA_BASE_PATH string literal; body excerpt:\n%s", body[:min(len(body), 800)])
	}
}

// TestWebContentAccessLog verifies that served UI content (HTML, JS, CSS,
// fonts, icons) produces a "[WEB]" access-log line through the full handler
// stack, and that nothing is logged when logging is disabled.
func TestWebContentAccessLog(t *testing.T) {
	for _, tc := range []struct {
		path string
	}{
		{"/"},
		{"/style.css"},
		{"/js/app/main.js"},
		{"/kula.svg"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			cfg := config.WebConfig{
				UI:      true,
				Logging: config.LogConfig{Enabled: true, Level: "access"},
			}
			s := NewServer(cfg, config.GlobalConfig{}, nil, nil, t.TempDir(), config.OllamaConfig{})

			var buf bytes.Buffer
			orig := log.Writer()
			log.SetOutput(&buf)
			defer log.SetOutput(orig)

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			s.buildHandler().ServeHTTP(rec, req)

			got := buf.String()
			if !strings.Contains(got, "[WEB] ") {
				t.Fatalf("expected a [WEB] access-log line for %s, got logs: %q", tc.path, got)
			}
			if !strings.Contains(got, tc.path) {
				t.Errorf("expected access log to include path %s, got logs: %q", tc.path, got)
			}
		})
	}
}

// TestWebContentAccessLogDisabled verifies no access-log line is emitted for
// served UI content when logging is disabled.
func TestWebContentAccessLogDisabled(t *testing.T) {
	cfg := config.WebConfig{
		UI:      true,
		Logging: config.LogConfig{Enabled: false},
	}
	s := NewServer(cfg, config.GlobalConfig{}, nil, nil, t.TempDir(), config.OllamaConfig{})

	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(orig)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/style.css", nil)
	s.buildHandler().ServeHTTP(rec, req)

	if strings.Contains(buf.String(), "[WEB]") {
		t.Errorf("expected no access log when logging disabled, got: %q", buf.String())
	}
}

// The customization menu reads its server-side defaults from /api/config, so
// the appearance and accessibility sections must survive the round trip --
// including the `false` values, which are the ones that turn a feature off.
func TestHandleConfigExposesUISettings(t *testing.T) {
	cfg := config.WebConfig{
		Appearance: config.AppearanceConfig{StickyTopbar: false, Gauges: true},
		Accessibility: config.AccessibilityConfig{
			HighContrast: true,
			TextSize:     120,
		},
	}
	c := collector.New(config.GlobalConfig{}, config.CollectionConfig{}, config.ApplicationsConfig{}, t.TempDir())
	s := NewServer(cfg, config.GlobalConfig{}, c, nil, t.TempDir(), config.OllamaConfig{})

	rec := httptest.NewRecorder()
	http.HandlerFunc(s.handleConfig).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/config", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var got struct {
		Appearance    map[string]bool `json:"appearance"`
		Accessibility struct {
			HighContrast   bool `json:"high_contrast"`
			ReduceMotion   bool `json:"reduce_motion"`
			UnderlineLinks bool `json:"underline_links"`
			FocusOutline   bool `json:"focus_outline"`
			TextSize       int  `json:"text_size"`
			TextSizeRange  struct {
				Min  int `json:"min"`
				Max  int `json:"max"`
				Step int `json:"step"`
			} `json:"text_size_range"`
		} `json:"accessibility"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding /api/config: %v", err)
	}

	wantAppearance := map[string]bool{"sticky_topbar": false, "gauges": true}
	if !reflect.DeepEqual(got.Appearance, wantAppearance) {
		t.Errorf("appearance = %v, want %v", got.Appearance, wantAppearance)
	}
	a := got.Accessibility
	if !a.HighContrast || a.ReduceMotion || a.UnderlineLinks || a.FocusOutline {
		t.Errorf("accessibility toggles = %+v, want only high_contrast on", a)
	}
	if a.TextSize != 120 {
		t.Errorf("text_size = %d, want 120", a.TextSize)
	}
	// The menu's stepper clamps to the range the loader enforces, so the two
	// must be served from the same constants.
	if a.TextSizeRange.Min != config.MinTextSize || a.TextSizeRange.Max != config.MaxTextSize ||
		a.TextSizeRange.Step != config.TextSizeStep {
		t.Errorf("text_size_range = %+v, want %d..%d step %d", a.TextSizeRange,
			config.MinTextSize, config.MaxTextSize, config.TextSizeStep)
	}
}

func TestHandleHealth(t *testing.T) {
	s := NewServer(config.WebConfig{Security: config.SecurityConfig{Headers: true, OriginValidation: true}}, config.GlobalConfig{}, nil, nil, t.TempDir(), config.OllamaConfig{})

	for _, path := range []string{"/health", "/status"} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, path, nil)

			http.HandlerFunc(s.handleHealth).ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("Expected status 200 for %s, got %d", path, rec.Code)
			}
			if rec.Body.String() != "kula is healthy" {
				t.Fatalf("Expected body %q for %s, got %q", "kula is healthy", path, rec.Body.String())
			}
		})
	}
}

func TestGameScoreURLTemplateAndCSP(t *testing.T) {
	globalCfg := config.GlobalConfig{
		EasterEgg:    true,
		GameScoreURL: "https://my-score-server.com/api/v1/submit?game=kula",
	}
	s := NewServer(config.WebConfig{Security: config.SecurityConfig{Headers: true}}, globalCfg, nil, nil, t.TempDir(), config.OllamaConfig{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/game.html", nil)

	handler := s.securityMiddleware(http.HandlerFunc(s.handleGame))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("Expected status 200, got %d", rec.Code)
	}

	csp := rec.Header().Get("Content-Security-Policy")
	expectedCSPPart := "connect-src 'self' https://my-score-server.com;"
	if !strings.Contains(csp, expectedCSPPart) {
		t.Errorf("expected CSP to contain %q, got %q", expectedCSPPart, csp)
	}

	body := rec.Body.String()
	expectedAttr := `data-score-url="https://my-score-server.com/api/v1/submit?game=kula"`
	if !strings.Contains(body, expectedAttr) {
		t.Errorf("expected HTML body to contain %q", expectedAttr)
	}
}

func TestInvalidGameScoreURLIsNotRendered(t *testing.T) {
	s := NewServer(config.WebConfig{Security: config.SecurityConfig{Headers: true}}, config.GlobalConfig{
		EasterEgg:    true,
		GameScoreURL: "https://scores.example.com;script-src/submit",
	}, nil, nil, t.TempDir(), config.OllamaConfig{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/game.html", nil)
	s.securityMiddleware(http.HandlerFunc(s.handleGame)).ServeHTTP(rec, req)

	if strings.Contains(rec.Header().Get("Content-Security-Policy"), "connect-src") {
		t.Errorf("invalid score URL added a connect-src directive: %q", rec.Header().Get("Content-Security-Policy"))
	}
	if strings.Contains(rec.Body.String(), "scores.example.com") {
		t.Error("invalid score URL was rendered into the game template")
	}
}

func TestGameScoreSubmissionRequestPolicy(t *testing.T) {
	gameJS, err := staticFS.ReadFile("static/game.js")
	if err != nil {
		t.Fatalf("ReadFile(game.js): %v", err)
	}
	for _, option := range []string{
		"credentials: 'omit'",
		"redirect: 'error'",
		"referrerPolicy: 'no-referrer'",
	} {
		if !strings.Contains(string(gameJS), option) {
			t.Errorf("game score request is missing %s", option)
		}
	}
}

// TestHandleHistoryIncludesPSU guards the reported battery bug end to end: the
// dashboard rebuilds every chart from /api/history whenever the time preset
// changes, so a power-supply series missing from that response is a battery
// chart that resets to empty on every reload.
func TestHandleHistoryIncludesPSU(t *testing.T) {
	store, err := storage.NewStore(config.StorageConfig{
		Directory: t.TempDir(),
		Tiers: []config.TierConfig{
			{Resolution: time.Second, MaxSize: "1MB", MaxBytes: 1024 * 1024},
		},
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer func() { _ = store.Close() }()

	base := time.Now().Add(-10 * time.Second).Truncate(time.Second)
	for i := 0; i < 5; i++ {
		sample := &collector.Sample{
			Timestamp: base.Add(time.Duration(i) * time.Second),
			System:    collector.SystemStats{Hostname: "test-host"},
			PSU: []collector.PowerSupplyStats{{
				Name: "BAT0", Type: "Battery", Status: "Discharging",
				Capacity: 91 - i, PowerW: 14.5, VoltageV: 12.1,
			}},
		}
		if err := store.WriteSample(sample); err != nil {
			t.Fatalf("WriteSample(%d): %v", i, err)
		}
	}

	s := NewServer(config.WebConfig{}, config.GlobalConfig{}, nil, store, t.TempDir(), config.OllamaConfig{})

	rec := httptest.NewRecorder()
	from := base.Add(-time.Minute).UTC().Format(time.RFC3339)
	to := time.Now().Add(time.Minute).UTC().Format(time.RFC3339)
	req := httptest.NewRequest(http.MethodGet, "/api/history?from="+from+"&to="+to, nil)
	http.HandlerFunc(s.handleHistory).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		Samples []struct {
			Data struct {
				PSU []collector.PowerSupplyStats `json:"psu"`
			} `json:"data"`
		} `json:"samples"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Samples) == 0 {
		t.Fatalf("no samples returned: %s", rec.Body.String())
	}
	for i, sample := range resp.Samples {
		if len(sample.Data.PSU) != 1 {
			t.Fatalf("sample %d carries no psu series: %s", i, rec.Body.String())
		}
		ps := sample.Data.PSU[0]
		if ps.Name != "BAT0" || ps.Type != "Battery" || ps.Status != "Discharging" {
			t.Errorf("sample %d psu identity = %q/%q/%q", i, ps.Name, ps.Type, ps.Status)
		}
		if ps.Capacity < 87 || ps.Capacity > 91 {
			t.Errorf("sample %d capacity = %d, want 87..91", i, ps.Capacity)
		}
		if ps.PowerW != 14.5 {
			t.Errorf("sample %d power = %v, want 14.5", i, ps.PowerW)
		}
	}
}
