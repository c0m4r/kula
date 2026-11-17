package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// kulaMock emulates what the fuzz probes meet in Kula's web server
// (internal/web): the routes and their method checks (readOnly), the auth
// gate in front of the API, the CSRF gate (a same-origin Origin on every
// state change, and the session's token on an authenticated one), the two
// login limiters, the AI chat limiter, and the login and chat handlers'
// body decoding. It records what reached the handlers, so a test can tell
// what the scanner sent there.
type kulaMock struct {
	auth, ollama bool
	// laxMethods makes the GET-only API routes answer 200 to any method,
	// as a server without readOnly would; static routes always do.
	laxMethods bool
	// loginLimit and chatLimit are the per-address limits (Kula: 15 logins
	// in five minutes, 10 chats a minute); a test sets fewer to begin
	// partly spent.
	loginLimit, chatLimit int
	// override, when set, answers an API request before the handler does,
	// after the gates; it returns false to let the handler answer.
	override func(w http.ResponseWriter, r *http.Request) bool

	mu            sync.Mutex
	loginAttempts int            // POSTs to /api/login past CSRF, counted by the address limiter
	accountTries  map[string]int // decoded logins per username
	loginStatuses []int          // what each login POST was answered
	chatBodies    []string       // bodies that reached the chat decoder
	chatDecoded   []string       // of those, bodies that decode: a real chat would have started
	logoutCookies []string       // the Cookie header of every request to /api/logout
	seen          []kulaMockRequest
}

type kulaMockRequest struct {
	method, path, query string
	header              http.Header
}

func (m *kulaMock) requests() []kulaMockRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]kulaMockRequest(nil), m.seen...)
}

const (
	kmUser    = "operator"
	kmPass    = "correct-horse-battery"
	kmSession = "km-session-token"
	kmCSRF    = "km-csrf-token"
)

func newKulaMock(auth, ollama bool) *kulaMock {
	return &kulaMock{auth: auth, ollama: ollama, loginLimit: 15, chatLimit: 10, accountTries: map[string]int{}}
}

// serve starts the mock and returns a scanner for it, after discovery.
func (m *kulaMock) serve(t *testing.T, opts Options) (*Scanner, *httptest.Server) {
	t.Helper()
	ts := httptest.NewUnstartedServer(m)
	// A test may make a handler panic; net/http's stack trace is noise.
	ts.Config.ErrorLog = log.New(io.Discard, "", 0)
	ts.Start()
	t.Cleanup(ts.Close)
	opts.Target = ts.URL
	if opts.Timeout == 0 {
		opts.Timeout = 5 * time.Second
	}
	if opts.Seed == 0 {
		opts.Seed = 1
	}
	if opts.FuzzIter == 0 {
		opts.FuzzIter = 40
	}
	opts.Fuzz = true
	s, err := NewScanner(opts)
	if err != nil {
		t.Fatal(err)
	}
	s.discover()
	return s, ts
}

func kmJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func kmJSONError(w http.ResponseWriter, msg string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	b, _ := json.Marshal(map[string]string{"error": msg})
	_, _ = w.Write(b)
}

func kmAIError(w http.ResponseWriter, code, msg string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	b, _ := json.Marshal(map[string]string{"error": msg, "code": code})
	_, _ = w.Write(b)
}

func (m *kulaMock) validSession(r *http.Request) bool {
	c, err := r.Cookie("kula_session")
	return err == nil && c.Value == kmSession
}

func kmSameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		origin = r.Header.Get("Referer")
	}
	u, err := url.Parse(origin)
	return origin != "" && err == nil && strings.EqualFold(u.Host, r.Host)
}

func (m *kulaMock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	m.seen = append(m.seen, kulaMockRequest{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone()})
	if r.URL.Path == "/api/logout" {
		m.logoutCookies = append(m.logoutCookies, r.Header.Get("Cookie"))
	}
	m.mu.Unlock()

	path := r.URL.Path
	switch {
	case path == "/health" || path == "/status":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "kula is healthy")
		return
	case path == "/metrics":
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = io.WriteString(w, "kula_up 1\n")
		return
	case path == "/ws":
		m.websocket(w, r)
		return
	case strings.HasPrefix(path, "/api/"):
		public := path == "/api/login" || path == "/api/logout" || path == "/api/auth/status"
		if m.auth && !public && !m.validSession(r) {
			kmJSONError(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			if !kmSameOrigin(r) {
				kmJSONError(w, "invalid origin", http.StatusForbidden)
				return
			}
			if m.auth && m.validSession(r) && r.Header.Get("X-CSRF-Token") != kmCSRF {
				kmJSONError(w, "invalid csrf token", http.StatusForbidden)
				return
			}
		}
		if m.override != nil && m.override(w, r) {
			return
		}
		m.api(w, r)
		return
	}
	// The UI: the index page and static assets answer any method.
	switch path {
	case "/", "/index.html":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, "<html><body>kula</body></html>")
	case "/style.css":
		w.Header().Set("Content-Type", "text/css")
		_, _ = io.WriteString(w, "body{}")
	default:
		http.Error(w, "Not Found", http.StatusNotFound)
	}
}

