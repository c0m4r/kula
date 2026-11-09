package web

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kula/internal/collector"
	"kula/internal/config"
)

// hostTestHandler serves the production handler chain with auth disabled,
// the dashboard on and an unauthenticated /metrics: the setup DNS rebinding
// reads from.
func hostTestHandler(t *testing.T, edit func(*config.WebConfig)) http.Handler {
	t.Helper()
	cfg := config.WebConfig{
		Enabled:                true,
		UI:                     true,
		MaxWebsocketConns:      10,
		MaxWebsocketConnsPerIP: 5,
		Security:               config.SecurityConfig{Headers: true, FrameProtection: true, OriginValidation: true},
		Auth:                   config.AuthConfig{SessionTimeout: time.Hour, SessionMaxLifetime: time.Hour},
		PrometheusMetrics:      config.MetricsConfig{Enabled: true},
	}
	if edit != nil {
		edit(&cfg)
	}
	s := NewServer(cfg, config.GlobalConfig{Hostname: "testhost.example.com"}, &collector.Collector{}, newTestStoreWithSample(t), t.TempDir(), config.OllamaConfig{})
	return s.buildHandler()
}

func serveHost(h http.Handler, host, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = host
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestHostCheckRefusesRebindingNames: with auth disabled, a name from public
// DNS (as a rebinding page sends) gets a JSON 403 on every route but the
// health probes, and the names an outside DNS server cannot answer for pass.
func TestHostCheckRefusesRebindingNames(t *testing.T) {
	h := hostTestHandler(t, func(cfg *config.WebConfig) {
		cfg.Security.AllowedHosts = []string{"kula.example.com"}
		cfg.Security.AllowedOrigins = []string{"https://dash.example.org"}
	})

	for _, path := range []string{"/", "/style.css", "/api/current", "/api/config", "/ws", "/metrics"} {
		rec := serveHost(h, "attacker.example:27960", path)
		var body struct{ Error string }
		if rec.Code != http.StatusForbidden || json.Unmarshal(rec.Body.Bytes(), &body) != nil ||
			!strings.Contains(body.Error, "web.security.allowed_hosts") {
			t.Errorf("%s with a public Host: %d %q, want a JSON 403 naming allowed_hosts", path, rec.Code, rec.Body.String())
		}
	}
	for _, path := range []string{"/health", "/status"} {
		if rec := serveHost(h, "attacker.example", path); rec.Code != http.StatusOK {
			t.Errorf("%s with a public Host: %d, want 200 for probes", path, rec.Code)
		}
	}
	// Behind a bearer token /metrics is not readable by a rebinding page, and
	// Prometheus scrapes by regular DNS names: the token check answers instead.
	tokened := hostTestHandler(t, func(cfg *config.WebConfig) { cfg.PrometheusMetrics.Token = "scrape-secret" })
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Host = "kula.example.com:27960"
	rec := httptest.NewRecorder()
	tokened.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("tokened /metrics with a public Host and no token: %d, want 401", rec.Code)
	}
	req.Header.Set("Authorization", "Bearer scrape-secret")
	rec = httptest.NewRecorder()
	tokened.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("tokened /metrics with a public Host and its token: %d, want 200", rec.Code)
	}
	if rec := serveHost(tokened, "attacker.example", "/api/current"); rec.Code != http.StatusForbidden {
		t.Errorf("a metrics token opened /api/current to a public Host: %d", rec.Code)
	}
	for _, host := range []string{
		"127.0.0.1:27960", "192.0.2.10", "[::1]:27960", "[2001:db8::1]",
		"localhost:27960", "LOCALHOST.", "kula.localhost", "kula", "kula:27960",
		"nas.local", "router.lan", "box.home.arpa", "svc.internal", "pi.home", "host.localdomain",
		"Kula.Example.com:443", "dash.example.org", "",
	} {
		if rec := serveHost(h, host, "/api/current"); rec.Code == http.StatusForbidden {
			t.Errorf("Host %q was refused: %s", host, rec.Body.String())
		}
	}
	// Local names and allowed names match whole labels, not as prefixes.
	for _, host := range []string{"kula.example.com.attacker.example", "local.attacker.example", "lan.attacker.example", "attacker-local.example"} {
		if rec := serveHost(h, host, "/api/current"); rec.Code != http.StatusForbidden {
			t.Errorf("Host %q was answered (%d)", host, rec.Code)
		}
	}
}

