package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"kula/internal/collector"
	"kula/internal/config"
)

// readOnlyRoutes are the API routes that only read: GET and HEAD.
var readOnlyRoutes = []string{
	"/api/current",
	"/api/system-info",
	"/api/history",
	"/api/config",
	"/api/auth/status",
	"/api/i18n?lang=en",
	"/api/ollama/models",
	"/api/ollama/context",
}

type methodResult struct {
	status int
	header http.Header
	body   string
}

func doMethod(t *testing.T, ts *httptest.Server, method, path string, header map[string]string) methodResult {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, nil)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, path, err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return methodResult{status: resp.StatusCode, header: resp.Header, body: string(raw)}
}

// wantMethodNotAllowed checks a JSON 405 whose Allow header names allow.
func wantMethodNotAllowed(t *testing.T, name string, r methodResult, allow string) {
	t.Helper()
	if r.status != http.StatusMethodNotAllowed {
		t.Errorf("%s: status = %d, want 405 (body %.80q)", name, r.status, r.body)
		return
	}
	if got := r.header.Get("Allow"); got != allow {
		t.Errorf("%s: Allow = %q, want %q", name, got, allow)
	}
	if ct := r.header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("%s: Content-Type = %q, want application/json", name, ct)
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(r.body), &body); err != nil || body.Error == "" {
		t.Errorf("%s: body %q is not a JSON error (%v)", name, r.body, err)
	}
}

// TestReadOnlyRoutesRefuseOtherMethods: a read-only API route answers GET and
// HEAD as before and any other method with a JSON 405 naming GET and HEAD,
// instead of serving its payload to a DELETE or a POST. CORS preflights keep
// the answers the CORS middleware gives them.
func TestReadOnlyRoutesRefuseOtherMethods(t *testing.T) {
	store := newTestStoreWithSample(t)
	defer func() { _ = store.Close() }()
	const allowedOrigin = "https://allowed.example"
	cfg := config.WebConfig{
		Enabled: true, UI: true, MaxWebsocketConns: 10, MaxWebsocketConnsPerIP: 2,
		Security: config.SecurityConfig{OriginValidation: true, AllowedOrigins: []string{allowedOrigin}},
	}
	c := collector.New(config.GlobalConfig{}, config.CollectionConfig{}, config.ApplicationsConfig{}, t.TempDir())
	s := NewServer(cfg, config.GlobalConfig{ShowSystemInfo: true}, c, store, t.TempDir(), config.OllamaConfig{})
	ts := httptest.NewServer(s.buildHandler())
	defer ts.Close()
	sameOrigin := map[string]string{"Origin": ts.URL}

	for _, path := range readOnlyRoutes {
		get := doMethod(t, ts, http.MethodGet, path, nil)
		if get.status == http.StatusMethodNotAllowed {
			t.Errorf("GET %s = 405", path)
		}
		head := doMethod(t, ts, http.MethodHead, path, nil)
		if head.status != get.status || head.body != "" {
			t.Errorf("HEAD %s = %d with %d body bytes, want %d with none", path, head.status, len(head.body), get.status)
		}
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
			wantMethodNotAllowed(t, method+" "+path, doMethod(t, ts, method, path, sameOrigin), "GET, HEAD")
		}

		preflight := doMethod(t, ts, http.MethodOptions, path, map[string]string{
			"Origin": allowedOrigin, "Access-Control-Request-Method": http.MethodGet,
		})
		if preflight.status != http.StatusNoContent || preflight.header.Get("Access-Control-Allow-Origin") != allowedOrigin {
			t.Errorf("preflight %s from an allowed origin = %d, ACAO %q; want 204, %q",
				path, preflight.status, preflight.header.Get("Access-Control-Allow-Origin"), allowedOrigin)
		}
		foreign := doMethod(t, ts, http.MethodOptions, path, map[string]string{
			"Origin": "https://evil.example", "Access-Control-Request-Method": http.MethodGet,
		})
		if foreign.status != http.StatusForbidden || foreign.header.Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("preflight %s from a foreign origin = %d, ACAO %q; want 403 and none",
				path, foreign.status, foreign.header.Get("Access-Control-Allow-Origin"))
		}
	}

	// The POST-only routes name POST.
	for _, path := range []string{"/api/logout", "/api/ollama/chat"} {
		wantMethodNotAllowed(t, "GET "+path, doMethod(t, ts, http.MethodGet, path, nil), http.MethodPost)
	}
}

// TestMethodCheckFollowsAuthAndCSRF: with auth on, the 405 is given only to a
// request auth and CSRF admit; the others keep the 401 or 403 they had, so the
// method check tells an anonymous client nothing new.
func TestMethodCheckFollowsAuthAndCSRF(t *testing.T) {
	_, ts := newSecuredTestServer(t, nil)

	if r := doMethod(t, ts, http.MethodDelete, "/api/config", map[string]string{"Origin": ts.URL}); r.status != http.StatusUnauthorized {
		t.Errorf("anonymous DELETE /api/config = %d, want 401", r.status)
	}
	res := login(t, ts, secUser, secPass)
	if res.session == "" {
		t.Fatal("setup login failed")
	}
	cookie := "kula_session=" + res.session
	if r := doMethod(t, ts, http.MethodDelete, "/api/config", map[string]string{"Cookie": cookie}); r.status != http.StatusForbidden {
		t.Errorf("DELETE /api/config without Origin = %d, want 403", r.status)
	}
	wantMethodNotAllowed(t, "authenticated DELETE /api/config",
		doMethod(t, ts, http.MethodDelete, "/api/config", map[string]string{
			"Cookie": cookie, "Origin": ts.URL, "X-CSRF-Token": res.csrf,
		}), "GET, HEAD")
	wantMethodNotAllowed(t, "GET /api/login", doMethod(t, ts, http.MethodGet, "/api/login", nil), http.MethodPost)
}
