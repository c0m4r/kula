package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"
)

// jsonHeader returns a JSON Content-Type header with the given extra
// name/value pairs.
func jsonHeader(pairs ...string) http.Header {
	h := http.Header{"Content-Type": {"application/json"}}
	for i := 0; i+1 < len(pairs); i += 2 {
		h.Set(pairs[i], pairs[i+1])
	}
	return h
}

func textHeader() http.Header {
	return http.Header{"Content-Type": {"text/plain; charset=utf-8"}}
}

// TestOracleKnowsKulasDeliberate5xx: the 5xx answers Kula gives on purpose
// are throttled or unavailable, not anomalies, and only on their own route
// with their own text (or, for a bodiless HEAD answer, a JSON one); any
// other 5xx is an anomaly.
func TestOracleKnowsKulasDeliberate5xx(t *testing.T) {
	head, get, post := http.MethodHead, http.MethodGet, http.MethodPost
	cases := []struct {
		name string
		r    httpResult
		want outcome
	}{
		{"no data yet", httpResult{method: get, path: "/api/current", status: 503, header: jsonHeader(), body: `{"error":"no data yet"}` + "\n"}, outUnavailable},
		{"no data yet, HEAD", httpResult{method: head, path: "/api/current", status: 503, header: jsonHeader()}, outUnavailable},
		{"history busy", httpResult{method: get, path: "/api/history?points=9", status: 503, header: jsonHeader("Retry-After", "2"),
			body: `{"error":"too many history requests in progress"}`}, outThrottled},
		{"history busy, HEAD", httpResult{method: head, path: "/api/history", status: 503, header: jsonHeader("Retry-After", "2")}, outThrottled},
		{"login busy", httpResult{method: post, path: "/api/login", status: 503, header: jsonHeader("Retry-After", "1"),
			body: `{"error":"server busy"}`}, outThrottled},
		{"assistant disabled, chat", httpResult{method: post, path: "/api/ollama/chat", status: 503, header: jsonHeader(),
			body: `{"code":"disabled","error":"ollama is not enabled"}`}, outUnavailable},
		{"assistant disabled, context", httpResult{method: get, path: "/api/ollama/context", status: 503, header: jsonHeader(),
			body: `{"code":"disabled","error":"ollama is not enabled"}`}, outUnavailable},
		{"assistant disabled, models HEAD", httpResult{method: head, path: "/api/ollama/models", status: 503, header: jsonHeader()}, outUnavailable},
		{"backend unreachable", httpResult{method: get, path: "/api/ollama/models", status: 502, header: jsonHeader(),
			body: `{"code":"backend_unavailable","error":"AI backend unavailable"}`}, outUnavailable},

		{"history 503 without Retry-After", httpResult{method: get, path: "/api/history", status: 503, header: jsonHeader(),
			body: `{"error":"too many history requests in progress"}`}, outAnomaly},
		{"no data yet from another route", httpResult{method: get, path: "/api/history", status: 503, header: jsonHeader(),
			body: `{"error":"no data yet"}`}, outAnomaly},
		{"503 with another text", httpResult{method: get, path: "/api/current", status: 503, header: jsonHeader(),
			body: `{"error":"database is locked"}`}, outAnomaly},
		{"backend unreachable from another route", httpResult{method: get, path: "/api/current", status: 502, header: jsonHeader(),
			body: `{"code":"backend_unavailable","error":"AI backend unavailable"}`}, outAnomaly},
		{"disabled code without its text", httpResult{method: get, path: "/api/ollama/models", status: 503, header: jsonHeader(),
			body: `{"code":"disabled","error":"oops"}`}, outAnomaly},
		{"HEAD 503 that is not JSON", httpResult{method: head, path: "/api/current", status: 503, header: textHeader()}, outAnomaly},
		{"plain 500", httpResult{method: get, path: "/api/history", status: 500, header: jsonHeader(),
			body: `{"error":"internal storage error"}`}, outAnomaly},
		{"bare 503", httpResult{status: 503}, outAnomaly},
		{"bare 500", httpResult{status: 500}, outAnomaly},

		{"net/http refuses a request line", httpResult{method: get, path: "/%zz", status: 400, header: textHeader(),
			body: "400 Bad Request: invalid URI"}, outReached},
		{"net/http refuses a Transfer-Encoding", httpResult{method: post, path: "/health", status: 501, header: textHeader(),
			body: "Unsupported transfer encoding"}, outReached},
		{"another 501", httpResult{method: get, path: "/", status: 501, header: textHeader(), body: "Not Implemented"}, outAnomaly},

		{"auth gate", httpResult{method: get, path: "/api/current", status: 401, header: jsonHeader(), body: `{"error":"unauthorized"}`}, outGated},
		{"auth gate, HEAD", httpResult{method: head, path: "/api/current", status: 401, header: jsonHeader()}, outGated},
		{"CSRF gate", httpResult{method: http.MethodPut, path: "/api/config", status: 403, header: jsonHeader(), body: `{"error":"invalid csrf token"}`}, outGated},
		{"origin gate", httpResult{method: http.MethodPut, path: "/api/config", status: 403, header: jsonHeader(), body: `{"error":"invalid origin"}`}, outGated},
		{"a wrong password reaches the handler", httpResult{method: post, path: "/api/login", status: 401, header: jsonHeader(),
			body: `{"error":"invalid credentials"}`}, outReached},
		{"a 400 from a handler", httpResult{method: get, path: "/api/history", status: 400, header: jsonHeader(),
			body: `{"error":"invalid 'tier'"}`}, outReached},
		{"429", httpResult{method: post, path: "/api/login", status: 429, header: jsonHeader(), body: `{"error":"too many requests"}`}, outThrottled},
	}
	for _, c := range cases {
		if got := classify(c.r); got.outcome != c.want {
			t.Errorf("%s: outcome %d (%s), want %d", c.name, got.outcome, got.why, c.want)
		}
	}
}

