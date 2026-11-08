package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"kula/internal/collector"
	"kula/internal/config"
	"kula/internal/storage"
)

// mockOllamaServer creates a test HTTP server that responds like Ollama.
// It serves both /api/chat (streaming) and /api/tags (model list).
func mockOllamaServer(t *testing.T, response string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/api/tags") && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"models": []map[string]string{
					{"name": "llama3:latest"},
					{"name": "mistral:latest"},
				},
			})
		case strings.HasSuffix(r.URL.Path, "/api/chat") && r.Method == http.MethodPost:
			w.Header().Set("Content-Type", "application/json")
			chunk := map[string]interface{}{
				"message": map[string]string{"content": response},
				"done":    false,
			}
			b, _ := json.Marshal(chunk)
			_, _ = fmt.Fprintln(w, string(b))
			done := map[string]interface{}{"message": map[string]string{"content": ""}, "done": true}
			b, _ = json.Marshal(done)
			_, _ = fmt.Fprintln(w, string(b))
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
}

func TestHandleOllamaChat_Disabled(t *testing.T) {
	srv := &Server{ollama: nil}
	req := httptest.NewRequest(http.MethodPost, "/api/ollama/chat", strings.NewReader(`{"prompt":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	srv.handleOllamaChat(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "not enabled") {
		t.Errorf("expected 'not enabled' in body, got %q", body)
	}
}

func TestHandleOllamaChat_WrongMethod(t *testing.T) {
	cfg := config.OllamaConfig{Enabled: true, URL: "http://localhost:11434", Model: "llama3", Timeout: "5s"}
	srv := &Server{ollama: newOllamaClient(cfg)}
	req := httptest.NewRequest(http.MethodGet, "/api/ollama/chat", nil)
	rr := httptest.NewRecorder()

	srv.handleOllamaChat(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rr.Code)
	}
}

func TestSanitisePrompt(t *testing.T) {
	tests := []struct {
		input    string
		wantLen  int
		wantNone string
	}{
		{"hello world", 11, ""},
		{"\x00\x00leading", 7, ""},                       // null bytes stripped
		{strings.Repeat("a", 3000), ollamaMaxPrompt, ""}, // clamped
		{"  spaces  ", 6, ""},                            // trimmed
		{"fine prompt\x00with\x00null", 19, "\x00"},      // nulls removed
	}
	for _, tt := range tests {
		got := sanitisePrompt(tt.input)
		if len([]rune(got)) > ollamaMaxPrompt {
			t.Errorf("sanitisePrompt(%q) length %d exceeds max %d", tt.input, len([]rune(got)), ollamaMaxPrompt)
		}
		if tt.wantLen > 0 && len([]rune(got)) != tt.wantLen {
			t.Errorf("sanitisePrompt(%q) = %q (len %d), want len %d", tt.input, got, len([]rune(got)), tt.wantLen)
		}
		if tt.wantNone != "" && strings.Contains(got, tt.wantNone) {
			t.Errorf("sanitisePrompt(%q) contains %q, should not", tt.input, tt.wantNone)
		}
	}
}

func TestBuildOllamaSystemPrompt_NoContext(t *testing.T) {
	srv := &Server{store: nil} // store nil handles panics if handled properly, wait store needs to be initialized.
	prompt := srv.buildOllamaSystemPrompt("", "")
	if !strings.Contains(strings.ToLower(prompt), "linux monitoring") {
		t.Errorf("system prompt missing expected header, got: %q", prompt)
	}
}

func TestBuildOllamaSystemPrompt_WithContext(t *testing.T) {
	srv := &Server{}
	csv := "chart: CPU\nTime,Usage\n10:00,50%"
	prompt := srv.buildOllamaSystemPrompt(csv, "")
	if !strings.Contains(prompt, "50%") {
		t.Errorf("expected context CSV in system prompt, got: %q", prompt)
	}
}

func TestOllamaStreamChat_Integration(t *testing.T) {
	mock := mockOllamaServer(t, "System looks healthy.")
	defer mock.Close()

	cfg := config.OllamaConfig{Enabled: true, URL: mock.URL, Model: "llama3", Timeout: "10s"}
	client := newOllamaClient(cfg)

	msgs := []ollamaMessage{{Role: "user", Content: "check system"}}

	rec := httptest.NewRecorder()
	err := client.streamChat(context.Background(), cfg.Model, msgs, nil, nil, rec, rec, false)
	if err != nil {
		t.Fatalf("streamChat error: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "System looks healthy.") {
		t.Errorf("unexpected output: %q", body)
	}
}

// TestOllamaProxyDoesNotFollowRedirects: the loopback-validated URL is the
// only place the proxy goes. A redirect is reported with its target, and the
// target never sees a request, for chat and model listing alike.
func TestOllamaProxyDoesNotFollowRedirects(t *testing.T) {
	var reached atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached.Add(1)
	}))
	defer target.Close()
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer redirecting.Close()

	cfg := config.OllamaConfig{Enabled: true, URL: redirecting.URL, Model: "llama3", Timeout: "5s"}
	client := newOllamaClient(cfg)
	rec := httptest.NewRecorder()
	err := client.streamChat(context.Background(), cfg.Model, []ollamaMessage{{Role: "user", Content: "hi"}}, nil, nil, rec, rec, false)
	if err == nil || !strings.Contains(err.Error(), "not followed") || !strings.Contains(err.Error(), target.URL) {
		t.Errorf("streamChat error = %v, want the refused redirect named", err)
	}
	if _, err := client.fetchModels(context.Background(), false); err == nil {
		t.Error("fetchModels followed a redirect")
	}
	if n := reached.Load(); n != 0 {
		t.Errorf("redirect target received %d request(s)", n)
	}
}

// Ensure streamChat returns an error when Ollama is unreachable.
func TestOllamaStreamChat_Unreachable(t *testing.T) {
	cfg := config.OllamaConfig{Enabled: true, URL: "http://127.0.0.1:1", Model: "llama3", Timeout: "1s"}
	client := newOllamaClient(cfg)
	rec := httptest.NewRecorder()
	err := client.streamChat(context.Background(), cfg.Model, nil, nil, nil, rec, rec, false)
	if err == nil {
		t.Error("expected error connecting to unreachable server, got nil")
	}
}

func TestHandleOllamaModels_Disabled(t *testing.T) {
	srv := &Server{ollama: nil}
	req := httptest.NewRequest(http.MethodGet, "/api/ollama/models", nil)
	rr := httptest.NewRecorder()
	srv.handleOllamaModels(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", rr.Code)
	}
}

func TestHandleOllamaModels_Integration(t *testing.T) {
	mock := mockOllamaServer(t, "")
	defer mock.Close()

	cfg := config.OllamaConfig{Enabled: true, URL: mock.URL, Model: "llama3", Timeout: "5s"}
	srv := &Server{ollama: newOllamaClient(cfg)}

	req := httptest.NewRequest(http.MethodGet, "/api/ollama/models", nil)
	rr := httptest.NewRecorder()
	srv.handleOllamaModels(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var result struct {
		Models []string `json:"models"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(result.Models) != 2 {
		t.Errorf("expected 2 models, got %d: %v", len(result.Models), result.Models)
	}
	if result.Models[0] != "llama3:latest" {
		t.Errorf("unexpected first model: %q", result.Models[0])
	}
}

func TestHandleOllamaChat_ModelOverride(t *testing.T) {
	mock := mockOllamaServer(t, "override ok")
	defer mock.Close()

	cfg := config.OllamaConfig{Enabled: true, URL: mock.URL, Model: "llama3", Timeout: "5s"}
	srv := &Server{ollama: newOllamaClient(cfg)}

	body := `{"prompt":"test","model":"mistral:latest"}`
	req := httptest.NewRequest(http.MethodPost, "/api/ollama/chat", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleOllamaChat(rr, req)

	if !strings.Contains(rr.Body.String(), "override ok") {
		t.Errorf("expected streamed response, got: %q", rr.Body.String())
	}
}

func TestHandleOllamaChat_ModelOverride_Rejected(t *testing.T) {
	// A model name containing shell metacharacters must be silently ignored;
	// the server should fall back to the configured model.
	var capturedModel atomic.Value
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/api/chat") {
			var body ollamaChatRequest
			_ = json.NewDecoder(r.Body).Decode(&body)
			capturedModel.Store(body.Model)
			done := map[string]interface{}{"message": map[string]string{"content": "ok"}, "done": true}
			b, _ := json.Marshal(done)
			_, _ = fmt.Fprintln(w, string(b))
			return
		}
		http.NotFound(w, r)
	}))
	defer mock.Close()

	cfg := config.OllamaConfig{Enabled: true, URL: mock.URL, Model: "llama3", Timeout: "5s"}
	srv := &Server{ollama: newOllamaClient(cfg)}

	body := `{"prompt":"test","model":"evil; rm -rf /"}`
	req := httptest.NewRequest(http.MethodPost, "/api/ollama/chat", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	srv.handleOllamaChat(httptest.NewRecorder(), req)

	if got := capturedModel.Load(); got != "llama3" {
		t.Errorf("expected malicious model override to be rejected; got %q", got)
	}
}