func (m *kulaMock) websocket(w http.ResponseWriter, r *http.Request) {
	if m.auth && !m.validSession(r) {
		kmJSONError(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	up := websocket.Upgrader{CheckOrigin: kmSameOrigin}
	c, err := up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c.SetReadLimit(4096)
	go func() {
		defer func() { _ = c.Close() }()
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	}()
}

func (m *kulaMock) api(w http.ResponseWriter, r *http.Request) {
	readOnly := func(serve func()) {
		if !m.laxMethods && r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			kmJSONError(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		serve()
	}
	postOnly := func(serve func()) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			kmJSONError(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		serve()
	}
	ok := func(v any) func() {
		return func() { kmJSON(w, v) }
	}
	aiRoute := func(v any) func() {
		return func() {
			if !m.ollama {
				kmAIError(w, "disabled", "ollama is not enabled", http.StatusServiceUnavailable)
				return
			}
			kmJSON(w, v)
		}
	}

	switch r.URL.Path {
	case "/api/current", "/api/system-info":
		readOnly(ok(map[string]any{"cpu": 1}))
	case "/api/config":
		readOnly(ok(map[string]any{"auth_enabled": m.auth, "ollama_enabled": m.ollama}))
	case "/api/auth/status":
		readOnly(ok(map[string]any{"auth_required": m.auth, "authenticated": !m.auth || m.validSession(r)}))
	case "/api/history":
		readOnly(func() { m.history(w, r) })
	case "/api/i18n":
		readOnly(func() {
			if lang := r.URL.Query().Get("lang"); lang != "" && lang != "en" && lang != "de" {
				kmJSONError(w, "invalid language", http.StatusBadRequest)
				return
			}
			kmJSON(w, map[string]any{"hello": "hi"})
		})
	case "/api/ollama/models":
		readOnly(aiRoute(map[string]any{"models": []string{"m"}}))
	case "/api/ollama/context":
		readOnly(aiRoute(map[string]any{"context": ""}))
	case "/api/login":
		if !m.auth {
			kmJSONError(w, "not found", http.StatusNotFound)
			return
		}
		postOnly(func() { m.login(w, r) })
	case "/api/logout":
		postOnly(ok(map[string]any{"status": "logged out"}))
	case "/api/ollama/chat":
		postOnly(func() { m.chat(w, r) })
	default:
		kmJSONError(w, "not found", http.StatusNotFound)
	}
}

func (m *kulaMock) history(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	for _, key := range []string{"from", "to"} {
		if v := q.Get(key); v != "" {
			if _, err := time.Parse(time.RFC3339, v); err != nil {
				kmJSONError(w, "invalid '"+key+"' time", http.StatusBadRequest)
				return
			}
		}
	}
	if tier := q.Get("tier"); tier != "" && strings.Trim(tier, "0123456789") != "" {
		kmJSONError(w, "invalid 'tier'", http.StatusBadRequest)
		return
	}
	kmJSON(w, map[string]any{"samples": []any{}, "tier": 0})
}

// login is handleLogin: the address limiter counts every attempt, then the
// body is decoded under a 4 KiB cap, then the account limiter counts it.
func (m *kulaMock) login(w http.ResponseWriter, r *http.Request) {
	status := func(code int) {
		m.mu.Lock()
		m.loginStatuses = append(m.loginStatuses, code)
		m.mu.Unlock()
	}
	m.mu.Lock()
	m.loginAttempts++
	refused := m.loginAttempts > m.loginLimit
	m.mu.Unlock()
	if refused {
		status(http.StatusTooManyRequests)
		kmJSONError(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	var creds struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&creds); err != nil {
		status(http.StatusBadRequest)
		kmJSONError(w, "invalid request", http.StatusBadRequest)
		return
	}
	m.mu.Lock()
	m.accountTries[creds.Username]++
	tooMany := m.accountTries[creds.Username] > 5
	m.mu.Unlock()
	if tooMany {
		status(http.StatusTooManyRequests)
		kmJSONError(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	if creds.Username != kmUser || creds.Password != kmPass {
		status(http.StatusUnauthorized)
		kmJSONError(w, "invalid credentials", http.StatusUnauthorized)
		return
	}
	status(http.StatusOK)
	http.SetCookie(w, &http.Cookie{Name: "kula_session", Value: kmSession, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
	kmJSON(w, map[string]string{"status": "logged in", "csrf_token": kmCSRF})
}

// kulaChatRequest mirrors internal/web's chatRequest, tool calls taken as
// raw JSON: it decodes whatever Kula's decodes.
type kulaChatRequest struct {
	Prompt   string `json:"prompt"`
	Messages []struct {
		Role       string          `json:"role"`
		Content    string          `json:"content,omitempty"`
		ToolCalls  json.RawMessage `json:"tool_calls,omitempty"`
		ToolCallID string          `json:"tool_call_id,omitempty"`
	} `json:"messages"`
	Context string `json:"context"`
	Lang    string `json:"lang"`
	Model   string `json:"model"`
}

// decodesAsChat reports whether body decodes into a chat request the way
// handleOllamaChat decodes it, size cap aside: a body that does would start
// a real chat with the model.
func decodesAsChat(body []byte) bool {
	return json.NewDecoder(bytes.NewReader(body)).Decode(&kulaChatRequest{}) == nil
}

// chat is handleOllamaChat up to the model: the limiter, then the body
// decoded under a 32 KiB cap. A body that decodes is recorded: Kula would
// have started a chat with it.
func (m *kulaMock) chat(w http.ResponseWriter, r *http.Request) {
	if !m.ollama {
		kmAIError(w, "disabled", "ollama is not enabled", http.StatusServiceUnavailable)
		return
	}
	m.mu.Lock()
	m.chatLimit--
	refused := m.chatLimit < 0
	m.mu.Unlock()
	if refused {
		kmAIError(w, "rate_limited", "rate limit exceeded", http.StatusTooManyRequests)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	m.mu.Lock()
	m.chatBodies = append(m.chatBodies, string(body))
	if decodesAsChat(body) {
		m.chatDecoded = append(m.chatDecoded, string(body))
	}
	m.mu.Unlock()
	var req kulaChatRequest
	if len(body) > 32*1024 || json.NewDecoder(bytes.NewReader(body)).Decode(&req) != nil {
		kmAIError(w, "bad_request", "invalid request body", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, "event: done\ndata: \"\"\n\n")
}