// TestOracleJudgesErrorsByType: a request that never left the client is not
// sent; one that was sent and then dropped or left unanswered is an anomaly.
func TestOracleJudgesErrorsByType(t *testing.T) {
	syscallErr := func(op string, errno syscall.Errno) error {
		return &url.Error{Op: "Get", URL: "http://kula.test/", Err: &net.OpError{Op: op, Net: "tcp", Err: os.NewSyscallError(op, errno)}}
	}
	cases := []struct {
		name   string
		r      httpResult
		want   outcome
		why    string
		refuse bool
	}{
		{"connection refused", httpResult{err: syscallErr("dial", syscall.ECONNREFUSED), unsent: true}, outUnsent, "", true},
		{"no connection", httpResult{err: errors.New("dial tcp: lookup kula.test: no such host"), unsent: true}, outUnsent, "", false},
		{"unbuildable URL", httpResult{err: errors.New(`parse "http://kula.test/%zz": invalid URL escape "%zz"`), unsent: true}, outUnsent, "", false},
		{"interrupted", httpResult{err: &url.Error{Op: "Get", URL: "http://kula.test/", Err: context.Canceled}}, outUnsent, "", false},
		{"EOF", httpResult{err: &url.Error{Op: "Get", URL: "http://kula.test/", Err: io.EOF}}, outAnomaly, kindDropped, false},
		{"unexpected EOF", httpResult{err: fmt.Errorf("reading: %w", io.ErrUnexpectedEOF)}, outAnomaly, kindDropped, false},
		{"reset", httpResult{err: syscallErr("read", syscall.ECONNRESET)}, outAnomaly, kindDropped, false},
		{"broken pipe", httpResult{err: syscallErr("write", syscall.EPIPE)}, outAnomaly, kindDropped, false},
		{"read deadline", httpResult{err: &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}}, outAnomaly, kindHang, false},
		{"request deadline", httpResult{err: &url.Error{Op: "Get", URL: "http://kula.test/", Err: context.DeadlineExceeded}}, outAnomaly, kindHang, false},
		{"a dial that timed out", httpResult{err: &url.Error{Op: "Get", URL: "http://kula.test/", Err: context.DeadlineExceeded}, unsent: true}, outUnsent, "", false},
		{"text only: reset", httpResult{err: errors.New("read: connection reset by peer")}, outAnomaly, kindDropped, false},
		{"text only: refused", httpResult{err: errors.New("dial tcp 127.0.0.1:1: connect: connection refused")}, outUnsent, "", true},
		{"something else", httpResult{err: errors.New("tls: failed to verify certificate")}, outFailed, "", false},
	}
	for _, c := range cases {
		got := classify(c.r)
		if got.outcome != c.want || got.why != c.why || got.refused != c.refuse {
			t.Errorf("%s: outcome %d, why %q, refused %v; want %d, %q, %v", c.name, got.outcome, got.why, got.refused, c.want, c.why, c.refuse)
		}
	}
}