func TestOllamaModelNameRe(t *testing.T) {
	good := []string{
		"llama3",
		"llama3:latest",
		"library/llama3.1",
		"hf.co/user/model:Q4_K_M",
		"mistral-small_v2",
	}
	bad := []string{
		"",
		"model name with spaces",
		"evil; rm -rf /",
		"back`tick",
		"pipe|injection",
		strings.Repeat("a", 201),
	}
	for _, s := range good {
		if !ollamaModelNameRe.MatchString(s) {
			t.Errorf("expected %q to be accepted", s)
		}
	}
	for _, s := range bad {
		if ollamaModelNameRe.MatchString(s) {
			t.Errorf("expected %q to be rejected", s)
		}
	}
}

func TestHandleOllamaContext_Disabled(t *testing.T) {
	srv := &Server{ollama: nil}
	req := httptest.NewRequest(http.MethodGet, "/api/ollama/context", nil)
	rr := httptest.NewRecorder()
	srv.handleOllamaContext(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", rr.Code)
	}
}

func TestHandleOllamaContext_NoStore(t *testing.T) {
	cfg := config.OllamaConfig{Enabled: true, URL: "http://127.0.0.1:1", Model: "llama3", Timeout: "1s"}
	srv := &Server{ollama: newOllamaClient(cfg), ollamaMetaLim: newMetaRateLimiter()}
	req := httptest.NewRequest(http.MethodGet, "/api/ollama/context", nil)
	rr := httptest.NewRecorder()
	srv.handleOllamaContext(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var out map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := out["context"]; !ok {
		t.Errorf("expected context field, got %v", out)
	}
}

func TestHandleOllamaContext_RateLimit(t *testing.T) {
	cfg := config.OllamaConfig{Enabled: true, URL: "http://127.0.0.1:1", Model: "llama3", Timeout: "1s"}
	srv := &Server{
		ollama:        newOllamaClient(cfg),
		ollamaMetaLim: &chatRateLimiter{limit: 1, requests: map[string][]time.Time{}},
	}
	// First request passes, second is rate-limited.
	for i, want := range []int{http.StatusOK, http.StatusTooManyRequests} {
		req := httptest.NewRequest(http.MethodGet, "/api/ollama/context", nil)
		rr := httptest.NewRecorder()
		srv.handleOllamaContext(rr, req)
		if rr.Code != want {
			t.Errorf("call %d: expected %d, got %d", i, want, rr.Code)
		}
	}
}

// ---- Tool-loop integration ------------------------------------------------

func TestOllamaStreamChat_ToolLoop(t *testing.T) {
	var round int32
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/api/chat") {
			http.NotFound(w, r)
			return
		}
		cur := atomic.AddInt32(&round, 1)
		w.Header().Set("Content-Type", "application/json")
		if cur == 1 {
			// Round 1: emit a single chunk containing a tool call.
			chunk := map[string]interface{}{
				"message": map[string]interface{}{
					"content": "",
					"tool_calls": []map[string]interface{}{{
						"function": map[string]interface{}{
							"name":      "get_metrics",
							"arguments": json.RawMessage(`{"resource":"cpu"}`),
						},
					}},
				},
				"done": true,
			}
			b, _ := json.Marshal(chunk)
			_, _ = fmt.Fprintln(w, string(b))
			return
		}
		// Round 2: stream a plain-text response and finish.
		chunk := map[string]interface{}{"message": map[string]string{"content": "cpu fine"}, "done": false}
		b, _ := json.Marshal(chunk)
		_, _ = fmt.Fprintln(w, string(b))
		done := map[string]interface{}{"message": map[string]string{"content": ""}, "done": true}
		b, _ = json.Marshal(done)
		_, _ = fmt.Fprintln(w, string(b))
	}))
	defer mock.Close()

	cfg := config.OllamaConfig{Enabled: true, URL: mock.URL, Model: "llama3", Timeout: "5s"}
	client := newOllamaClient(cfg)

	var toolCalls int32
	executor := func(name string, args json.RawMessage) string {
		atomic.AddInt32(&toolCalls, 1)
		return "Time,CPU%\n10:00,42.0\n"
	}

	tools := []ollamaToolDef{metricsToolDef()}
	rec := httptest.NewRecorder()
	err := client.streamChat(context.Background(), cfg.Model, []ollamaMessage{{Role: "user", Content: "how's cpu?"}}, tools, executor, rec, rec, false)
	if err != nil {
		t.Fatalf("streamChat: %v", err)
	}
	if got := atomic.LoadInt32(&toolCalls); got != 1 {
		t.Errorf("expected 1 tool invocation, got %d", got)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "cpu fine") {
		t.Errorf("expected final-round text in output, got %q", body)
	}
	if !strings.Contains(body, "event: tool_call") {
		t.Errorf("expected tool_call SSE event in output, got %q", body)
	}
}

