package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---- the AI chat ---------------------------------------------------------------------

// chatKula answers /api/ollama/chat as handleOllamaChat does, behind Kula's
// origin check: limit requests at most from the scanner (0: no limit), the
// body read through a cap of capBytes (0: none). A body that decodes fails
// the test: Kula would start a chat with it.
type chatKula struct {
	t        *testing.T
	limit    int
	capBytes int64
	mu       sync.Mutex
	allowed  int
	requests int
}

func (c *chatKula) chat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	if !sameOriginPost(r) {
		http.Error(w, `{"error":"invalid origin"}`, http.StatusForbidden)
		return
	}
	c.mu.Lock()
	c.requests++
	limited := c.limit > 0 && c.allowed >= c.limit
	if !limited {
		c.allowed++
	}
	c.mu.Unlock()
	if limited {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"rate limit exceeded","code":"rate_limited"}`))
		return
	}
	body := r.Body
	if c.capBytes > 0 {
		body = http.MaxBytesReader(w, r.Body, c.capBytes)
	}
	var req struct {
		Prompt   string            `json:"prompt"`
		Messages []json.RawMessage `json:"messages"`
		Context  string            `json:"context"`
		Model    string            `json:"model"`
	}
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid request body","code":"bad_request"}`))
		return
	}
	c.t.Errorf("a chat body decoded: Kula would have started a chat")
	w.Header().Set("Content-Type", "text/event-stream")
}

// count returns how many chat requests arrived.
func (c *chatKula) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.requests
}

// authOffChat serves Kula with auth off and the assistant enabled, its chat
// answered by c.
func authOffChat(c *chatKula) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("kula is healthy"))
	})
	mux.HandleFunc("/api/auth/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSONResp(w, map[string]any{"auth_required": false})
	})
	mux.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		writeJSONResp(w, map[string]any{"auth_enabled": false, "ollama_enabled": true})
	})
	mux.HandleFunc(chatPath, c.chat)
	return mux
}

// authOnChat serves the secure mock (auth on) with the assistant enabled:
// /api/config says so to a session, and the chat, answered by c, needs the
// session and its CSRF token.
func authOnChat(c *chatKula) http.Handler {
	mock := secureMock()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/config" && hasValidSession(r):
			writeJSONResp(w, map[string]any{"auth_enabled": true, "ollama_enabled": true})
		case r.URL.Path == chatPath && !hasValidSession(r):
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		case r.URL.Path == chatPath && r.Header.Get("X-CSRF-Token") != "csrftok":
			http.Error(w, `{"error":"invalid csrf token"}`, http.StatusForbidden)
		case r.URL.Path == chatPath:
			c.chat(w, r)
		default:
			mock.ServeHTTP(w, r)
		}
	})
}

// limitScanner scans handler, discovered, with credentials when user is set.
func limitScanner(t *testing.T, handler http.Handler, user string, timeout time.Duration) *Scanner {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	s, err := NewScanner(Options{Target: ts.URL, Username: user, Password: mockPass, Timeout: timeout, Aggressive: true})
	if err != nil {
		t.Fatal(err)
	}
	s.discover()
	return s
}

func TestChatRateLimit(t *testing.T) {
	for _, c := range []struct {
		name    string
		chat    *chatKula
		auth    bool
		want    Status
		detail  string
		maxSent int
	}{
		{"throttled after ten", &chatKula{limit: 10}, false, StatusPass, "429 at request 11, after letting 10 through", 11},
		{"throttled after ten, with auth", &chatKula{limit: 10}, true, StatusPass, "429 at request 11", 11},
		{"the budget already spent", &chatKula{limit: 10, allowed: 10}, false, StatusPass, "first chat request was already answered 429", 1},
		{"never throttled", &chatKula{}, false, StatusWarn, "14 chat requests within a minute were never answered 429", 14},
	} {
		c.chat.t = t
		handler, user := authOffChat(c.chat), ""
		if c.auth {
			handler, user = authOnChat(c.chat), mockUser
		}
		f, _ := byID(runChatRateCheck(limitScanner(t, handler, user, 5*time.Second)), "RATE-CHAT")
		if f.Status != c.want || !strings.Contains(f.Detail, c.detail) {
			t.Errorf("%s: RATE-CHAT = %s %q, want %s saying %q", c.name, f.Status, f.Detail, c.want, c.detail)
		}
		if n := c.chat.count(); n != c.maxSent {
			t.Errorf("%s: %d chat requests, want %d", c.name, n, c.maxSent)
		}
	}
}

