package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The secure mock's /metrics takes the bearer token "metricssecret".
const (
	rightMetricsToken = "metricssecret"
	wrongMetricsToken = "not-the-metrics-t0ken"
)

func TestPrometheusAcceptsTheConfiguredToken(t *testing.T) {
	ts := httptest.NewServer(secureMock())
	defer ts.Close()

	code, stdout, stderr := runCaptured(t, "-json", "-v", "-only", "metrics", "-metrics-token", rightMetricsToken, ts.URL)
	if code != exitClean {
		t.Fatalf("exit %d; stderr:\n%s", code, stderr)
	}
	report := decodeReport(t, stdout)
	for _, id := range []string{"PROM-001", "PROM-002", "PROM-003"} {
		wantStatus(t, report.Findings, id, StatusPass)
	}
	if strings.Contains(stdout, rightMetricsToken) || strings.Contains(stderr, rightMetricsToken) {
		t.Errorf("the output shows the metrics token:\n%s\n%s", stdout, stderr)
	}
}

// TestRefusedMetricsTokenIsInconclusive: a token /metrics refuses says
// nothing of the target, like refused credentials, and comes from the
// environment as well as from the flag.
func TestRefusedMetricsTokenIsInconclusive(t *testing.T) {
	ts := httptest.NewServer(secureMock())
	defer ts.Close()
	t.Setenv(metricsTokenEnv, wrongMetricsToken)

	code, stdout, stderr := runCaptured(t, "-json", "-v", "-only", "metrics", ts.URL)
	if code != exitInconclusive {
		t.Errorf("exit %d, want 3; stderr:\n%s", code, stderr)
	}
	report := decodeReport(t, stdout)
	wantStatus(t, report.Findings, "PROM-003", StatusError)
	if f, _ := byID(report.Findings, "PROM-003"); !strings.Contains(f.Detail, "refused the supplied token (HTTP 401)") {
		t.Errorf("PROM-003 does not say the token was refused: %q", f.Detail)
	}
	if !strings.Contains(stderr, "inconclusive") || !strings.Contains(stderr, "PROM-003") {
		t.Errorf("no closing line explains the exit:\n%s", stderr)
	}
	if strings.Contains(stdout, wrongMetricsToken) || strings.Contains(stderr, wrongMetricsToken) {
		t.Errorf("the output shows the metrics token:\n%s\n%s", stdout, stderr)
	}

	if got := resolveMetricsToken("from-flag"); got != "from-flag" {
		t.Errorf("an explicit -metrics-token gave way to the environment: %q", got)
	}
}

// metricsTarget serves the secure mock with /metrics answered by metrics.
func metricsTarget(t *testing.T, metrics http.HandlerFunc) *httptest.Server {
	t.Helper()
	mock := secureMock()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			metrics(w, r)
			return
		}
		mock.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestMetricsTokenOutcomes(t *testing.T) {
	bearer := func(r *http.Request) bool { return r.Header.Get("Authorization") == "Bearer "+rightMetricsToken }
	refuse := func(w http.ResponseWriter) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="kula-metrics"`)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
	}
	cases := []struct {
		name    string
		token   string
		metrics http.HandlerFunc
		want    Status
		detail  string
	}{
		{"no token supplied", "", secureMock().ServeHTTP, StatusSkip, "no -metrics-token"},
		{"/metrics disabled", rightMetricsToken, http.NotFound, StatusSkip, "not enabled"},
		{"/metrics open", rightMetricsToken, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("kula_up 1\n"))
		}, StatusSkip, "no token is required"},
		{"Kula before its first sample", rightMetricsToken, func(w http.ResponseWriter, r *http.Request) {
			if !bearer(r) {
				refuse(w)
				return
			}
			w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		}, StatusPass, "Prometheus exposition"},
		{"comments, then samples", rightMetricsToken, func(w http.ResponseWriter, r *http.Request) {
			if !bearer(r) {
				refuse(w)
				return
			}
			w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
			_, _ = w.Write([]byte("# HELP kula_cpu_usage_percent CPU.\n# TYPE kula_cpu_usage_percent gauge\nkula_cpu_usage_percent{host=\"a b\"} 1.5\n"))
		}, StatusPass, "Prometheus exposition"},
		{"a page, not metrics", rightMetricsToken, func(w http.ResponseWriter, r *http.Request) {
			if !bearer(r) {
				refuse(w)
				return
			}
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html>sign in</html>"))
		}, StatusWarn, "not with a Prometheus exposition body"},
		{"another refusal", rightMetricsToken, func(w http.ResponseWriter, r *http.Request) {
			if !bearer(r) {
				refuse(w)
				return
			}
			http.Error(w, "forbidden", http.StatusForbidden)
		}, StatusWarn, "HTTP 403"},
	}
	for _, c := range cases {
		ts := metricsTarget(t, c.metrics)
		s, err := NewScanner(Options{Target: ts.URL, Timeout: 5 * time.Second, MetricsToken: c.token})
		if err != nil {
			t.Fatal(err)
		}
		s.discover()
		f, ok := byID(runMetricsChecks(s), "PROM-003")
		if !ok || f.Status != c.want || !strings.Contains(f.Detail, c.detail) {
			t.Errorf("%s: PROM-003 = %s %q, want %s saying %q", c.name, f.Status, f.Detail, c.want, c.detail)
		}
		if strings.Contains(f.Detail+f.Evidence, rightMetricsToken) {
			t.Errorf("%s: PROM-003 shows the token: %+v", c.name, f)
		}
	}
}