// ---- Tool data formatting -------------------------------------------------

func mkSample(cpuUsage float64) *storage.AggregatedSample {
	return &storage.AggregatedSample{
		Timestamp: time.Date(2026, 4, 16, 10, 0, 0, 0, time.UTC),
		Data: &collector.Sample{
			CPU:    collector.CPUStats{Total: collector.CPUCoreStats{Usage: cpuUsage}},
			Memory: collector.MemoryStats{Total: 16 << 30, Used: 8 << 30, UsedPercent: 50},
		},
	}
}

func TestFormatMetricsData_Unknown(t *testing.T) {
	out := formatMetricsData("not-a-thing", nil)
	if !strings.Contains(out, "unknown resource type") {
		t.Errorf("unexpected output: %q", out)
	}
}

func TestFormatMetricsData_CPU(t *testing.T) {
	out := formatMetricsData("cpu", []*storage.AggregatedSample{mkSample(42)})
	if !strings.HasPrefix(out, "Time,CPU%\n") {
		t.Errorf("expected CPU header, got %q", out)
	}
	if !strings.Contains(out, "42.0") {
		t.Errorf("expected sample value in output, got %q", out)
	}
}

func TestFormatMetricsData_NetworkTruncation(t *testing.T) {
	ifaces := make([]collector.NetInterface, ollamaToolMaxItems+5)
	for i := range ifaces {
		ifaces[i] = collector.NetInterface{Name: fmt.Sprintf("eth%d", i), RxMbps: float64(i), TxMbps: 0}
	}
	s := &storage.AggregatedSample{
		Timestamp: time.Date(2026, 4, 16, 10, 0, 0, 0, time.UTC),
		Data:      &collector.Sample{Network: collector.NetworkStats{Interfaces: ifaces}},
	}
	out := formatMetricsData("network", []*storage.AggregatedSample{s})
	rows := strings.Count(out, "\n") - 1 // subtract header
	if rows != ollamaToolMaxItems {
		t.Errorf("expected %d rows, got %d; output:\n%s", ollamaToolMaxItems, rows, out)
	}
	// eth10 is the first one that should be truncated.
	if strings.Contains(out, fmt.Sprintf("eth%d", ollamaToolMaxItems)) {
		t.Errorf("expected eth%d to be truncated, got %q", ollamaToolMaxItems, out)
	}
}