// TestHostCheckAppliesOnlyWithoutAuthOverTCP: signed-in access has no session
// to lend a rebinding page, a Unix socket has no browser connecting to it, and
// "*" turns the check off.
func TestHostCheckAppliesOnlyWithoutAuthOverTCP(t *testing.T) {
	for name, edit := range map[string]func(*config.WebConfig){
		"auth enabled": func(cfg *config.WebConfig) {
			cfg.Auth.Enabled = true
			cfg.Auth.Username = "admin"
			cfg.Auth.PasswordHash = "00"
			cfg.Auth.PasswordSalt = "00"
			cfg.Auth.Argon2 = config.Argon2Config{Time: 1, Memory: 8, Threads: 1}
		},
		"unix socket":       func(cfg *config.WebConfig) { cfg.UnixSocket = "/run/kula/kula.sock" },
		"allowed_hosts '*'": func(cfg *config.WebConfig) { cfg.Security.AllowedHosts = []string{"kula.example.com", "*"} },
	} {
		h := hostTestHandler(t, edit)
		if rec := serveHost(h, "attacker.example", "/"); rec.Code == http.StatusForbidden {
			t.Errorf("%s: a public Host was refused: %s", name, rec.Body.String())
		}
	}
}

// TestHostCheckMatchesTheMachineName covers the machine's own hostname, which
// a LAN resolver may serve with its search domain stripped or not at all, and
// global.hostname, often the FQDN where os.Hostname is the short name.
func TestHostCheckMatchesTheMachineName(t *testing.T) {
	p := newHostPolicy(config.WebConfig{}, "Build-Box.corp.example.")
	if !p.allows("build-box.corp.example:27960") || p.allows("other.corp.example") {
		t.Error("the machine's hostname is not matched exactly")
	}
	p = newHostPolicy(config.WebConfig{}, "polaris2", "Polaris2.Example.com", "")
	for _, host := range []string{"polaris2:27960", "polaris2.example.com", "polaris2.example.com.:27960"} {
		if !p.allows(host) {
			t.Errorf("%s: this machine's configured name was refused", host)
		}
	}
	for _, host := range []string{"polaris2.attacker.com", "polaris2.example.com.attacker.com", "example.com"} {
		if p.allows(host) {
			t.Errorf("%s: a name only extending this machine's was allowed", host)
		}
	}

	h := hostTestHandler(t, nil) // global.hostname "testhost.example.com"
	if rec := serveHost(h, "testhost.example.com:27960", "/api/current"); rec.Code == http.StatusForbidden {
		t.Errorf("global.hostname as Host was refused: %s", rec.Body.String())
	}
}

// TestHostCheckLogsEachNameOnce keeps a flood of refused names from flooding
// the log: each name once, and a bounded number of names.
func TestHostCheckLogsEachNameOnce(t *testing.T) {
	var out bytes.Buffer
	previous, flags := log.Writer(), log.Flags()
	log.SetOutput(&out)
	log.SetFlags(0)
	defer func() { log.SetOutput(previous); log.SetFlags(flags) }()

	h := hostTestHandler(t, nil)
	out.Reset()
	for range 3 {
		serveHost(h, "attacker.example", "/api/current")
	}
	if n := strings.Count(out.String(), `refused Host "attacker.example"`); n != 1 {
		t.Fatalf("one refused name logged %d times:\n%s", n, out.String())
	}
	for i := range 3 * maxLoggedHosts {
		serveHost(h, strings.Repeat("a", i+1)+".attacker.example", "/api/current")
	}
	if n := strings.Count(out.String(), "Host check:"); n > maxLoggedHosts+1 {
		t.Fatalf("%d host-check lines logged for %d names, want at most %d", n, 3*maxLoggedHosts+1, maxLoggedHosts+1)
	}
}
