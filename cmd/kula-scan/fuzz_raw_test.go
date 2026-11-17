package main

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// quietServer serves handler, without net/http's log.
func quietServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	ts := httptest.NewUnstartedServer(handler)
	ts.Config.ErrorLog = log.New(io.Discard, "", 0)
	ts.Start()
	t.Cleanup(ts.Close)
	return ts
}

func fuzzScanner(t *testing.T, target string, iter int) *Scanner {
	t.Helper()
	s, err := NewScanner(Options{Target: target, Timeout: 5 * time.Second, Fuzz: true, FuzzIter: iter, Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	s.discover()
	return s
}

// TestFuzzPathSendsRawPathsAsWritten: the raw half of FUZZ-PATH reaches the
// server unescaped, so a page that echoes the request URI is caught by a
// raw request's canary, and bad escapes reach net/http's parser, which
// refuses them.
func TestFuzzPathSendsRawPathsAsWritten(t *testing.T) {
	var mu sync.Mutex
	var raw, escaped int
	ts := quietServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_, _ = io.WriteString(w, "kula is healthy")
			return
		case "/api/auth/status":
			kmJSON(w, map[string]any{"auth_required": false})
			return
		}
		mu.Lock()
		if strings.Contains(r.RequestURI, "<kx") {
			raw++
		}
		if strings.Contains(r.RequestURI, "%3Ckx") {
			escaped++
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html>"+r.RequestURI+"</html>")
	}))
	s := fuzzScanner(t, ts.URL, 60)

	f, _ := byID(runFuzzPath(s), "FUZZ-PATH")
	if f.Status != StatusFail || !strings.Contains(f.Evidence, "reflected input (possible XSS) ← raw GET /") {
		t.Errorf("FUZZ-PATH = %s against a page echoing the raw request URI; want a FAIL on a raw request: %s | %s", f.Status, f.Detail, f.Evidence)
	}
	mu.Lock()
	defer mu.Unlock()
	if raw == 0 || escaped == 0 {
		t.Errorf("the server saw %d request URIs with the canary as written and %d escaped; want both", raw, escaped)
	}

	// Against a page that escapes nothing but echoes nothing either, every
	// request is answered, the malformed ones by net/http's parser.
	ts = quietServer(t, secureMock())
	s = fuzzScanner(t, ts.URL, 60)
	f, _ = byID(runFuzzPath(s), "FUZZ-PATH")
	if f.Status != StatusPass || !strings.Contains(f.Detail, "60 planned, 60 sent: 60 reached the server (") ||
		!strings.Contains(f.Detail, "refused by the HTTP parser before routing") {
		t.Errorf("FUZZ-PATH against the secure mock = %s: %s", f.Status, f.Detail)
	}
}

// TestFuzzQueryCoversEveryHistoryParameter: FUZZ-QUERY sends tier and
// sections as well as from, to and points, both well formed and raw, and
// some of its windows pass validation.
func TestFuzzQueryCoversEveryHistoryParameter(t *testing.T) {
	m := newKulaMock(true, false)
	answered := map[int]int{}
	var mu sync.Mutex
	m.override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/api/history" {
			return false
		}
		rec := &statusRecorder{ResponseWriter: w}
		m.api(rec, r)
		mu.Lock()
		answered[rec.status]++
		mu.Unlock()
		return true
	}
	s, _ := m.serve(t, Options{Username: kmUser, Password: kmPass, FuzzIter: 200})
	wantStatus(t, runFuzzQuery(s), "FUZZ-QUERY", StatusPass)

	seen := map[string]int{}
	for _, r := range m.requests() {
		if r.path != "/api/history" {
			continue
		}
		for _, key := range historyParams {
			if strings.Contains(r.query, key+"=") {
				seen[key]++
			}
		}
		if strings.ContainsAny(r.query, ";<>") || strings.Contains(r.query, "&&") {
			seen["raw"]++
		}
	}
	for _, key := range append(historyParams, "raw") {
		if seen[key] == 0 {
			t.Errorf("no history request carried %s: %v", key, seen)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if answered[http.StatusOK] == 0 || answered[http.StatusBadRequest] == 0 {
		t.Errorf("history answers by status: %v; want some valid windows (200) and some refused ones (400)", answered)
	}
}

// statusRecorder notes the status a handler answers.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}