func TestExecuteGetMetrics_NoStore(t *testing.T) {
	srv := &Server{}
	out := srv.executeGetMetrics(json.RawMessage(`{"resource":"cpu"}`))
	if !strings.Contains(out, "no storage available") {
		t.Errorf("expected no-store error, got %q", out)
	}
}

func TestExecuteGetMetrics_InvalidArgs(t *testing.T) {
	srv := &Server{}
	out := srv.executeGetMetrics(json.RawMessage(`{"resource":`))
	if !strings.Contains(out, "invalid arguments") {
		t.Errorf("expected invalid-args error, got %q", out)
	}
}

// TestExecuteGetMetricsRangeLimits: the AI tool is held to the history API's
// limits, and an unreadable bound is reported instead of being replaced by
// the default hour.
func TestExecuteGetMetricsRangeLimits(t *testing.T) {
	srv := &Server{}
	for args, want := range map[string]string{
		`{"resource":"cpu","from":"-1h","to":"-2h"}`:                                   "time range inverted",
		`{"resource":"cpu","from":"-800h"}`:                                            "maximum is 31 days",
		`{"resource":"cpu","from":"2026-01-01T00:00:00Z","to":"2026-03-01T00:00:00Z"}`: "maximum is 31 days",
		`{"resource":"cpu","from":"yesterday"}`:                                        "invalid 'from' time",
		`{"resource":"cpu","to":"soon"}`:                                               "invalid 'to' time",
		// Within the limits the query reaches the store (absent here).
		`{"resource":"cpu","to":"2026-03-01T00:00:00Z"}`: "no storage available",
		`{"resource":"cpu","from":"-744h"}`:              "no storage available",
	} {
		if out := srv.executeGetMetrics(json.RawMessage(args)); !strings.Contains(out, want) {
			t.Errorf("%s: got %q, want %q", args, out, want)
		}
	}
}

