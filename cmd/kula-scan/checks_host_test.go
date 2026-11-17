package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// kulaLocalHost reports whether Kula's host check (host_check.go) answers
// host when no name is configured: an address, a single-label name or a name
// under a local-only domain, with or without a port and a trailing dot.
func kulaLocalHost(host string) bool {
	name := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		name = h
	}
	name = strings.TrimSuffix(strings.ToLower(strings.Trim(name, "[]")), ".")
	if _, err := netip.ParseAddr(name); err == nil || name == "" || !strings.Contains(name, ".") {
		return true
	}
	for _, suffix := range []string{"localhost", "local", "test", "internal", "home.arpa", "lan", "home", "localdomain", "corp"} {
		if name == suffix || strings.HasSuffix(name, "."+suffix) {
			return true
		}
	}
	return false
}

// withHostCheck serves next behind Kula's host check: a request for a path
// guarded reports true for is refused, as Kula refuses it, when its Host is
// not local.
func withHostCheck(next http.Handler, guarded func(path string) bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if guarded(r.URL.Path) && !kulaLocalHost(r.Host) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"host not allowed: add it to web.security.allowed_hosts"}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// authOffGuard is what Kula guards with auth off: every route but the probes.
func authOffGuard(path string) bool { return path != "/health" && path != "/status" }

// metricsGuard is what Kula guards with auth on and /metrics served without
// a token: /metrics alone.
func metricsGuard(path string) bool { return path == "/metrics" }

// authOff is authOffMock without Ollama, its counters discarded.
func authOff() http.Handler {
	return authOffMock(false, new(atomic.Int64), new(atomic.Int64))
}

// openMetrics serves the secure mock (auth on) with /metrics open: no token.
func openMetrics() http.Handler {
	mock := secureMock()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
			_, _ = w.Write([]byte("kula_cpu_usage_percent 1\n"))
			return
		}
		mock.ServeHTTP(w, r)
	})
}

// hostFinding runs HOST-001 against handler, scanned by its address.
func hostFinding(t *testing.T, handler http.Handler) Finding {
	t.Helper()
	ts := httptest.NewServer(handler)
	defer ts.Close()
	s := scannerFor(t, ts, "", "")
	f, ok := byID(runHostCheck(s), "HOST-001")
	if !ok {
		t.Fatal("HOST-001 not reported")
	}
	return f
}

func TestHostCheckRefusesForeignNames(t *testing.T) {
	rec := &requestRecorder{}
	f := hostFinding(t, rec.wrap(withHostCheck(authOff(), authOffGuard)))
	if f.Status != StatusPass || f.Severity != SevHigh || !strings.Contains(f.Detail, "Kula's host check refused 5 with 403") {
		t.Errorf("HOST-001 = %s %s %q, want a PASS naming Kula's refusals", f.Status, f.Severity, f.Detail)
	}

	// The control under the scanner's own name, then each foreign form;
	// discovery asked /api/config before them.
	var hosts []string
	for _, r := range rec.requests() {
		if r.path == "/api/config" {
			hosts = append(hosts, r.host)
		}
	}
	if len(hosts) >= 6 {
		hosts = hosts[len(hosts)-6:]
	}
	if len(hosts) != 6 || !kulaLocalHost(hosts[0]) {
		t.Fatalf("sent /api/config under %q, want the own name and five foreign ones", hosts)
	}
	name := strings.ToLower(hosts[1])
	if !strings.HasPrefix(name, "kula-scan-") || !strings.HasSuffix(name, ".example.com") {
		t.Fatalf("the foreign name %q is not a random name under example.com", hosts[1])
	}
	for i, want := range []string{name + ":", name + ".", "", "localhost." + name} {
		got := hosts[i+2]
		switch {
		case want == "": // mixed case
			if got == name || !strings.EqualFold(got, name) {
				t.Errorf("the mixed-case form is %q", got)
			}
		case !strings.HasPrefix(got, want):
			t.Errorf("form %d is %q, want it to start with %q", i+2, got, want)
		}
	}
}

