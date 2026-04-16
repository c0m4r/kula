package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"kula/internal/config"
)

const (
	// ollamaMaxPrompt is the maximum allowed prompt length in runes.
	ollamaMaxPrompt = 2000
	// ollamaMaxBody is the maximum request body size for /api/ollama/chat.
	ollamaMaxBody = 32 * 1024
	// ollamaDefaultTimeout is the fallback streaming timeout.
	ollamaDefaultTimeout = 120 * time.Second
	// ollamaMaxResponse is the maximum total bytes read from an Ollama stream.
	ollamaMaxResponse = 10 * 1024 * 1024 // 10 MB
	// ollamaChatRateLimit is the max requests per IP per minute.
	ollamaChatRateLimit = 10
)

// chatRateLimiter is a per-IP sliding-window rate limiter for the Ollama chat endpoint.
type chatRateLimiter struct {
	mu       sync.Mutex
	requests map[string][]time.Time
}

func newChatRateLimiter() *chatRateLimiter {
	return &chatRateLimiter{requests: make(map[string][]time.Time)}
}

// Allow returns true if the IP has made fewer than ollamaChatRateLimit requests in the last minute.
func (rl *chatRateLimiter) Allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-time.Minute)
	var recent []time.Time
	for _, t := range rl.requests[ip] {
		if t.After(cutoff) {
			recent = append(recent, t)
		}
	}
	if len(recent) >= ollamaChatRateLimit {
		return false
	}
	rl.requests[ip] = append(recent, now)
	return true
}

// ollamaClient proxies requests to a local Ollama instance.
type ollamaClient struct {
	cfg     config.OllamaConfig
	timeout time.Duration
}

// newOllamaClient parses the configured timeout and returns a ready client.
// Returns nil when ollama is disabled.
func newOllamaClient(cfg config.OllamaConfig) *ollamaClient {
	if !cfg.Enabled {
		return nil
	}
	d, err := time.ParseDuration(cfg.Timeout)
	if err != nil || d <= 0 {
		log.Printf("[Ollama] invalid timeout %q, using default %s", cfg.Timeout, ollamaDefaultTimeout) // [L1]
		d = ollamaDefaultTimeout
	}
	return &ollamaClient{cfg: cfg, timeout: d}
}

// ollamaChatRequest is the payload sent to Ollama /api/chat.
type ollamaChatRequest struct {
	Model    string          `json:"model"`
	Messages []ollamaMessage `json:"messages"`
	Stream   bool            `json:"stream"`
}

type ollamaMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ollamaChunk is one streaming response frame from Ollama.
type ollamaChunk struct {
	Message struct {
		Content string `json:"content"`
	} `json:"message"`
	Done bool `json:"done"`
}

// chatRequest is the JSON body accepted by /api/ollama/chat.
type chatRequest struct {
	Prompt   string          `json:"prompt"`
	Messages []ollamaMessage `json:"messages"` // prior conversation turns
	Context  string          `json:"context"`  // "current" or "chart:..."
	Lang     string          `json:"lang"`     // BCP 47 language code, e.g. "en", "de"
}

// sanitisePrompt strips null bytes and clamps to ollamaMaxPrompt.
func sanitisePrompt(s string) string {
	s = strings.ReplaceAll(s, "\x00", "")
	if utf8.RuneCountInString(s) > ollamaMaxPrompt {
		runes := []rune(s)
		s = string(runes[:ollamaMaxPrompt])
	}
	return strings.TrimSpace(s)
}

// handleOllamaChat is the HTTP handler for POST /api/ollama/chat.
// It accepts a JSON body, builds a conversation, and streams the Ollama
// response back as Server-Sent Events (text/event-stream).
func (s *Server) handleOllamaChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.ollama == nil || !s.ollama.cfg.Enabled {
		jsonError(w, "ollama is not enabled", http.StatusServiceUnavailable)
		return
	}

	ip := getClientIP(r, s.cfg.TrustProxy)
	if s.ollamaLimiter != nil && !s.ollamaLimiter.Allow(ip) {
		jsonError(w, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, ollamaMaxBody)
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	userPrompt := sanitisePrompt(req.Prompt)
	if userPrompt == "" {
		userPrompt = "Analyse the current server metrics and summarise the system health."
	}

	// Build message list: system prompt + prior turns + current user turn.
	systemContent := s.buildOllamaSystemPrompt(req.Context, req.Lang)
	messages := []ollamaMessage{
		{Role: "system", Content: systemContent},
	}
	// Append prior conversation history (cap at 20 turns to stay within context)
	if len(req.Messages) > 20 {
		req.Messages = req.Messages[len(req.Messages)-20:]
	}
	messages = append(messages, req.Messages...)

	// Only append the current prompt if it's not already at the end of the history.
	// This prevents duplication when the frontend sends the updated history.
	last := messages[len(messages)-1]
	if last.Role != "user" || last.Content != userPrompt {
		messages = append(messages, ollamaMessage{Role: "user", Content: userPrompt})
	}
	debugLog := s.cfg.Logging.Enabled && s.cfg.Logging.Level == "debug"

	// Stream Ollama response back as SSE.
	flusher, ok := w.(http.Flusher)
	if !ok {
		jsonError(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx proxy buffering

	// Disable write timeout for long-running SSE streams.
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})

	err := s.ollama.streamChat(r.Context(), messages, w, flusher, debugLog)
	if err != nil {
		// If headers already sent, we can only log the error.
		log.Printf("[Ollama] stream error: %v", err)
		// Signal the client that an error occurred mid-stream.
		_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", err.Error())
		flusher.Flush()
	}

	// Signal end of stream.
	_, _ = fmt.Fprintf(w, "event: done\ndata: \n\n")
	flusher.Flush()
}