func TestChatRateLimiterCapsDistinctKeys(t *testing.T) {
	rl := &chatRateLimiter{limit: ollamaChatRateLimit, requests: make(map[string][]time.Time)}

	for i := 0; i < maxRateLimiterKeys; i++ {
		if !rl.Allow(fmt.Sprintf("ip-%d", i)) {
			t.Fatalf("key %d should be allowed while under the cap", i)
		}
	}

	// Saturated with fresh entries: a new IP is rejected so the map cannot grow
	// without bound (this limiter was previously never purged).
	if rl.Allow("overflow-ip") {
		t.Fatal("new key should be denied when the chat limiter is saturated")
	}
	if len(rl.requests) > maxRateLimiterKeys {
		t.Fatalf("map grew past cap: got %d keys, want <= %d", len(rl.requests), maxRateLimiterKeys)
	}
}

func TestChatRateLimiterPurgeStaleReclaims(t *testing.T) {
	rl := newChatRateLimiter()
	rl.requests["old-ip"] = []time.Time{time.Now().Add(-2 * time.Minute)}
	rl.requests["fresh-ip"] = []time.Time{time.Now()}

	rl.purgeStale()

	if _, ok := rl.requests["old-ip"]; ok {
		t.Error("stale entry should have been purged")
	}
	if _, ok := rl.requests["fresh-ip"]; !ok {
		t.Error("fresh entry should have been retained")
	}
}
