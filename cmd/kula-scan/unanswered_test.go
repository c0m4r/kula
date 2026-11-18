package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// dropping serves next, but hangs up without an answer on every request drop
// reports true for, as a gateway that loses some requests does.
func dropping(next http.Handler, drop func(r *http.Request) bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if drop(r) {
			dropConnection(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// TestAPassNeedsEveryRequestAnswered: a check that sends several requests
// and finds nothing wrong in the answers it got has proved nothing of the
// ones that got none. With one of its requests unanswered, each of these
// checks is an ERROR that names it, not a PASS.
func TestAPassNeedsEveryRequestAnswered(t *testing.T) {
	for _, c := range []struct {
		id    string
		run   func(*Scanner) []Finding
		drop  func(r *http.Request) bool
		named string // how the ERROR names the dropped request
	}{
		{"HDR-001", runHeaderChecks, func(r *http.Request) bool { return r.URL.Path == "/style.css" }, "/style.css"},
		{"HDR-010", runHeaderChecks, func(r *http.Request) bool {
			return r.URL.Path == "/api/config" && r.Header.Get("Cookie") != ""
		}, "/api/config"},
		{"AUTH-001", runAuthChecks, func(r *http.Request) bool {
			return r.URL.Path == "/api/history" && r.Header.Get("Cookie") == ""
		}, "/api/history"},
		{"AUTH-002", runAuthChecks, func(r *http.Request) bool { return r.Header.Get("Cookie") == "kula_session=deadbeef" }, `"deadbeef"`},
		{"CSRF-002", runCSRFChecks, func(r *http.Request) bool {
			return r.URL.Path == "/api/logout" && r.Header.Get("Referer") != ""
		}, "Referer: https://evil.example/"},
		{"CORS-003", runCORSChecks, func(r *http.Request) bool { return r.Header.Get("Origin") == "null" }, "null"},
		{"INPUT-001", runInputChecks, func(r *http.Request) bool { return r.URL.Query().Get("tier") == "-1" }, "negative tier"},
		{"INPUT-003", runInputChecks, func(r *http.Request) bool { return r.URL.Query().Get("lang") == "zz" }, "zz"},
		{"REDIR-001", runOpenRedirectCheck, func(r *http.Request) bool { return r.RequestURI == "/.evil.example" }, "/.evil.example"},
	} {
		t.Run(c.id, func(t *testing.T) {
			ts := httptest.NewServer(dropping(secureMock(), c.drop))
			defer ts.Close()
			s := scannerFor(t, ts, mockUser, mockPass)
			f, ok := byID(c.run(s), c.id)
			if !ok {
				t.Fatalf("%s not reported", c.id)
			}
			if f.Status != StatusError || !strings.Contains(f.Detail, c.named) || !strings.Contains(f.Detail, "unanswered") {
				t.Errorf("%s with one request unanswered = %s %q, want an ERROR naming %s", c.id, f.Status, f.Detail, c.named)
			}
		})
	}
}

// TestUnansweredRequestsKeepAFailure: a check that found something wrong
// still reports it when some of its other requests went unanswered, and
// names those.
func TestUnansweredRequestsKeepAFailure(t *testing.T) {
	ts := httptest.NewServer(dropping(insecureMock(), func(r *http.Request) bool {
		return r.URL.Path == "/api/history" && r.Header.Get("Cookie") == ""
	}))
	defer ts.Close()
	s := scannerFor(t, ts, "", "")
	f, _ := byID(runAuthChecks(s), "AUTH-001")
	if f.Status != StatusFail || !strings.Contains(f.Detail, "/api/current (HTTP 200)") || !strings.Contains(f.Detail, "/api/history") {
		t.Errorf("AUTH-001 = %s %q, want a FAIL naming the open routes and the unanswered one", f.Status, f.Detail)
	}
}

// TestAuthRoutesNeedEveryAnswer: AUTH-001 and AUTH-002 do not pass on the
// one request a target answered while it hung up on all the others.
func TestAuthRoutesNeedEveryAnswer(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/health":
			_, _ = w.Write([]byte("kula is healthy"))
		case r.URL.Path == "/api/auth/status":
			_, _ = w.Write([]byte(`{"auth_required":true,"authenticated":false}`))
		case r.URL.Path == "/api/current" && r.Header.Get("Cookie") == "":
			w.WriteHeader(http.StatusUnauthorized)
		case r.URL.Path == "/api/i18n" && r.Header.Get("Cookie") == "kula_session=deadbeef":
			w.WriteHeader(http.StatusUnauthorized)
		default:
			dropConnection(w)
		}
	}))
	defer ts.Close()
	s := scannerFor(t, ts, "", "")
	findings := runAuthChecks(s)
	for id, want := range map[string]string{
		"AUTH-001": "8 of the 9 requests to the protected API went unanswered",
		"AUTH-002": "4 of the 5 requests with a forged session cookie went unanswered",
	} {
		if f, _ := byID(findings, id); f.Status != StatusError || !strings.Contains(f.Detail, want) {
			t.Errorf("%s = %s %q, want an ERROR saying %q", id, f.Status, f.Detail, want)
		}
	}
}

