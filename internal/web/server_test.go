package web

import (
	"bytes"
	"context"
	"encoding/json"
	"html"
	"log"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
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
	// An unknown sri key renders an empty integrity, which browsers ignore.
	if strings.Contains(body, `integrity=""`) {
		t.Error("HTML body has a script without an SRI hash")
	}
	if bundle := s.sriHashes["js/chartjs/chartjs-bundle.min.js"]; bundle == "" || !strings.Contains(body, `integrity="`+bundle+`"`) {
		t.Error("HTML body missing the Chart.js bundle SRI")
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
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://localhost"+tc.path, nil))
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
			req := httptest.NewRequest(http.MethodGet, "http://localhost"+tc.path, nil)
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

// TestAccessLogEscapesThePath: a decoded path may hold a newline (%0a), which
// would let a request forge whole access-log lines, so the escaped path is
// logged instead.
func TestAccessLogEscapesThePath(t *testing.T) {
	cfg := config.WebConfig{
		UI:      true,
		Logging: config.LogConfig{Enabled: true, Level: "access"},
	}
	h := NewServer(cfg, config.GlobalConfig{}, nil, nil, t.TempDir(), config.OllamaConfig{}).buildHandler()

	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(orig)

	const forged = "/x%0a2026/01/01%2000:00:00%20[API]%2010.0.0.1%20POST%20/api/login%20200%201ms%0d"
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://localhost"+forged, nil))

	got := buf.String()
	if n := strings.Count(got, "\n"); n != 1 {
		t.Fatalf("one request wrote %d log lines: %q", n, got)
	}
	if strings.ContainsAny(got[:len(got)-1], "\r\n") || !strings.Contains(got, "/x%0a2026") {
		t.Fatalf("access log = %q, want the path escaped on one line", got)
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
	req := httptest.NewRequest(http.MethodGet, "http://localhost/style.css", nil)
	s.buildHandler().ServeHTTP(rec, req)

	if strings.Contains(buf.String(), "[WEB]") {
		t.Errorf("expected no access log when logging disabled, got: %q", buf.String())
	}
}

// The customization menu reads its server-side defaults from /api/config, so
// the appearance and accessibility sections must survive the round trip --
// including the `false` values, which are the ones that turn a feature off.
// TestHandleConfigSendsEveryGraphBound: each configured Y-axis bound reaches
// the dashboard, which asks for it by the same key (getChartMaxBound).
func TestHandleConfigSendsEveryGraphBound(t *testing.T) {
	cfg := config.WebConfig{Graphs: config.GraphConfig{
		CPUTemp:  config.GraphMaxConfig{MaxMode: "on", MaxValue: 90},
		DiskTemp: config.GraphMaxConfig{MaxMode: "on", MaxValue: 70},
		GPUTemp:  config.GraphMaxConfig{MaxMode: "on", MaxValue: 85},
		Network:  config.GraphMaxConfig{MaxMode: "on", MaxValue: 2500},
	}}
	c := collector.New(config.GlobalConfig{}, config.CollectionConfig{}, config.ApplicationsConfig{}, t.TempDir())
	s := NewServer(cfg, config.GlobalConfig{}, c, nil, t.TempDir(), config.OllamaConfig{})
	rec := httptest.NewRecorder()
	http.HandlerFunc(s.handleConfig).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/config", nil))

	var got struct {
		Graphs map[string]struct {
			Mode  string  `json:"mode"`
			Value float64 `json:"value"`
		} `json:"graphs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding /api/config: %v", err)
	}
	for key, want := range map[string]float64{"cpu_temp": 90, "disk_temp": 70, "gpu_temp": 85, "network": 2500} {
		if b, ok := got.Graphs[key]; !ok || b.Mode != "on" || b.Value != want {
			t.Errorf("graphs.%s = %+v (present %v), want on/%v", key, b, ok, want)
		}
	}
}

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
		Appearance map[string]bool `json:"appearance"`
		History    struct {
			CollectionIntervalMS float64                 `json:"collection_interval_ms"`
			Ranges               []storage.RetainedRange `json:"ranges"`
		} `json:"history"`
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
	if got.History.CollectionIntervalMS != 1000 || got.History.Ranges == nil || len(got.History.Ranges) != 0 {
		t.Fatalf("empty history metadata = %+v", got.History)
	}
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
			req := httptest.NewRequest(http.MethodGet, "http://localhost"+path, nil)

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

// The policy denies plugins and keeps <base> and form targets on the page's
// origin, under a base path too, whose <base> it must still admit. The game
// score origin is for the game's page alone: no other response may connect
// there.
func TestContentSecurityPolicyDirectives(t *testing.T) {
	s := NewServer(config.WebConfig{UI: true, BasePath: "/kula", Security: config.SecurityConfig{Headers: true, FrameProtection: true}},
		config.GlobalConfig{EasterEgg: true, GameScoreURL: "https://scores.example.com/submit"}, nil, nil, t.TempDir(), config.OllamaConfig{})
	handler := s.buildHandler()
	for _, path := range []string{"/kula/", "/kula/index.html", "/kula/api/auth/status", "/kula/js/app/main.js", "/kula/missing"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://localhost"+path, nil))
		csp := rec.Header().Get("Content-Security-Policy")
		for _, want := range []string{"default-src 'self';", "object-src 'none';", "base-uri 'self';", "form-action 'self';", "frame-ancestors 'none';"} {
			if !strings.Contains(csp, want) {
				t.Errorf("%s: CSP %q lacks %q", path, csp, want)
			}
		}
		if strings.Contains(csp, "connect-src") || strings.Contains(csp, "scores.example.com") {
			t.Errorf("%s: CSP %q lets the page connect to the game score origin", path, csp)
		}
		if path == "/kula/" && !strings.Contains(rec.Body.String(), `<base href="/kula/">`) {
			t.Errorf("the dashboard under a base path has no same-origin <base>")
		}
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

func TestHistoryFrontendRegressions(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; skipping frontend module tests")
	}

	cmd := exec.Command(node, "testdata/history_frontend_test.mjs")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("history frontend tests failed: %v\n%s", err, output)
	}
}

func TestChartBundle(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; skipping Chart.js bundle tests")
	}

	output, err := exec.Command(node, "testdata/chart_bundle_test.mjs").CombinedOutput()
	if err != nil {
		t.Fatalf("Chart.js bundle tests failed: %v\n%s", err, output)
	}
}

func TestHistoryFrontendUsesSharedLifecycleAndCanonicalItems(t *testing.T) {
	chartsData, err := staticFS.ReadFile("static/js/app/charts-data.js")
	if err != nil {
		t.Fatalf("ReadFile(charts-data.js): %v", err)
	}
	source := string(chartsData)

	// Rolling, rolling tail, custom and zoomed history all load through the
	// shared latest-request-wins controller.
	if got := strings.Count(source, "return requestHistory("); got != 4 {
		t.Fatalf("history loaders using the shared controller = %d, want 4", got)
	}
	for _, obsolete := range []string{
		"if (state.loadingHistory) return;",
		"state.dataBuffer.push(sample);",
		"addSampleToCharts(item.data || item",
		"state.liveTail",
	} {
		if strings.Contains(source, obsolete) {
			t.Errorf("history data path still contains obsolete pattern %q", obsolete)
		}
	}
	for _, required := range []string{
		"new HistoryRequestController()",
		"data.map(normalizeHistoryItem)",
		"renderHistoryItems(visible)",
		"historyItemExtrema(historyItem, validAggregations)",
		"historyPointBudget()",
		"queueAllChartUpdates()",
		"state.timeRange === null && state.customFrom && state.customTo",
	} {
		if !strings.Contains(source, required) {
			t.Errorf("history data path is missing canonical pattern %q", required)
		}
	}
	if strings.Contains(source, "window.innerWidth || 1000") {
		t.Error("history point budget still uses browser width instead of actual plot width")
	}
	if strings.Contains(source, ".update('none')") {
		t.Error("charts-data bypasses the animation-frame chart controller")
	}

	mainJS, err := staticFS.ReadFile("static/js/app/main.js")
	if err != nil {
		t.Fatalf("ReadFile(main.js): %v", err)
	}
	if !strings.Contains(string(mainJS), "redrawChartsFromBuffer();") {
		t.Error("aggregation changes do not use the envelope-preserving redraw path")
	}

	indexHTML, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("ReadFile(index.html): %v", err)
	}
	for _, hiddenControl := range []string{
		`id="btn-agg-menu" class="btn-icon btn-time-menu hidden"`,
		`id="agg-presets-list" class="time-presets-list hidden"`,
	} {
		if !strings.Contains(string(indexHTML), hiddenControl) {
			t.Errorf("aggregation controls are exposed before validity metadata: missing %q", hiddenControl)
		}
	}
	for _, interaction := range []string{
		`id="btn-live"`,
		`id="btn-history-back"`,
		`id="btn-history-forward"`,
		`id="btn-zoom-out"`,
		`id="pinned-time"`,
		`id="pinned-time-announcement"`,
		`id="history-status-announcement"`,
		`data-i18n-aria-label="historical_navigation"`,
		`data-i18n-title="history_back"`,
		`data-i18n-title="history_forward"`,
		`data-i18n-title="zoom_out"`,
		`data-time-zone="local"`,
		`data-time-zone="utc"`,
		`js/app/chart-controller.js`,
		`js/app/chart-interactions.js`,
		`js/app/chart-envelope.js`,
		`js/app/chart-accessibility.js`,
		`js/app/format.js`,
		`js/app/history-data.js`,
		`js/app/history-navigation.js`,
	} {
		if !strings.Contains(string(indexHTML), interaction) {
			t.Errorf("history interaction UI is missing %q", interaction)
		}
	}
	if count := strings.Count(string(indexHTML), `aria-live="polite"`); count != 3 {
		t.Errorf("ARIA polite live regions = %d, want pin, history-status, and system-info announcements", count)
	}
}

func TestHistoricalTooltipsUseBucketContextAndExplicitTimeZone(t *testing.T) {
	checks := map[string][]string{
		"static/js/app/charts-init.js": {
			"formatFullTimestamp(",
			"historyTooltipLines(",
			"state.historyPointContexts.get(",
			"formatChartTick(",
		},
		"static/js/app/charts-data.js": {
			"annotateHistoryItems(samples, response)",
			"rebuildHistoryPointContexts()",
		},
		"static/js/app/controls.js": {
			"formatDateTimeInput(date, state.timeZone, true",
			"parseDateTimeInput(endpointValue('start'), state.timeZone)",
		},
		"static/js/app/main.js": {
			"writePref('kula_time_zone', state.timeZone)",
			"syncTimeZoneControls()",
		},
	}
	for path, required := range checks {
		source, err := staticFS.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", path, err)
		}
		for _, fragment := range required {
			if !strings.Contains(string(source), fragment) {
				t.Errorf("%s is missing %q", path, fragment)
			}
		}
	}
}

func TestAuthenticatedFrontendReloadsTranslationsBeforeWebSocket(t *testing.T) {
	authJS, err := staticFS.ReadFile("static/js/app/auth.js")
	if err != nil {
		t.Fatalf("ReadFile(auth.js): %v", err)
	}
	source := string(authJS)
	configAt := strings.Index(source, "const config = await fetchConfig();")
	refreshAt := strings.Index(source, "await i18n.refreshAfterAuth(config);")
	connectAt := -1
	if refreshAt >= 0 {
		if offset := strings.Index(source[refreshAt:], "connectWS();"); offset >= 0 {
			connectAt = refreshAt + offset
		}
	}
	if configAt < 0 || refreshAt <= configAt || connectAt <= refreshAt {
		t.Error("successful login does not restore translations before opening the WebSocket")
	}

	i18nJS, err := staticFS.ReadFile("static/js/app/i18n.js")
	if err != nil {
		t.Fatalf("ReadFile(i18n.js): %v", err)
	}
	for _, required := range []string{
		"async refreshAfterAuth(config = {})",
		"this.applyTranslations();",
		"new Event('kula-i18n-changed')",
	} {
		if !strings.Contains(string(i18nJS), required) {
			t.Errorf("authenticated translation refresh is missing %q", required)
		}
	}
}

func TestHistoricalChartsUseCompactTrustworthyEnvelopes(t *testing.T) {
	chartsData, err := staticFS.ReadFile("static/js/app/charts-data.js")
	if err != nil {
		t.Fatalf("ReadFile(charts-data.js): %v", err)
	}
	source := string(chartsData)
	for _, required := range []string{
		"historyItemExtrema(historyItem, validAggregations)",
		"const hasEnvelope = !!minimum && !!maximum;",
		"appendEnvelopePoint(",
		"trimEnvelopeData(",
		"clearEnvelopeData(",
		"appendEnvelopeGap(",
		"state.historyGaps",
	} {
		if !strings.Contains(source, required) {
			t.Errorf("history envelope lifecycle is missing %q", required)
		}
	}

	chartsInit, err := staticFS.ReadFile("static/js/app/charts-init.js")
	if err != nil {
		t.Fatalf("ReadFile(charts-init.js): %v", err)
	}
	initSource := string(chartsInit)
	for _, required := range []string{
		"Chart.register(envelopePlugin, chartAccessibilityPlugin, crosshairPlugin, tooltipGesturePlugin)",
		"dataset.fill = false;",
		"dataset.tension = 0;",
		"line: { tension: 0",
		"gaps: () => state.historyGaps",
	} {
		if !strings.Contains(initSource, required) {
			t.Errorf("historical chart presentation is missing %q", required)
		}
	}

	envelopeJS, err := staticFS.ReadFile("static/js/app/chart-envelope.js")
	if err != nil {
		t.Fatalf("ReadFile(chart-envelope.js): %v", err)
	}
	for _, required := range []string{
		"export function measurementGapRects(",
		"drawMeasurementGaps(chart, xScale, options)",
		"chart.ctx.fillRect(",
	} {
		if !strings.Contains(string(envelopeJS), required) {
			t.Errorf("measurement-gap shading is missing %q", required)
		}
	}
}

func TestHistoricalChartsExposeAccessibleAlternatives(t *testing.T) {
	checks := map[string][]string{
		"static/js/app/chart-accessibility.js": {
			"export function chartSummaryText(",
			"export function keyboardCursorTimestamp(",
			"export function chartTableModel(",
			"export function chartCSV(",
			"canvas.setAttribute('aria-labelledby'",
			"canvas.setAttribute('aria-describedby'",
			"canvas.setAttribute('aria-keyshortcuts'",
			"TABLE_ROW_LIMIT = 50",
		},
		"static/js/app/charts-data.js": {
			"document.getElementById('history-status-announcement')",
			"updateChartAccessibility(chart)",
		},
		"static/js/app/charts-init.js": {
			"attachChartAccessibility(chart",
			"chartAccessibilityPlugin",
		},
	}
	for path, required := range checks {
		source, err := staticFS.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", path, err)
		}
		for _, fragment := range required {
			if !strings.Contains(string(source), fragment) {
				t.Errorf("%s is missing %q", path, fragment)
			}
		}
	}
}

func TestFocusModeEmptySelectionRestoresFullHistory(t *testing.T) {
	focusJS, err := staticFS.ReadFile("static/js/app/focus-mode.js")
	if err != nil {
		t.Fatalf("ReadFile(focus-mode.js): %v", err)
	}
	source := string(focusJS)
	start := strings.Index(source, "if (selected.length === 0)")
	if start < 0 {
		t.Fatal("Focus Mode empty-selection exit branch is missing")
	}
	branch := source[start:]
	end := strings.Index(branch, "state.focusVisible = selected;")
	if end < 0 {
		t.Fatal("could not isolate Focus Mode empty-selection exit branch")
	}
	branch = branch[:end]
	for _, required := range []string{
		"state.focusMode = false;",
		"state.focusVisible = null;",
		"removePref('kula_focus_visible');",
		"kula-history-sections-changed",
	} {
		if !strings.Contains(branch, required) {
			t.Errorf("empty-selection exit does not restore full history: missing %q", required)
		}
	}
}

func TestChartsUseOneDelegatedDoubleClickReset(t *testing.T) {
	for _, path := range []string{
		"static/js/app/split.js",
		"static/js/app/chart-card-actions.js",
	} {
		source, err := staticFS.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", path, err)
		}
		if strings.Contains(string(source), "addEventListener('dblclick'") {
			t.Errorf("%s still installs a second direct double-click reset handler", path)
		}
	}

	mainJS, err := staticFS.ReadFile("static/js/app/main.js")
	if err != nil {
		t.Fatalf("ReadFile(main.js): %v", err)
	}
	if !strings.Contains(string(mainJS), "event.target?.matches?.('.chart-body canvas')") {
		t.Fatal("global delegated chart double-click reset handler is missing")
	}
}

// TestHandleHistoryIncludesPSU guards the reported battery bug end to end: the
// dashboard rebuilds every chart from /api/history whenever the time preset
// changes, so a power-supply series missing from that response is a battery
// chart that resets to empty on every reload.
func TestHandleHistoryThirtyDays(t *testing.T) {
	store, err := storage.NewStore(config.StorageConfig{
		Directory: t.TempDir(),
		Tiers:     []config.TierConfig{{Resolution: 5 * time.Minute, MaxBytes: 10 * 1024 * 1024}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range 8640 {
		if err := store.WriteSample(&collector.Sample{Timestamp: base.Add(time.Duration(i+1) * 5 * time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{store: store}
	rec := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/history?from="+base.Format(time.RFC3339)+"&to="+base.Add(30*24*time.Hour).Format(time.RFC3339), nil)
	s.handleHistory(rec, request)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var result storage.HistoryResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	contributors := 0
	for _, sample := range result.Samples {
		contributors += sample.SampleCount
	}
	if !result.ExactComplete || len(result.Samples) > 450 || contributors != 8640 {
		t.Fatalf("incomplete 30d response: exact=%v points=%d contributors=%d", result.ExactComplete, len(result.Samples), contributors)
	}
}

// TestHandleHistoryTierHint covers the tier hint a dashboard tail refresh
// sends: a few minutes of a tier-1 view must come from tier 1, not from the
// raw tier the window alone would pick, and a malformed hint is refused.
func TestHandleHistoryTierHint(t *testing.T) {
	store, err := storage.NewStore(config.StorageConfig{
		Directory: t.TempDir(),
		Tiers: []config.TierConfig{
			{Resolution: time.Second, MaxBytes: 4 * 1024 * 1024},
			{Resolution: time.Minute, MaxBytes: 4 * 1024 * 1024},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 1; i <= 600; i++ {
		if err := store.WriteSample(&collector.Sample{Timestamp: base.Add(time.Duration(i) * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{store: store}
	query := func(extra string) (int, storage.HistoryResult) {
		t.Helper()
		rec := httptest.NewRecorder()
		url := "/api/history?from=" + base.Add(5*time.Minute).Format(time.RFC3339) +
			"&to=" + base.Add(8*time.Minute).Format(time.RFC3339) + "&points=3" + extra
		s.handleHistory(rec, httptest.NewRequest(http.MethodGet, url, nil))
		var result storage.HistoryResult
		if rec.Code == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
		}
		return rec.Code, result
	}
	if code, result := query(""); code != http.StatusOK || result.Tier != 0 {
		t.Fatalf("no hint: status %d, tier %d, want 200 from tier 0", code, result.Tier)
	}
	if code, result := query("&tier=1"); code != http.StatusOK || result.Tier != 1 || result.Resolution != "1m" {
		t.Fatalf("tier=1: status %d, tier %d at %s, want 200 from tier 1 at 1m", code, result.Tier, result.Resolution)
	}
	// A tier the store does not have is no hint.
	if code, result := query("&tier=7"); code != http.StatusOK || result.Tier != 0 {
		t.Fatalf("tier=7: status %d, tier %d, want 200 from tier 0", code, result.Tier)
	}
	for _, bad := range []string{"&tier=-1", "&tier=one", "&tier=1.5"} {
		if code, _ := query(bad); code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", bad, code)
		}
	}
}

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
	req := httptest.NewRequest(http.MethodGet, "/api/history?from="+from+"&to="+to+"&points=2", nil)
	http.HandlerFunc(s.handleHistory).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		ValidAggregations []string `json:"valid_aggregations"`
		Samples           []struct {
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
	if !reflect.DeepEqual(resp.ValidAggregations, []string{"data", "min", "max"}) {
		t.Errorf("valid_aggregations = %v, want [data min max]", resp.ValidAggregations)
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

func TestHandleHistorySelectsRequestedSections(t *testing.T) {
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

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	sample := &collector.Sample{
		Timestamp: base,
		CPU:       collector.CPUStats{Total: collector.CPUCoreStats{Usage: 42}},
		Memory:    collector.MemoryStats{Total: 1024, Used: 512},
		Disks: collector.DiskStats{Devices: []collector.DiskDevice{{
			Name: "sda", ReadBytesPS: 1234,
		}}},
		Apps: collector.ApplicationsStats{Custom: map[string][]collector.CustomMetricValue{
			"large": {{Name: "work", Value: 99}},
		}},
	}
	if err := store.WriteSample(sample); err != nil {
		t.Fatalf("WriteSample: %v", err)
	}

	srv := NewServer(config.WebConfig{}, config.GlobalConfig{}, nil, store, t.TempDir(), config.OllamaConfig{})
	query := "?from=" + base.Add(-time.Second).Format(time.RFC3339) +
		"&to=" + base.Add(time.Second).Format(time.RFC3339) + "&sections=mem,cpu"
	rec := httptest.NewRecorder()
	http.HandlerFunc(srv.handleHistory).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/history"+query, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	var response struct {
		Sections         []string `json:"sections"`
		SourceResolution string   `json:"source_resolution"`
		Samples          []struct {
			Data        map[string]json.RawMessage `json:"data"`
			BucketStart time.Time                  `json:"bucket_start"`
			BucketEnd   time.Time                  `json:"bucket_end"`
			SampleCount int                        `json:"sample_count"`
			Coverage    float64                    `json:"coverage"`
		} `json:"samples"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !reflect.DeepEqual(response.Sections, []string{"cpu", "mem"}) {
		t.Fatalf("sections=%v, want [cpu mem]", response.Sections)
	}
	if response.SourceResolution != "1s" {
		t.Fatalf("source_resolution=%q, want 1s", response.SourceResolution)
	}
	if len(response.Samples) != 1 {
		t.Fatalf("samples=%d, want 1", len(response.Samples))
	}
	if got := response.Samples[0]; got.SampleCount != 1 || got.Coverage != 1 ||
		!got.BucketStart.Equal(base.Add(-time.Second)) || !got.BucketEnd.Equal(base) {
		t.Errorf("sectioned history lost bucket metadata: %+v", got)
	}
	for _, key := range []string{"ts", "cpu", "mem"} {
		if _, ok := response.Samples[0].Data[key]; !ok {
			t.Errorf("selected response missing %q", key)
		}
	}
	for _, key := range []string{"disk", "apps", "gpu", "psu"} {
		if _, ok := response.Samples[0].Data[key]; ok {
			t.Errorf("selected response unexpectedly includes %q", key)
		}
	}

	bad := httptest.NewRecorder()
	http.HandlerFunc(srv.handleHistory).ServeHTTP(bad,
		httptest.NewRequest(http.MethodGet, "/api/history?sections=cpu,secrets", nil))
	if bad.Code != http.StatusBadRequest {
		t.Errorf("unknown section status=%d, want 400", bad.Code)
	}
}

func TestHandleHistoryRejectsWideRangeEvenWithinRetention(t *testing.T) {
	store, err := storage.NewStore(config.StorageConfig{
		Directory: t.TempDir(),
		Tiers: []config.TierConfig{
			{Resolution: 24 * time.Hour, MaxSize: "1MB", MaxBytes: 1024 * 1024},
		},
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer func() { _ = store.Close() }()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, ts := range []time.Time{base, base.Add(40 * 24 * time.Hour)} {
		if err := store.WriteSample(&collector.Sample{Timestamp: ts}); err != nil {
			t.Fatalf("WriteSample(%v): %v", ts, err)
		}
	}
	srv := NewServer(config.WebConfig{}, config.GlobalConfig{}, nil, store, t.TempDir(), config.OllamaConfig{})

	query := func(from, to time.Time) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		url := "/api/history?from=" + from.Format(time.RFC3339) +
			"&to=" + to.Format(time.RFC3339) + "&points=10"
		http.HandlerFunc(srv.handleHistory).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
		return rec
	}

	inside := query(base, base.Add(40*24*time.Hour))
	if inside.Code != http.StatusBadRequest {
		t.Fatalf("retained 40-day range status=%d body=%s", inside.Code, inside.Body.String())
	}

	outside := query(base.Add(-2*24*time.Hour), base.Add(40*24*time.Hour))
	if outside.Code != http.StatusBadRequest {
		t.Errorf("range before retained coverage status=%d, want 400", outside.Code)
	}

	future := query(base, base.Add(43*24*time.Hour))
	if future.Code != http.StatusBadRequest {
		t.Errorf("range after retained coverage status=%d, want 400", future.Code)
	}
}

// TestAPIResponsesAreNotStored: every API response, errors and unknown paths
// included, tells caches not to keep it, while static assets keep their
// revalidating policy.
func TestAPIResponsesAreNotStored(t *testing.T) {
	cfg := config.WebConfig{Enabled: true, UI: true, MaxWebsocketConns: 10, MaxWebsocketConnsPerIP: 2}
	c := collector.New(config.GlobalConfig{}, config.CollectionConfig{}, config.ApplicationsConfig{}, t.TempDir())
	s := NewServer(cfg, config.GlobalConfig{}, c, nil, t.TempDir(), config.OllamaConfig{})
	ts := httptest.NewServer(s.buildHandler())
	defer ts.Close()

	for path, want := range map[string]string{
		"/api/current":     "no-store",
		"/api/config":      "no-store",
		"/api/auth/status": "no-store",
		"/api/no-such":     "no-store",
		"/js/app/main.js":  "no-cache",
	} {
		resp, err := ts.Client().Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if got := resp.Header.Get("Cache-Control"); got != want {
			t.Errorf("%s (HTTP %d): Cache-Control = %q, want %q", path, resp.StatusCode, got, want)
		}
	}
}

// A value encoding/json refuses (an infinity or NaN in a sample) used to leave
// the handler with nothing written: an empty 200 the dashboard cannot parse.
func TestWriteJSONAnswersAnEncodingFailure(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, map[string]float64{"load": math.Inf(1)})
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	var failure map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &failure); err != nil || failure["error"] == "" {
		t.Errorf("body %q is not a JSON error (%v)", rec.Body.String(), err)
	}

	rec = httptest.NewRecorder()
	writeJSON(rec, map[string]float64{"load": 1.5})
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"load":1.5}` {
		t.Errorf("answered %d %q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
}

// TestShutdownRacingStart: kula serve runs Start on a goroutine of its own and
// Shutdown when a signal comes, possibly before Start has begun serving. The
// server must stop either way, and the two must not race on it.
func TestShutdownRacingStart(t *testing.T) {
	srv := hostTestServer(t, func(cfg *config.WebConfig) { cfg.Listen = "127.0.0.1" })
	done := make(chan error, 1)
	go func() { done <- srv.Start() }()
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the server kept serving after Shutdown")
	}
}