func TestChatProbesSkipWithoutTheAssistantOrASession(t *testing.T) {
	var loginHits, chatHits atomic.Int64
	off := limitScanner(t, authOffMock(false, &loginHits, &chatHits), "", 5*time.Second)
	noSession := limitScanner(t, authOnChat(&chatKula{t: t}), "", 5*time.Second)
	for _, run := range []func(*Scanner) []Finding{runChatRateCheck, runChatBodyCapCheck} {
		if f := run(off)[0]; f.Status != StatusSkip || !strings.Contains(f.Detail, "not enabled") {
			t.Errorf("assistant off: %s = %s %q", f.ID, f.Status, f.Detail)
		}
		if f := run(noSession)[0]; f.Status != StatusSkip || !strings.Contains(f.Detail, "-username/-password") {
			t.Errorf("no session: %s = %s %q", f.ID, f.Status, f.Detail)
		}
	}
	if chatHits.Load() != 0 {
		t.Errorf("%d chat requests sent to an instance without the assistant", chatHits.Load())
	}
}

// loggedWriter wraps a ResponseWriter as Kula's logging middleware does
// (statusResponseWriter), which hides it from http.MaxBytesReader: the
// server is not told to close the connection, and drains what it can of the
// unread body before it answers.
type loggedWriter struct {
	http.ResponseWriter
	status int
}

func (w *loggedWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&loggedWriter{ResponseWriter: w}, r)
	})
}

func TestChatBodyCap(t *testing.T) {
	for _, c := range []struct {
		name    string
		chat    *chatKula
		auth    bool
		logging bool
		want    Status
		detail  string
	}{
		{"capped at 32 KiB", &chatKula{capBytes: chatMaxBody}, false, false, StatusPass, "320 KiB chat body was refused with HTTP 400 before it was complete"},
		{"capped, behind Kula's logging", &chatKula{capBytes: chatMaxBody}, false, true, StatusPass, "before it was complete"},
		{"capped, with auth", &chatKula{capBytes: chatMaxBody}, true, true, StatusPass, "before it was complete"},
		{"no cap", &chatKula{}, false, true, StatusWarn, "only once the scanner had sent all of it"},
		{"rate-limited", &chatKula{limit: 1, allowed: 1}, false, true, StatusSkip, "budget"},
	} {
		c.chat.t = t
		handler, user := authOffChat(c.chat), ""
		if c.auth {
			handler, user = authOnChat(c.chat), mockUser
		}
		if c.logging {
			handler = withLogging(handler)
		}
		f, _ := byID(runChatBodyCapCheck(limitScanner(t, handler, user, time.Second)), "INPUT-CHAT")
		if f.Status != c.want || !strings.Contains(f.Detail, c.detail) {
			t.Errorf("%s: INPUT-CHAT = %s %q | %s, want %s saying %q", c.name, f.Status, f.Detail, f.Evidence, c.want, c.detail)
		}
	}
}

// ---- history burst -------------------------------------------------------------------

// historyKula serves Kula with auth off whose /api/history runs two queries
// at a time, each taking query, and keeps a request waiting for a slot up to
// slotWait before it answers 503 with Retry-After, as Kula does; answer,
// when set, answers instead. /health is answered by health when set.
func historyKula(query, slotWait time.Duration, answer, health http.HandlerFunc) http.Handler {
	slots := make(chan struct{}, 2)
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if health != nil {
			health(w, r)
			return
		}
		_, _ = w.Write([]byte("kula is healthy"))
	})
	mux.HandleFunc("/api/auth/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSONResp(w, map[string]any{"auth_required": false})
	})
	mux.HandleFunc("/api/history", func(w http.ResponseWriter, r *http.Request) {
		if answer != nil {
			answer(w, r)
			return
		}
		select {
		case slots <- struct{}{}:
		case <-time.After(slotWait):
			w.Header().Set("Retry-After", "2")
			http.Error(w, `{"error":"too many history requests in progress"}`, http.StatusServiceUnavailable)
			return
		}
		defer func() { <-slots }()
		time.Sleep(query)
		writeJSONResp(w, map[string]any{"samples": []any{}, "resolution": "1s"})
	})
	return mux
}