// TestIntegrityNeedsEveryScriptFetched: SRI-002 does not pass on the one
// script it could fetch while the others, behind a CDN's challenge page,
// were never compared.
func TestIntegrityNeedsEveryScriptFetched(t *testing.T) {
	good := "console.log(1)"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.Header().Set("Content-Type", "text/html")
			_, _ = fmt.Fprintf(w, `<html><script src="/a.js" integrity="%s"></script>`, sriHash(good))
			for i := 0; i < 20; i++ {
				_, _ = fmt.Fprintf(w, `<script src="/b%d.js" integrity="%s"></script>`, i, sriHash(good))
			}
		case "/a.js":
			_, _ = w.Write([]byte(good))
		case "/b0.js":
			dropConnection(w)
		default:
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer ts.Close()
	f, _ := byID(runIntegrityChecks(dosScanner(t, ts.URL, 2*time.Second, time.Second)), "SRI-002")
	if f.Status != StatusError || !strings.Contains(f.Detail, "20 of the 21 scripts") ||
		!strings.Contains(f.Detail, "/b0.js (") || !strings.Contains(f.Detail, "/b19.js (HTTP 403)") {
		t.Errorf("SRI-002 with 20 of 21 scripts unfetched = %s %q, want an ERROR naming them", f.Status, f.Detail)
	}
}

// TestHostCheckNeedsEveryAnswer: HOST-001 does not pass on the foreign Host
// names Kula refused while requests under the others were never delivered.
// The target stops listening after the third foreign name.
func TestHostCheckNeedsEveryAnswer(t *testing.T) {
	ts := httptest.NewUnstartedServer(nil)
	kula := withHostCheck(authOff(), authOffGuard)
	ts.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.Host, ".") {
			_ = ts.Listener.Close()
		}
		kula.ServeHTTP(w, r)
	})
	ts.Start()
	defer ts.Close()
	s := scannerFor(t, ts, "", "")
	f, _ := byID(runHostCheck(s), "HOST-001")
	if f.Status != StatusError || !strings.Contains(f.Detail, "2 of 5 requests") {
		t.Errorf("HOST-001 with two foreign names undelivered = %s %q, want an ERROR", f.Status, f.Detail)
	}
}

// TestXFFBypassNeedsEveryAnswer: BYPASS-XFF does not pass on a throttle
// seen firing while some attempts of its burst got no answer.
func TestXFFBypassNeedsEveryAnswer(t *testing.T) {
	var n atomic.Int64
	mock := xffMock(false)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/login" {
			if n.Add(1) == 3 {
				dropConnection(w)
				return
			}
		}
		mock.ServeHTTP(w, r)
	}))
	defer ts.Close()
	s := scannerForDos(t, ts, "", "", time.Second)
	f, _ := byID(runXFFBypassCheck(s), "BYPASS-XFF")
	if f.Status != StatusError {
		t.Errorf("BYPASS-XFF with an attempt unanswered = %s %q | %s, want an ERROR", f.Status, f.Detail, f.Evidence)
	}
}