func TestHostCheckOffWarns(t *testing.T) {
	f := hostFinding(t, authOff())
	if f.Status != StatusWarn || f.Severity != SevHigh || f.Expectation != "host-check" || !strings.Contains(f.Detail, "DNS rebinding") {
		t.Errorf("HOST-001 = %s %s expectation %q %q, want a HIGH WARN against host-check", f.Status, f.Severity, f.Expectation, f.Detail)
	}

	code, report, stderr := scanJSON(t, authOff(), "-only", "host")
	if code != exitClean {
		t.Errorf("without -expect: exit %d, want 0; stderr:\n%s", code, stderr)
	}
	wantStatus(t, report.Findings, "HOST-001", StatusWarn)

	code, report, stderr = scanJSON(t, authOff(), "-only", "host", "-expect", "host-check")
	if code != exitFailing {
		t.Errorf("-expect host-check: exit %d, want 1; stderr:\n%s", code, stderr)
	}
	if f, _ := byID(report.Findings, "HOST-001"); f.Status != StatusFail || f.Expectation != "host-check" {
		t.Errorf("-expect host-check: HOST-001 = %s, expectation %q", f.Status, f.Expectation)
	}
}

// TestSloppyHostCheckIsCaught: a check that compares the name as written
// misses the port and trailing-dot forms.
func TestSloppyHostCheckIsCaught(t *testing.T) {
	mock := authOff()
	f := hostFinding(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(strings.ToLower(r.Host), ".example.com") {
			http.Error(w, `{"error":"host not allowed"}`, http.StatusForbidden)
			return
		}
		mock.ServeHTTP(w, r)
	}))
	if f.Status != StatusWarn || !strings.Contains(f.Detail, "with a port") || !strings.Contains(f.Detail, "with a trailing dot") ||
		strings.Contains(f.Detail, "mixed case") {
		t.Errorf("HOST-001 = %s %q, want a WARN naming the port and trailing-dot forms only", f.Status, f.Detail)
	}
}

func TestProxyRefusalOfForeignNamesPasses(t *testing.T) {
	mock := authOff()
	proxy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !kulaLocalHost(r.Host) {
			http.Error(w, "Misdirected Request", http.StatusMisdirectedRequest)
			return
		}
		mock.ServeHTTP(w, r)
	})
	f := hostFinding(t, proxy)
	if f.Status != StatusPass || !strings.Contains(f.Detail, "something other than Kula's host check refused") || !strings.Contains(f.Detail, "HTTP 421") {
		t.Errorf("HOST-001 = %s %q, want a PASS naming the 421", f.Status, f.Detail)
	}

	// A default page for unknown names is not Kula's answer.
	welcome := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !kulaLocalHost(r.Host) {
			_, _ = w.Write([]byte("<html>Welcome to nginx!</html>"))
			return
		}
		mock.ServeHTTP(w, r)
	})
	if f := hostFinding(t, welcome); f.Status != StatusPass || !strings.Contains(f.Detail, "not Kula's answer") {
		t.Errorf("a proxy's default page: HOST-001 = %s %q, want a PASS", f.Status, f.Detail)
	}
}

func TestHostCheckWithAuthJudgesATokenlessMetrics(t *testing.T) {
	disabled := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			http.NotFound(w, r)
			return
		}
		secureMock().ServeHTTP(w, r)
	})
	for _, c := range []struct {
		name    string
		handler http.Handler
		want    Status
		detail  string
	}{
		{"a token", secureMock(), StatusSkip, "bearer token (HTTP 401)"},
		{"no /metrics", disabled, StatusSkip, "not enabled (HTTP 404)"},
		{"no token, local names only", withHostCheck(openMetrics(), metricsGuard), StatusPass, "/metrics answered none"},
		{"no token, any name", openMetrics(), StatusWarn, "/metrics answered Host names"},
	} {
		f := hostFinding(t, c.handler)
		if f.Status != c.want || !strings.Contains(f.Detail, c.detail) {
			t.Errorf("%s: HOST-001 = %s %q, want %s saying %q", c.name, f.Status, f.Detail, c.want, c.detail)
		}
		if c.want == StatusWarn && !strings.Contains(f.Remediation, "prometheus_metrics.token") {
			t.Errorf("%s: the remediation does not name the token: %q", c.name, f.Remediation)
		}
	}
}

