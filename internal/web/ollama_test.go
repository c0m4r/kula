package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kula/internal/config"
)

// mockOllamaServer creates a test HTTP server that responds like Ollama.
func mockOllamaServer(t *testing.T, response string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/api/chat") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// Write one chunk then done
		chunk := map[string]interface{}{
			"message": map[string]string{"content": response},
			"done":    false,
		}
		b, _ := json.Marshal(chunk)
		_, _ = fmt.Fprintln(w, string(b))
		done := map[string]interface{}{"message": map[string]string{"content": ""}, "done": true}
		b, _ = json.Marshal(done)
		_, _ = fmt.Fprintln(w, string(b))
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
		{"\x00\x00leading", 7, ""},                                   // null bytes stripped
		{strings.Repeat("a", 3000), ollamaMaxPrompt, ""},             // clamped
		{"  spaces  ", 6, ""},                                        // trimmed
		{"fine prompt\x00with\x00null", 19, "\x00"},                  // nulls removed
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
	err := client.streamChat(context.Background(), msgs, rec, rec, false)
	if err != nil {
		t.Fatalf("streamChat error: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "System looks healthy.") {
		t.Errorf("unexpected output: %q", body)
	}
}

// Ensure streamChat returns an error when Ollama is unreachable.
func TestOllamaStreamChat_Unreachable(t *testing.T) {
	cfg := config.OllamaConfig{Enabled: true, URL: "http://127.0.0.1:1", Model: "llama3", Timeout: "1s"}
	client := newOllamaClient(cfg)
	rec := httptest.NewRecorder()
	err := client.streamChat(context.Background(), nil, rec, rec, false)
	if err == nil {
		t.Error("expected error connecting to unreachable server, got nil")
	}
}