// TestFuzzLetsDeliberate5xxThrough: a target answering each of Kula's
// deliberate 503 and 502 answers is not flagged, and the throttled and
// unavailable answers are counted apart from the answered ones.
func TestFuzzLetsDeliberate5xxThrough(t *testing.T) {
	m := newKulaMock(false, false)
	m.override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			return false
		}
		switch r.URL.Path {
		case "/api/current":
			kmJSONError(w, "no data yet", http.StatusServiceUnavailable)
		case "/api/history":
			w.Header().Set("Retry-After", "2")
			kmJSONError(w, "too many history requests in progress", http.StatusServiceUnavailable)
		case "/api/ollama/models":
			kmAIError(w, "backend_unavailable", "AI backend unavailable", http.StatusBadGateway)
		default:
			return false // the context route answers that the assistant is disabled
		}
		return true
	}
	s, _ := m.serve(t, Options{})

	for _, run := range []func(*Scanner) []Finding{runFuzzMethods, runFuzzQuery} {
		for _, f := range run(s) {
			if f.Status == StatusFail || f.Status == StatusError {
				t.Errorf("%s = %s against deliberate 5xx answers: %s | %s", f.ID, f.Status, f.Detail, f.Evidence)
			}
		}
	}
	query, _ := byID(runFuzzQuery(s), "FUZZ-QUERY")
	if !strings.Contains(query.Detail, "throttled") {
		t.Errorf("FUZZ-QUERY does not count the busy history answers as throttled: %s", query.Detail)
	}

	// Login's busy answer, with auth on, is throttling too.
	m = newKulaMock(true, false)
	m.override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/api/login" || r.Method != http.MethodPost {
			return false
		}
		w.Header().Set("Retry-After", "1")
		kmJSONError(w, "server busy", http.StatusServiceUnavailable)
		return true
	}
	s, _ = m.serve(t, Options{})
	body := runFuzzBody(s)
	wantStatus(t, body, "FUZZ-BODY", StatusSkip)
	if f, _ := byID(body, "FUZZ-BODY"); !strings.Contains(f.Detail, "busy") {
		t.Errorf("FUZZ-BODY does not say login was busy: %s", f.Detail)
	}
}

// TestFuzzFlagsA500AndAPanic: a plain 500 and a handler panic (net/http
// drops the connection) are anomalies.
func TestFuzzFlagsA500AndAPanic(t *testing.T) {
	for _, c := range []struct {
		name   string
		answer func(w http.ResponseWriter)
		want   string
	}{
		{"500", func(w http.ResponseWriter) { kmJSONError(w, "internal storage error", http.StatusInternalServerError) }, "HTTP 500"},
		{"panic", func(http.ResponseWriter) { panic("kula-scan test: a handler panics") }, kindDropped},
	} {
		m := newKulaMock(false, false)
		m.override = func(w http.ResponseWriter, r *http.Request) bool {
			if r.URL.Path != "/api/history" {
				return false
			}
			c.answer(w)
			return true
		}
		s, _ := m.serve(t, Options{FuzzIter: 20})
		f, _ := byID(runFuzzQuery(s), "FUZZ-QUERY")
		if f.Status != StatusFail || !strings.Contains(f.Evidence, c.want) {
			t.Errorf("%s: FUZZ-QUERY = %s, evidence %q; want a FAIL naming %q", c.name, f.Status, f.Evidence, c.want)
		}
	}
}