func TestHostCheckWithTheUIOff(t *testing.T) {
	uiOff := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health", "/status":
			_, _ = w.Write([]byte("kula is healthy"))
		default:
			http.NotFound(w, r)
		}
	})
	// Kula's refusal of the foreign names still shows the check is on.
	if f := hostFinding(t, withHostCheck(uiOff, authOffGuard)); f.Status != StatusPass {
		t.Errorf("UI off behind the host check: HOST-001 = %s %q, want a PASS", f.Status, f.Detail)
	}
	if f := hostFinding(t, uiOff); f.Status != StatusSkip || !strings.Contains(f.Detail, "HTTP 404") {
		t.Errorf("UI off without the host check: HOST-001 = %s %q, want a SKIP", f.Status, f.Detail)
	}
}

func TestHostCheckSkipsOverAUnixSocket(t *testing.T) {
	var hits atomic.Int64
	mock := authOff()
	socket := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/config" {
			hits.Add(1)
		}
		mock.ServeHTTP(w, r)
	}))
	s, err := NewScanner(Options{Target: "http://kula.test/", Unix: socket, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	s.discover()
	hits.Store(0)
	f, _ := byID(runHostCheck(s), "HOST-001")
	if f.Status != StatusSkip || !strings.Contains(f.Detail, "Unix socket") || hits.Load() != 0 {
		t.Errorf("-unix: HOST-001 = %s %q after %d requests, want a SKIP sending none", f.Status, f.Detail, hits.Load())
	}
}

// ---- the scanner's own Host name refused ---------------------------------------

// refusingKula serves Kula with auth off as buildHandler stacks it: the
// security headers are set before the host check refuses a request.
func refusingKula() http.Handler {
	return secureHeaders(withHostCheck(authOff(), authOffGuard))
}

// scanUnderForeignName scans handler by its address under the name
// kula.example.com, which Kula's host check refuses.
func scanUnderForeignName(t *testing.T, handler http.Handler, args ...string) (int, Report, string) {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	address := ts.Listener.Addr().String()
	_, port, _ := net.SplitHostPort(address)
	all := append([]string{"-json", "-connect", address}, args...)
	code, stdout, stderr := runCaptured(t, append(all, "http://kula.example.com:"+port+"/")...)
	return code, decodeReport(t, stdout), stderr
}

func TestScannersOwnHostRefused(t *testing.T) {
	code, report, stderr := scanUnderForeignName(t, refusingKula())
	if code != exitInconclusive {
		t.Errorf("exit %d, want 3; stderr:\n%s", code, stderr)
	}
	if ids := reportedIDs(report); len(ids) < 2 || ids[0] != "DISC-001" || ids[1] != "DISC-003" {
		t.Fatalf("reported %v, want DISC-001 then DISC-003", ids)
	}
	f, _ := byID(report.Findings, "DISC-003")
	for _, want := range []string{"kula.example.com", "IP address", "-connect", "allowed_hosts"} {
		if f.Status != StatusError || !strings.Contains(f.Detail, want) {
			t.Errorf("DISC-003 = %s %q, want an ERROR saying %q", f.Status, f.Detail, want)
		}
	}

	// The checks that read the refusal as their own result are skipped.
	for _, id := range []string{"CSRF-001", "CSRF-002", "CORS-001", "CORS-003", "TRAV-001", "PROM-001", "WS-000", "WS-002",
		"INPUT-001", "INPUT-003", "REDIR-001"} {
		f, ok := byID(report.Findings, id)
		if !ok || f.Status != StatusSkip || !strings.Contains(f.Detail, "DISC-003") {
			t.Errorf("%s = %s %q, want a SKIP naming DISC-003", id, f.Status, f.Detail)
		}
	}
	// The ones that judge something still run.
	for _, id := range []string{"HDR-001", "HDR-003", "HDR-004", "HOST-001"} {
		wantStatus(t, report.Findings, id, StatusPass)
	}
	wantStatus(t, report.Findings, "AUTH-000", StatusWarn)
	if f, _ := byID(report.Findings, "HOST-001"); !strings.Contains(f.Detail, "own name kula.example.com") {
		t.Errorf("HOST-001 does not say the scanner's own name is refused too: %q", f.Detail)
	}
	if !strings.Contains(stderr, "inconclusive") || !strings.Contains(stderr, "DISC-003") {
		t.Errorf("no closing line explains the exit:\n%s", stderr)
	}

	// Scanned by its address, nothing is refused.
	code, report, _ = scanJSON(t, refusingKula(), "-only", "discovery,host,csrf")
	if _, ok := byID(report.Findings, "DISC-003"); ok || code == exitInconclusive {
		t.Errorf("by address: exit %d with %v", code, reportedIDs(report))
	}
	wantStatus(t, report.Findings, "HOST-001", StatusPass)
}

func TestRefusedHostNameAndSelection(t *testing.T) {
	// The refusal skipped what was asked for: DISC-003 says why.
	code, report, _ := scanUnderForeignName(t, refusingKula(), "-only", "csrf")
	if got := strings.Join(reportedIDs(report), ","); got != "DISC-001,DISC-003,CSRF-001,CSRF-002,CSRF-003,CSRF-004" || code != exitInconclusive {
		t.Errorf("-only csrf: exit %d, reported %s", code, got)
	}
	// It skipped nothing asked for.
	code, report, _ = scanUnderForeignName(t, refusingKula(), "-only", "headers")
	if _, ok := byID(report.Findings, "DISC-003"); ok || code != exitClean {
		t.Errorf("-only headers: exit %d, reported %v", code, reportedIDs(report))
	}
	// -skip names it.
	code, report, _ = scanUnderForeignName(t, refusingKula(), "-only", "csrf", "-skip", "disc-003")
	if _, ok := byID(report.Findings, "DISC-003"); ok || code != exitClean {
		t.Errorf("-skip disc-003: exit %d, reported %v", code, reportedIDs(report))
	}
}

// TestTokenlessMetricsUnderARefusedName: with auth on Kula checks the Host
// name of a /metrics served without a token only, so its refusal says so.
func TestTokenlessMetricsUnderARefusedName(t *testing.T) {
	ts := httptest.NewServer(withHostCheck(openMetrics(), metricsGuard))
	defer ts.Close()
	address := ts.Listener.Addr().String()
	_, port, _ := net.SplitHostPort(address)
	s, err := NewScanner(Options{Target: "http://kula.example.com:" + port, Connect: address, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, refused := byID(s.discover(), "DISC-003"); refused || s.hostRefused || !s.authEnabled {
		t.Fatalf("with auth on, only /metrics is refused: DISC-003 %v, authEnabled %v", refused, s.authEnabled)
	}
	prom := runMetricsChecks(s)
	f, _ := byID(prom, "PROM-001")
	if f.Status != StatusWarn || f.Expectation != "metrics-token" || !strings.Contains(f.Detail, "without a bearer token") {
		t.Errorf("PROM-001 = %s expectation %q %q, want a WARN against metrics-token", f.Status, f.Expectation, f.Detail)
	}
	wantStatus(t, prom, "PROM-003", StatusSkip)
	if f, _ := byID(runHostCheck(s), "HOST-001"); f.Status != StatusPass || !strings.Contains(f.Detail, "own name") {
		t.Errorf("HOST-001 = %s %q, want a PASS noting the own name is refused too", f.Status, f.Detail)
	}
}

func TestHostCheckUndeliveredIsAnError(t *testing.T) {
	s, err := NewScanner(Options{Target: "http://" + closedAddress(t), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	wantStatus(t, runHostCheck(s), "HOST-001", StatusError)
}