func TestHistoryBurst(t *testing.T) {
	defer func(wait time.Duration) { historyBurstWait = wait }(historyBurstWait)
	historyBurstWait = 0

	var seen sync.Map
	record := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/history" {
				seen.Store(r.URL.RawQuery, true)
			}
			next.ServeHTTP(w, r)
		})
	}
	hang := func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }
	var inFlight atomic.Int64
	for _, c := range []struct {
		name    string
		handler http.Handler
		want    Status
		detail  string
	}{
		{"all answered", historyKula(0, time.Second, nil, nil), StatusPass, "8 with data and 0 deferred"},
		{"some deferred", historyKula(400*time.Millisecond, 50*time.Millisecond, nil, nil), StatusPass, "2 with data and 6 deferred with 503 and Retry-After"},
		{"hung", historyKula(0, 0, hang, nil), StatusFail, "8 of 8 requests got no answer within 1s"},
		{"server errors", historyKula(0, 0, func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}, nil), StatusFail, "8 answered with a server error"},
		{"503 without Retry-After", historyKula(0, 0, func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "busy", http.StatusServiceUnavailable)
		}, nil), StatusWarn, "8 answered neither with data nor with Kula's 503"},
		{"/health starved", historyKula(0, 0, func(w http.ResponseWriter, r *http.Request) {
			inFlight.Add(1)
			defer inFlight.Add(-1)
			time.Sleep(600 * time.Millisecond)
		}, func(w http.ResponseWriter, r *http.Request) {
			if inFlight.Load() > 0 {
				<-r.Context().Done()
				return
			}
			_, _ = w.Write([]byte("kula is healthy"))
		}), StatusFail, "/health failed"},
	} {
		s := limitScanner(t, record(c.handler), "", time.Second)
		f, _ := byID(runHistoryBurstCheck(s), "DOS-HISTORY")
		if f.Status != c.want || !strings.Contains(f.Detail, c.detail) {
			t.Errorf("%s: DOS-HISTORY = %s %q | %s, want %s saying %q", c.name, f.Status, f.Detail, f.Evidence, c.want, c.detail)
		}
	}
	seen.Range(func(query, _ any) bool {
		q := query.(string)
		if !strings.Contains(q, "points=5000") || !strings.Contains(q, "sections=lavg") {
			t.Errorf("a burst request asked %q", q)
		}
		return true
	})

	// A request is not hung for taking longer than -timeout while Kula keeps
	// it waiting for a slot: the burst waits historyBurstWait at least. Four
	// rounds of two 300 ms queries end after 1.2 s.
	historyBurstWait = 3 * time.Second
	s := limitScanner(t, historyKula(300*time.Millisecond, 3*time.Second, nil, nil), "", time.Second)
	if f, _ := byID(runHistoryBurstCheck(s), "DOS-HISTORY"); f.Status != StatusPass {
		t.Errorf("slow slots: DOS-HISTORY = %s %q | %s", f.Status, f.Detail, f.Evidence)
	}
}

func TestHistoryBurstNeedsASession(t *testing.T) {
	s := limitScanner(t, secureMock(), "", time.Second)
	if f, _ := byID(runHistoryBurstCheck(s), "DOS-HISTORY"); f.Status != StatusSkip || !strings.Contains(f.Detail, "-username/-password") {
		t.Errorf("DOS-HISTORY = %s %q", f.Status, f.Detail)
	}
	s = limitScanner(t, secureMock(), mockUser, 5*time.Second)
	if f, _ := byID(runHistoryBurstCheck(s), "DOS-HISTORY"); f.Status != StatusPass {
		t.Errorf("with a session: DOS-HISTORY = %s %q | %s", f.Status, f.Detail, f.Evidence)
	}
}

// TestAggressiveRunSpendsOneLogin: the limit checks share the run's session.
func TestAggressiveRunSpendsOneLogin(t *testing.T) {
	only, err := newSelection("INPUT-CHAT,RATE-CHAT,DOS-HISTORY", "")
	if err != nil {
		t.Fatal(err)
	}
	findings, ledger := scanWithLedger(t, true, only)
	for _, id := range []string{"INPUT-CHAT", "RATE-CHAT"} {
		wantStatus(t, findings, id, StatusSkip) // the secure mock's assistant is off
	}
	wantStatus(t, findings, "DOS-HISTORY", StatusPass)
	if got := ledger.attempts[mockUser]; len(got) != 1 {
		t.Errorf("logged in %d times, want once", len(got))
	}
}