// streamChat calls Ollama /api/chat in streaming mode and writes SSE frames
// to the provided ResponseWriter / Flusher.
func (oc *ollamaClient) streamChat(
	ctx context.Context,
	messages []ollamaMessage,
	w io.Writer,
	flusher http.Flusher,
	debugLog bool,
) error {
	reqBody := ollamaChatRequest{
		Model:    oc.cfg.Model,
		Messages: messages,
		Stream:   true,
	}
	if debugLog {
		msgJSON, _ := json.MarshalIndent(reqBody, "", "  ")
		log.Printf("[DEBUG] [Ollama] Full Request Sent to Ollama:\n%s", string(msgJSON))
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	// Use a fresh http.Client with the configured timeout.
	httpClient := &http.Client{Timeout: oc.timeout}
	apiURL := strings.TrimRight(oc.cfg.URL, "/") + "/api/chat"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("connect to ollama: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("ollama returned %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	scanner := bufio.NewScanner(io.LimitReader(resp.Body, ollamaMaxResponse)) // [M6]
	scanner.Buffer(make([]byte, 64*1024), 64*1024)

	var fullResponse strings.Builder

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var chunk ollamaChunk
		if err := json.Unmarshal(line, &chunk); err != nil {
			if debugLog {
				log.Printf("[DEBUG] [Ollama] skipped malformed chunk: %s", line) // [M5]
			}
			continue
		}
		if chunk.Message.Content != "" {
			if debugLog {
				fullResponse.WriteString(chunk.Message.Content)
			}

			// Replace actual newlines with literal '\n' for SSE protocol
			s := strings.ReplaceAll(chunk.Message.Content, "\n", "\\n")
			_, _ = fmt.Fprintf(w, "data: %s\n\n", s)
			flusher.Flush()
		}
		if chunk.Done {
			break
		}
	}

	if debugLog {
		log.Printf("[DEBUG] [Ollama] Output response:\n%s", fullResponse.String())
	}

	return scanner.Err()
}

// buildOllamaSystemPrompt handles context evaluation based on string input
func (s *Server) buildOllamaSystemPrompt(ctx, lang string) string {
	var sb strings.Builder
	// These are caveman style prompts, my english is broken, but not as bad :D
	sb.WriteString("U r linux monitoring expert.\n")
	sb.WriteString("Ur task is analyse metrics.\n")
	sb.WriteString("Be concise and keep it brief.\n")
	sb.WriteString("Use ✅ ok ⚠️ warn 🚨 crit\n")
	sb.WriteString("No look for problems if no any.\n")
	if lang != "" && lang != "en" {
		fmt.Fprintf(&sb, "Respond in the user's language: %s.\n", lang)
	}
	sb.WriteString("\n")

	if strings.HasPrefix(ctx, "chart:") {
		sb.WriteString("The user has requested analysis of a specific chart. Here is the historical data:\n```csv\n")
		sb.WriteString(ctx)
		sb.WriteString("\n```\n")
	} else if ctx == "current" || ctx == "" {
		if s.store != nil {
			if agg, _ := s.store.QueryLatest(); agg != nil && agg.Data != nil {
				sb.WriteString(agg.Data.FormatForAI())
			} else {
				sb.WriteString("No metric data available yet. Ask the user to wait for the first sample.\n")
			}
		} else {
			sb.WriteString("No metric data available yet. Ask the user to wait for the first sample.\n")
		}
	} else {
		// Fallback for custom contexts passed directly by the frontend
		sb.WriteString(ctx)
		sb.WriteString("\n")
	}

	return sb.String()
}
