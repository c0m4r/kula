package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"kula/internal/config"
	"kula/internal/storage"
)

// ollamaModelNameRe matches well-formed Ollama model identifiers like
// "llama3:latest", "library/llama3.1", or "hf.co/user/model:Q4_K_M".
var ollamaModelNameRe = regexp.MustCompile(`^[A-Za-z0-9._:/-]{1,200}$`)

// ollamaChatFailed is all a client learns of a failed chat; the server log
// has the detail.
const ollamaChatFailed = "AI backend request failed; the server log has the details"

// ollamaLangRe matches the language tags the dashboard sends ("en", "pt-BR").
var ollamaLangRe = regexp.MustCompile(`^[A-Za-z]{2,3}(-[A-Za-z0-9]{2,8})?$`)

const (
	// ollamaMaxPrompt is the maximum length, in runes, of the prompt and of each
	// user turn of the conversation history.
	ollamaMaxPrompt = 2000
	// ollamaMaxAnswer is the maximum length, in runes, of each assistant turn
	// of the conversation history.
	ollamaMaxAnswer = 8000
	// ollamaMaxMessages is the number of prior turns a chat request keeps.
	ollamaMaxMessages = 20
	// ollamaMaxContext is the maximum length, in runes, of the session context
	// (a metrics snapshot or chart CSV) placed in the system prompt.
	ollamaMaxContext = 16000
	// ollamaMaxBody is the maximum request body size for /api/ollama/chat.
	ollamaMaxBody = 32 * 1024
	// ollamaDefaultTimeout is the fallback streaming timeout.
	ollamaDefaultTimeout = 120 * time.Second
	// ollamaMaxResponse is the maximum total bytes read from an Ollama stream.
	ollamaMaxResponse = 10 * 1024 * 1024 // 10 MB
	// ollamaChatRateLimit is the max chat requests per IP per minute.
	ollamaChatRateLimit = 10
	// ollamaMaxChats is the max chats streaming at once, server-wide.
	ollamaMaxChats = 4
	// ollamaMetaRateLimit is the max /models + /context requests per IP per minute.
	ollamaMetaRateLimit = 60
	// ollamaMaxToolRounds is the maximum number of tool-call iterations.
	ollamaMaxToolRounds = 5
	// ollamaMaxToolCalls is the maximum number of tool calls run per round:
	// one per resource get_metrics offers. Further calls are dropped.
	ollamaMaxToolCalls = 8
	// ollamaToolMaxItems caps per-category rows in tool result CSV.
	ollamaToolMaxItems = 10
	// ollamaMaxToolless is the number of models remembered as having no tool
	// support; the memory starts over when it is full.
	ollamaMaxToolless = 64
	// ollamaMaxEventField is the maximum length, in runes, of each bound a
	// tool_call event names.
	ollamaMaxEventField = 64
)

// Codes of the AI endpoints' JSON errors, beside the error text: the
// dashboard tells the user what happened in its own language from them.
const (
	aiCodeDisabled           = "disabled"
	aiCodeRateLimited        = "rate_limited"
	aiCodeBusy               = "busy"
	aiCodeBadRequest         = "bad_request"
	aiCodeBackendUnavailable = "backend_unavailable"
)

// aiError answers an AI endpoint's request that fails before any stream
// starts: jsonError's {"error": msg}, with a stable code beside it.
func aiError(w http.ResponseWriter, code, msg string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	b, _ := json.Marshal(map[string]string{"error": msg, "code": code})
	// Writes JSON (not text/html); the XSS rule doesn't apply.
	// nosemgrep: no-direct-write-to-responsewriter
	_, _ = w.Write(b)
}

// errToolsUnsupported marks a round the backend refused because the model
// cannot call tools; streamChat runs it again without them.
var errToolsUnsupported = errors.New("model does not support tools")

// lacksToolSupport reports whether a backend's answer to a round refuses the
// tools it was sent: Ollama answers 400 `{"error":"… does not support
// tools"}`, its OpenAI-compatible API and others 400 with the same words.
func lacksToolSupport(status int, body string) bool {
	return status == http.StatusBadRequest && strings.Contains(strings.ToLower(body), "does not support tools")
}

// roundStatusError is the error of a round the backend answered with status
// other than 200, with the start of its answer for the log. A refusal of the
// tools sent wraps errToolsUnsupported.
func roundStatusError(backend string, resp *http.Response, sentTools bool) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	body := strings.TrimSpace(string(b))
	err := fmt.Errorf("%s returned %d%s: %s", backend, resp.StatusCode, redirectNotFollowed(resp), body)
	if sentTools && lacksToolSupport(resp.StatusCode, body) {
		return fmt.Errorf("%w: %w", errToolsUnsupported, err)
	}
	return err
}

// chatRateLimiter is a per-IP sliding-window rate limiter. The limit is set at
// construction time so a single type can back both the chat endpoint and the
// lighter-weight /models and /context endpoints.
type chatRateLimiter struct {
	mu       sync.Mutex
	limit    int
	requests map[string][]time.Time
}

func newChatRateLimiter() *chatRateLimiter {
	return &chatRateLimiter{limit: ollamaChatRateLimit, requests: make(map[string][]time.Time)}
}

func newMetaRateLimiter() *chatRateLimiter {
	return &chatRateLimiter{limit: ollamaMetaRateLimit, requests: make(map[string][]time.Time)}
}

// Allow returns true if the IP has made fewer than rl.limit requests in the last minute.
func (rl *chatRateLimiter) Allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-time.Minute)

	if !reserveRateLimiterKey(rl.requests, ip, func() { rl.purge(cutoff) }) {
		return false
	}

	var recent []time.Time
	for _, t := range rl.requests[ip] {
		if t.After(cutoff) {
			recent = append(recent, t)
		}
	}
	if len(recent) >= rl.limit {
		return false
	}
	rl.requests[ip] = append(recent, now)
	return true
}

// purge removes entries with no requests after cutoff. Caller must hold rl.mu.
func (rl *chatRateLimiter) purge(cutoff time.Time) {
	for key, reqs := range rl.requests {
		var recent []time.Time
		for _, t := range reqs {
			if t.After(cutoff) {
				recent = append(recent, t)
			}
		}
		if len(recent) == 0 {
			delete(rl.requests, key)
		} else {
			rl.requests[key] = recent
		}
	}
}

// purgeStale drops entries older than the one-minute rate-limit window. It takes
// the lock itself, so the periodic cleanup goroutine can reclaim idle keys without
// waiting for the next Allow call to trip the size cap.
func (rl *chatRateLimiter) purgeStale() {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.purge(time.Now().Add(-time.Minute))
}

// ollamaClient proxies requests to a local Ollama instance or any OpenAI-compatible backend.
// The API flavour is detected automatically on the first fetchModels call and cached.
type ollamaClient struct {
	cfg         config.OllamaConfig
	timeout     time.Duration
	mu          sync.RWMutex
	detectedAPI string          // "ollama" or "openai"; empty until first successful probe
	models      map[string]bool // the backend's last model listing; nil until one succeeds
	modelsErr   string          // why the last listing failed, as logged; "" after a success
	toolless    map[string]bool // models the backend refused tools for, ollamaMaxToolless at most
	chats       chan struct{}   // one token per chat in progress, ollamaMaxChats at most
}

// lacksTools reports whether model is remembered as having no tool support.
func (oc *ollamaClient) lacksTools(model string) bool {
	oc.mu.RLock()
	defer oc.mu.RUnlock()
	return oc.toolless[model]
}

// rememberToolless records that model cannot call tools, so its next chats
// carry none from the start. It reports whether model was new to the memory.
func (oc *ollamaClient) rememberToolless(model string) bool {
	oc.mu.Lock()
	defer oc.mu.Unlock()
	if oc.toolless[model] {
		return false
	}
	if oc.toolless == nil || len(oc.toolless) >= ollamaMaxToolless {
		oc.toolless = make(map[string]bool)
	}
	oc.toolless[model] = true
	return true
}

// startChat takes a chat slot, or reports that all ollamaMaxChats are in use.
// A taken slot is returned with endChat.
func (oc *ollamaClient) startChat() bool {
	select {
	case oc.chats <- struct{}{}:
		return true
	default:
		return false
	}
}

func (oc *ollamaClient) endChat() { <-oc.chats }

// effectiveAPIType returns the detected API flavour, defaulting to "ollama" before detection.
func (oc *ollamaClient) effectiveAPIType() string {
	oc.mu.RLock()
	t := oc.detectedAPI
	oc.mu.RUnlock()
	if t != "" {
		return t
	}
	return "ollama"
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
	log.Printf("[Ollama] client ready: url=%s model=%s timeout=%s (API auto-detect)", cfg.URL, cfg.Model, d)
	return &ollamaClient{cfg: cfg, timeout: d, chats: make(chan struct{}, ollamaMaxChats)}
}

// ---- Ollama API types --------------------------------------------------------

// ollamaToolDef describes a function the model may call.
type ollamaToolDef struct {
	Type     string         `json:"type"`
	Function ollamaToolFunc `json:"function"`
}

type ollamaToolFunc struct {
	Name        string           `json:"name"`
	Description string           `json:"description"`
	Parameters  ollamaToolParams `json:"parameters"`
}

type ollamaToolParams struct {
	Type       string                    `json:"type"`
	Properties map[string]ollamaToolProp `json:"properties"`
	Required   []string                  `json:"required"`
}

type ollamaToolProp struct {
	Type        string   `json:"type"`
	Description string   `json:"description"`
	Enum        []string `json:"enum,omitempty"`
}

// ollamaCallFunc is the function part of a tool call.
type ollamaCallFunc struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ollamaToolCall is a single tool invocation requested by the model.
type ollamaToolCall struct {
	ID       string         `json:"id,omitempty"` // present in OpenAI-compat responses
	Function ollamaCallFunc `json:"function"`
}

// openAIToolCallDelta is a fragment of a tool call in an OpenAI streaming chunk.
type openAIToolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// openAIStreamChunk is one SSE frame from an OpenAI-compatible streaming response.
type openAIStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
			// A reasoning model's thinking: reasoning_content (DeepSeek, vLLM,
			// llama.cpp), or reasoning (Ollama, OpenRouter).
			ReasoningContent string                `json:"reasoning_content"`
			Reasoning        string                `json:"reasoning"`
			ToolCalls        []openAIToolCallDelta `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
}

// ollamaChatRequest is the payload sent to Ollama /api/chat.
type ollamaChatRequest struct {
	Model    string          `json:"model"`
	Messages []ollamaMessage `json:"messages"`
	Tools    []ollamaToolDef `json:"tools,omitempty"`
	Stream   bool            `json:"stream"`
}

type ollamaMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content,omitempty"`
	ToolCalls  []ollamaToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"` // required by OpenAI-compat for tool result messages
}

// ollamaChunk is one streaming response frame from Ollama.
type ollamaChunk struct {
	Message struct {
		Content   string           `json:"content"`
		Thinking  string           `json:"thinking"` // a thinking model's reasoning
		ToolCalls []ollamaToolCall `json:"tool_calls"`
	} `json:"message"`
	Done bool `json:"done"`
}

// chatRequest is the JSON body accepted by /api/ollama/chat.
type chatRequest struct {
	Prompt   string          `json:"prompt"`
	Messages []ollamaMessage `json:"messages"` // prior conversation turns
	Context  string          `json:"context"`  // "current" or "chart:..."
	Lang     string          `json:"lang"`     // BCP 47 language code, e.g. "en", "de"
	Model    string          `json:"model"`    // optional: overrides the configured model
}

// clampText strips null bytes, keeps at most limit runes and trims whitespace.
func clampText(s string, limit int) string {
	s = strings.ReplaceAll(s, "\x00", "")
	if utf8.RuneCountInString(s) > limit {
		runes := []rune(s)
		s = string(runes[:limit])
	}
	return strings.TrimSpace(s)
}

// sanitisePrompt strips null bytes and clamps to ollamaMaxPrompt.
func sanitisePrompt(s string) string {
	return clampText(s, ollamaMaxPrompt)
}

// chatHistory keeps what the model may see of the client's prior turns: the
// text of user and assistant turns, each clamped, the newest ollamaMaxMessages.
// System and tool turns, and tool calls, come only from the server.
func chatHistory(turns []ollamaMessage) []ollamaMessage {
	kept := make([]ollamaMessage, 0, min(len(turns), ollamaMaxMessages))
	for _, m := range turns {
		var content string
		switch m.Role {
		case "user":
			content = sanitisePrompt(m.Content)
		case "assistant":
			content = clampText(m.Content, ollamaMaxAnswer)
		default:
			continue
		}
		if content != "" {
			kept = append(kept, ollamaMessage{Role: m.Role, Content: content})
		}
	}
	if len(kept) > ollamaMaxMessages {
		kept = kept[len(kept)-ollamaMaxMessages:]
	}
	return kept
}

// chatModel is the model a chat runs on: the requested one when the backend
// lists it, the configured one otherwise. Before any listing has succeeded
// (Kula restarted under an open dashboard) it asks the backend for one.
func (oc *ollamaClient) chatModel(ctx context.Context, requested string, debugLog bool) string {
	m := strings.TrimSpace(requested)
	if m == "" || m == oc.cfg.Model {
		return oc.cfg.Model
	}
	if !ollamaModelNameRe.MatchString(m) {
		if debugLog {
			log.Printf("[DEBUG] [Ollama] rejected invalid model name %q, using configured model %s", m, oc.cfg.Model)
		}
		return oc.cfg.Model
	}
	oc.mu.RLock()
	listed, fetched := oc.models[m], oc.models != nil
	oc.mu.RUnlock()
	if !fetched {
		if names, err := oc.fetchModels(ctx, debugLog); err == nil {
			listed = slices.Contains(names, m)
		}
	}
	if !listed {
		if debugLog {
			log.Printf("[DEBUG] [Ollama] model %q is not offered by the backend, using configured model %s", m, oc.cfg.Model)
		}
		return oc.cfg.Model
	}
	return m
}

// handleOllamaChat is the HTTP handler for POST /api/ollama/chat.
// It accepts a JSON body, builds a conversation, and streams the Ollama
// response back as Server-Sent Events (text/event-stream).
func (s *Server) handleOllamaChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}

	if s.ollama == nil || !s.ollama.cfg.Enabled {
		aiError(w, aiCodeDisabled, "ollama is not enabled", http.StatusServiceUnavailable)
		return
	}

	ip := getClientIP(r, s.cfg.TrustProxy)
	if s.ollamaLimiter != nil && !s.ollamaLimiter.Allow(ip) {
		aiError(w, aiCodeRateLimited, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, ollamaMaxBody)
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		aiError(w, aiCodeBadRequest, "invalid request body", http.StatusBadRequest)
		return
	}

	// A chat holds the backend for up to ollamaMaxToolRounds timeouts; the
	// per-address limit alone would let a few addresses keep it busy. The
	// slot is taken once the request is read: a client sending its body
	// slowly holds none.
	if !s.ollama.startChat() {
		aiError(w, aiCodeBusy, "too many AI chats in progress", http.StatusTooManyRequests)
		return
	}
	defer s.ollama.endChat()

	userPrompt := sanitisePrompt(req.Prompt)
	if userPrompt == "" {
		userPrompt = "Analyse the current server metrics and summarise the system health."
	}
	debugLog := s.cfg.Logging.Enabled && s.cfg.Logging.Level == "debug"

	// The client may pick another model (the dashboard's model selector), but
	// only one the backend lists: anything else runs on the configured model.
	model := s.ollama.chatModel(r.Context(), req.Model, debugLog)

	// Wire up tools when storage is available, unless the backend refused
	// them for this model before.
	toolless := s.ollama.lacksTools(model)
	var tools []ollamaToolDef
	var toolExecutor func(string, json.RawMessage) string
	if s.store != nil && !toolless {
		tools = []ollamaToolDef{metricsToolDef()}
		toolExecutor = func(name string, args json.RawMessage) string {
			switch name {
			case "get_metrics":
				return s.executeGetMetrics(args)
			default:
				return fmt.Sprintf("unknown tool: %s", name)
			}
		}
	}

	// Build message list: system prompt + prior turns + current user turn.
	// A round that finds the model cannot call tools is run again with the
	// system prompt for a chat without them.
	systemContent := s.buildOllamaSystemPrompt(req.Context, req.Lang, len(tools) > 0)
	withoutTools := func() string { return s.buildOllamaSystemPrompt(req.Context, req.Lang, false) }
	messages := []ollamaMessage{
		{Role: "system", Content: systemContent},
	}
	history := chatHistory(req.Messages)
	messages = append(messages, history...)

	// Only append the current prompt if it's not already at the end of the history.
	// This prevents duplication when the frontend sends the updated history.
	last := messages[len(messages)-1]
	if last.Role != "user" || last.Content != userPrompt {
		messages = append(messages, ollamaMessage{Role: "user", Content: userPrompt})
	}

	if debugLog {
		log.Printf("[DEBUG] [Ollama] chat: ip=%s model=%s api=%s tools=%t toolless_remembered=%t prompt_len=%d history=%d",
			ip, model, s.ollama.effectiveAPIType(), len(tools) > 0, toolless, utf8.RuneCountInString(userPrompt), len(history))
	}

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

	// Flush headers immediately so the browser opens the stream before Ollama
	// responds. Without this, tool-call rounds that produce no early text
	// content would leave the browser with no SSE connection established.
	// The first event names the model the chat runs on, which may not be the
	// one asked for; a model known to lack tools is said so at once.
	writeSSE(w, "model", model)
	if toolless && s.store != nil {
		writeSSE(w, "notools", "")
	}
	flusher.Flush()

	err := s.ollama.streamChat(r.Context(), model, messages, tools, toolExecutor, withoutTools, w, flusher, debugLog)
	if err != nil {
		// The detail (the backend's address, its answer) is for the operator;
		// the client, who may be anyone with auth off, learns only that it failed.
		log.Printf("[Ollama] stream error: %q", err.Error())
		writeSSE(w, "error", ollamaChatFailed)
		flusher.Flush()
	}

	// Signal end of stream.
	writeSSE(w, "done", "")
	flusher.Flush()
}

// writeSSE writes one text/event-stream event (not text/html) whose data is
// payload as a JSON string. Model output and backend errors may hold any
// character, and a raw CR or LF would end the data line and let the rest
// pass for fields of its own; JSON escapes both.
func writeSSE(w io.Writer, event, payload string) {
	var frame bytes.Buffer
	if event != "" {
		frame.WriteString("event: " + event + "\n")
	}
	data, _ := json.Marshal(payload) // a string always encodes
	frame.WriteString("data: ")
	frame.Write(data)
	frame.WriteString("\n\n")
	_, _ = w.Write(frame.Bytes())
}

// streamChat runs the agentic tool-calling loop: it calls doStreamRound up to
// ollamaMaxToolRounds times, executing any requested tools between rounds.
//
// A round the backend refuses because the model cannot call tools is run
// once more without them, the model is remembered as tool-less, and the
// client is sent a notools event. withoutTools, when not nil, gives the
// system prompt (messages[0]) for that run.
func (oc *ollamaClient) streamChat(
	ctx context.Context,
	model string,
	messages []ollamaMessage,
	tools []ollamaToolDef,
	toolExecutor func(string, json.RawMessage) string,
	withoutTools func() string,
	w io.Writer,
	flusher http.Flusher,
	debugLog bool,
) error {
	msgs := messages
	for round := 0; round < ollamaMaxToolRounds; round++ {
		toolCalls, err := oc.doStreamRound(ctx, model, msgs, tools, w, flusher, debugLog)
		if errors.Is(err, errToolsUnsupported) {
			if oc.rememberToolless(model) {
				log.Printf("[Ollama] model %q does not support tools; its chats run without get_metrics", model)
			}
			if debugLog {
				log.Printf("[DEBUG] [Ollama] %q; retrying round %d without tools", err.Error(), round+1)
			}
			writeSSE(w, "notools", "")
			flusher.Flush()
			tools = nil
			if withoutTools != nil && len(msgs) > 0 && msgs[0].Role == "system" {
				msgs = slices.Clone(msgs)
				msgs[0].Content = withoutTools()
			}
			toolCalls, err = oc.doStreamRound(ctx, model, msgs, nil, w, flusher, debugLog)
		}
		if err != nil {
			return err
		}
		if len(toolCalls) == 0 || toolExecutor == nil || len(tools) == 0 {
			break
		}

		// Append the assistant's tool-call turn (no text content).
		msgs = append(msgs, ollamaMessage{Role: "assistant", ToolCalls: toolCalls})

		// Execute each requested tool and append the results, telling the
		// client before each what it fetches.
		for _, tc := range toolCalls {
			writeSSE(w, "tool_call", toolCallEvent(tc))
			flusher.Flush()
			result := toolExecutor(tc.Function.Name, tc.Function.Arguments)
			if debugLog {
				log.Printf("[DEBUG] [Ollama] tool %q result (%d bytes)", tc.Function.Name, len(result))
			}
			msg := ollamaMessage{Role: "tool", Content: result}
			if tc.ID != "" {
				msg.ToolCallID = tc.ID
			}
			msgs = append(msgs, msg)
		}
	}
	return nil
}

// toolCallEvent is the data of a tool_call event, a JSON object of the
// resource and bounds a call asks for, which the dashboard shows. The
// resource is named only when get_metrics offers it; the bounds are the
// model's text, without control or format characters and clamped. Arguments
// that are no such object give empty fields.
func toolCallEvent(tc ollamaToolCall) string {
	var args getMetricsArgs
	if json.Unmarshal(tc.Function.Arguments, &args) != nil {
		args = getMetricsArgs{}
	}
	if !slices.Contains(metricsToolResources, args.Resource) {
		args.Resource = ""
	}
	args.From = eventField(args.From)
	args.To = eventField(args.To)
	b, _ := json.Marshal(args) // strings always encode
	return string(b)
}

// eventField drops control and format characters (bidi overrides, zero
// widths) from s and keeps at most ollamaMaxEventField runes.
func eventField(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
	if utf8.RuneCountInString(s) > ollamaMaxEventField {
		s = string([]rune(s)[:ollamaMaxEventField])
	}
	return s
}

// doStreamRound performs one HTTP round-trip to the configured AI backend, streaming
// text chunks as SSE data frames. It returns any tool calls present in the response.
// Routes to the OpenAI-compatible implementation when the backend is detected as such.
func (oc *ollamaClient) doStreamRound(
	ctx context.Context,
	model string,
	messages []ollamaMessage,
	tools []ollamaToolDef,
	w io.Writer,
	flusher http.Flusher,
	debugLog bool,
) ([]ollamaToolCall, error) {
	if oc.effectiveAPIType() == "openai" {
		return oc.doStreamRoundOpenAI(ctx, model, messages, tools, w, flusher, debugLog)
	}

	reqBody := ollamaChatRequest{
		Model:    model,
		Messages: messages,
		Tools:    tools,
		Stream:   true,
	}
	if debugLog {
		msgJSON, _ := json.MarshalIndent(reqBody, "", "  ")
		log.Printf("[DEBUG] [Ollama] full request:\n%s", string(msgJSON))
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpClient := newOllamaHTTPClient(oc.timeout)
	apiURL := strings.TrimRight(oc.cfg.URL, "/") + "/api/chat"
	if debugLog {
		log.Printf("[DEBUG] [Ollama] POST %s (model=%s)", apiURL, model)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connect to ollama: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if debugLog {
		log.Printf("[DEBUG] [Ollama] HTTP %d from %s", resp.StatusCode, apiURL)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, roundStatusError("ollama", resp, len(tools) > 0)
	}

	scanner := bufio.NewScanner(io.LimitReader(resp.Body, ollamaMaxResponse)) // [M6]
	scanner.Buffer(make([]byte, 64*1024), 64*1024)

	var fullResponse, fullThinking strings.Builder
	var toolCalls []ollamaToolCall

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
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
		// Collect tool calls from any chunk (not just the done chunk), up to
		// the per-round cap.
		calls := chunk.Message.ToolCalls
		if room := ollamaMaxToolCalls - len(toolCalls); len(calls) > room {
			if debugLog {
				log.Printf("[DEBUG] [Ollama] dropped %d tool call(s) over the cap of %d per round", len(calls)-room, ollamaMaxToolCalls)
			}
			calls = calls[:room]
		}
		toolCalls = append(toolCalls, calls...)
		if chunk.Message.Thinking != "" {
			if debugLog {
				fullThinking.WriteString(chunk.Message.Thinking)
			}
			writeSSE(w, "thinking", chunk.Message.Thinking)
			flusher.Flush()
		}
		if chunk.Message.Content != "" {
			if debugLog {
				fullResponse.WriteString(chunk.Message.Content)
			}
			writeSSE(w, "", chunk.Message.Content)
			flusher.Flush()
		}
		if chunk.Done {
			break
		}
	}

	if debugLog && fullThinking.Len() > 0 {
		log.Printf("[DEBUG] [Ollama] Thinking:\n%s", fullThinking.String())
	}
	if debugLog && fullResponse.Len() > 0 {
		log.Printf("[DEBUG] [Ollama] Output response:\n%s", fullResponse.String())
	}

	return toolCalls, scanner.Err()
}

// doStreamRoundOpenAI performs one HTTP round-trip to an OpenAI-compatible /v1/chat/completions
// endpoint, streaming text chunks as SSE data frames. Tool call arguments are accumulated
// across multiple delta chunks before being returned.
func (oc *ollamaClient) doStreamRoundOpenAI(
	ctx context.Context,
	model string,
	messages []ollamaMessage,
	tools []ollamaToolDef,
	w io.Writer,
	flusher http.Flusher,
	debugLog bool,
) ([]ollamaToolCall, error) {
	reqBody := ollamaChatRequest{
		Model:    model,
		Messages: messages,
		Tools:    tools,
		Stream:   true,
	}
	if debugLog {
		msgJSON, _ := json.MarshalIndent(reqBody, "", "  ")
		log.Printf("[DEBUG] [Ollama/OpenAI] full request:\n%s", string(msgJSON))
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpClient := newOllamaHTTPClient(oc.timeout)
	apiURL := strings.TrimRight(oc.cfg.URL, "/") + "/v1/chat/completions"
	if debugLog {
		log.Printf("[DEBUG] [Ollama/OpenAI] POST %s (model=%s)", apiURL, model)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connect to openai-compat: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if debugLog {
		log.Printf("[DEBUG] [Ollama/OpenAI] HTTP %d from %s", resp.StatusCode, apiURL)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, roundStatusError("api", resp, len(tools) > 0)
	}

	scanner := bufio.NewScanner(io.LimitReader(resp.Body, ollamaMaxResponse))
	scanner.Buffer(make([]byte, 64*1024), 64*1024)

	// Accumulate streaming tool call fragments keyed by index.
	type toolCallAccum struct {
		id   string
		name string
		args strings.Builder
	}
	accumMap := make(map[int]*toolCallAccum)
	dropped := 0 // fragments of calls over the per-round cap

	var fullResponse, fullThinking strings.Builder
	var finishReason string

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			break
		}
		var chunk openAIStreamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			if debugLog {
				log.Printf("[DEBUG] [Ollama/OpenAI] skipped malformed chunk: %s", payload)
			}
			continue
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		choice := chunk.Choices[0]
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			finishReason = *choice.FinishReason
		}
		for _, tc := range choice.Delta.ToolCalls {
			// The index names the call a fragment belongs to; a negative one
			// names none. Calls past the per-round cap are dropped.
			if tc.Index < 0 {
				continue
			}
			a, ok := accumMap[tc.Index]
			if !ok {
				if len(accumMap) >= ollamaMaxToolCalls {
					dropped++
					continue
				}
				a = &toolCallAccum{}
				accumMap[tc.Index] = a
			}
			if tc.ID != "" {
				a.id = tc.ID
			}
			if tc.Function.Name != "" {
				a.name = tc.Function.Name
			}
			a.args.WriteString(tc.Function.Arguments)
		}
		thinking := choice.Delta.ReasoningContent
		if thinking == "" {
			thinking = choice.Delta.Reasoning
		}
		if thinking != "" {
			if debugLog {
				fullThinking.WriteString(thinking)
			}
			writeSSE(w, "thinking", thinking)
			flusher.Flush()
		}
		if choice.Delta.Content != "" {
			if debugLog {
				fullResponse.WriteString(choice.Delta.Content)
			}
			writeSSE(w, "", choice.Delta.Content)
			flusher.Flush()
		}
	}

	if debugLog && fullThinking.Len() > 0 {
		log.Printf("[DEBUG] [Ollama/OpenAI] thinking:\n%s", fullThinking.String())
	}
	if debugLog && fullResponse.Len() > 0 {
		log.Printf("[DEBUG] [Ollama/OpenAI] output response:\n%s", fullResponse.String())
	}
	if debugLog && dropped > 0 {
		log.Printf("[DEBUG] [Ollama/OpenAI] dropped %d tool-call fragment(s) over the cap of %d calls per round", dropped, ollamaMaxToolCalls)
	}

	if finishReason != "tool_calls" || len(accumMap) == 0 {
		return nil, scanner.Err()
	}

	// Calls in index order. The indices need not be dense: the backend's
	// numbering only groups fragments.
	indices := make([]int, 0, len(accumMap))
	for i := range accumMap {
		indices = append(indices, i)
	}
	sort.Ints(indices)
	toolCalls := make([]ollamaToolCall, 0, len(indices))
	for _, i := range indices {
		a := accumMap[i]
		toolCalls = append(toolCalls, ollamaToolCall{
			ID: a.id,
			Function: ollamaCallFunc{
				Name:      a.name,
				Arguments: json.RawMessage(a.args.String()),
			},
		})
	}
	if debugLog {
		log.Printf("[DEBUG] [Ollama/OpenAI] %d tool call(s) requested", len(toolCalls))
	}
	return toolCalls, scanner.Err()
}

// handleOllamaModels returns the list of models available in the Ollama instance.
func (s *Server) handleOllamaModels(w http.ResponseWriter, r *http.Request) {
	if s.ollama == nil || !s.ollama.cfg.Enabled {
		aiError(w, aiCodeDisabled, "ollama is not enabled", http.StatusServiceUnavailable)
		return
	}
	ip := getClientIP(r, s.cfg.TrustProxy)
	if s.ollamaMetaLim != nil && !s.ollamaMetaLim.Allow(ip) {
		aiError(w, aiCodeRateLimited, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}
	debugLog := s.cfg.Logging.Enabled && s.cfg.Logging.Level == "debug"
	models, err := s.ollama.fetchModels(r.Context(), debugLog)
	if err != nil {
		s.ollama.logModelsError(err)
		aiError(w, aiCodeBackendUnavailable, "AI backend unavailable", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"models": models})
}

// fetchModels returns the list of available model names, auto-detecting the backend API
// on the first call. It tries the native Ollama /api/tags endpoint first; if that fails
// it falls back to the OpenAI-compatible /v1/models endpoint and caches the result.
func (oc *ollamaClient) fetchModels(ctx context.Context, debugLog bool) ([]string, error) {
	// Use cached detection if available.
	oc.mu.RLock()
	detected := oc.detectedAPI
	oc.mu.RUnlock()

	if detected == "openai" {
		names, err := oc.fetchModelsOpenAI(ctx, debugLog)
		if err == nil {
			oc.mu.Lock()
			oc.setModelsLocked(names)
			oc.mu.Unlock()
		}
		return names, err
	}

	// Try native Ollama API.
	names, err := oc.fetchModelsOllama(ctx, debugLog)
	if err == nil {
		oc.mu.Lock()
		oc.detectedAPI = "ollama"
		oc.setModelsLocked(names)
		oc.mu.Unlock()
		return names, nil
	}

	// Native API failed — try OpenAI-compatible fallback.
	if debugLog {
		log.Printf("[DEBUG] [Ollama] native API probe failed (%v), trying OpenAI-compatible", err)
	} else {
		log.Printf("[Ollama] native API not available, trying OpenAI-compatible endpoint")
	}
	names, err = oc.fetchModelsOpenAI(ctx, debugLog)
	if err == nil {
		oc.mu.Lock()
		oc.detectedAPI = "openai"
		oc.setModelsLocked(names)
		oc.mu.Unlock()
		log.Printf("[Ollama] detected OpenAI-compatible API at %s", oc.cfg.URL)
	}
	return names, err
}

// setModelsLocked records the backend's model listing, the models a chat may
// ask for, and forgets that a model lacks tools once the listing no longer
// has it (it may come back as another model of that name). The configured
// model is kept: a chat may run on it under a name the listing spells
// otherwise ("llama3" for "llama3:latest"). Caller must hold oc.mu for writing.
func (oc *ollamaClient) setModelsLocked(names []string) {
	oc.models = make(map[string]bool, len(names))
	for _, name := range names {
		oc.models[name] = true
	}
	oc.modelsErr = ""
	for name := range oc.toolless {
		if !oc.models[name] && name != oc.cfg.Model {
			delete(oc.toolless, name)
		}
	}
}

// logModelsError logs why the model list is unavailable, escaped, once until
// the reason changes or a listing succeeds: an open AI panel retries every
// few seconds.
func (oc *ollamaClient) logModelsError(err error) {
	msg := err.Error()
	oc.mu.Lock()
	repeated := msg == oc.modelsErr
	oc.modelsErr = msg
	oc.mu.Unlock()
	if !repeated {
		log.Printf("[Ollama] model list unavailable: %q", msg)
	}
}

// newOllamaHTTPClient returns a client that does not follow redirects and
// connects only to loopback addresses. The configured URL is validated to be
// loopback; a redirect could lead the proxy anywhere else, so it is reported
// instead (see redirectNotFollowed).
func newOllamaHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: ollamaTransport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// ollamaTransport carries every request to the AI backend, through no proxy.
// ollama.url is checked to name a loopback host at config load, but a name
// ("localhost") is resolved when connecting: its dialer refuses any address
// that is not loopback, so a resolver's answer cannot send the chat, with
// the metrics in it, to another host.
var ollamaTransport = func() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second, Control: dialLoopbackOnly}
	t.DialContext = dialer.DialContext
	return t
}()

// dialLoopbackOnly is a net.Dialer Control: it runs for each address a name
// resolved to, before connecting, and refuses one that is not loopback.
func dialLoopbackOnly(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	if ip, err := netip.ParseAddr(host); err != nil || !ip.Unmap().IsLoopback() {
		return fmt.Errorf("refused %s: the AI backend must be on a loopback address", address)
	}
	return nil
}

// redirectNotFollowed names the target of a refused redirect for an error
// message, so the operator can point ollama.url at it; "" for other statuses.
func redirectNotFollowed(resp *http.Response) string {
	if resp.StatusCode < 300 || resp.StatusCode > 399 {
		return ""
	}
	return fmt.Sprintf(" (redirect to %q not followed; set ollama.url to the final address)", resp.Header.Get("Location"))
}

// fetchModelsOllama queries the native Ollama /api/tags endpoint.
func (oc *ollamaClient) fetchModelsOllama(ctx context.Context, debugLog bool) ([]string, error) {
	httpClient := newOllamaHTTPClient(5 * time.Second)
	apiURL := strings.TrimRight(oc.cfg.URL, "/") + "/api/tags"
	if debugLog {
		log.Printf("[DEBUG] [Ollama] fetchModels: GET %s", apiURL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connect to ollama: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if debugLog {
		log.Printf("[DEBUG] [Ollama] fetchModels: HTTP %d", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama returned %d%s", resp.StatusCode, redirectNotFollowed(resp))
	}
	var result struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&result); err != nil {
		return nil, fmt.Errorf("parse models: %w", err)
	}
	names := make([]string, 0, len(result.Models))
	for _, m := range result.Models {
		if m.Name != "" {
			names = append(names, m.Name)
		}
	}
	if debugLog {
		log.Printf("[DEBUG] [Ollama] fetchModels: found %d model(s)", len(names))
	}
	return names, nil
}

// fetchModelsOpenAI queries an OpenAI-compatible /v1/models endpoint.
func (oc *ollamaClient) fetchModelsOpenAI(ctx context.Context, debugLog bool) ([]string, error) {
	httpClient := newOllamaHTTPClient(5 * time.Second)
	apiURL := strings.TrimRight(oc.cfg.URL, "/") + "/v1/models"
	if debugLog {
		log.Printf("[DEBUG] [Ollama/OpenAI] fetchModels: GET %s", apiURL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connect to openai-compat: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if debugLog {
		log.Printf("[DEBUG] [Ollama/OpenAI] fetchModels: HTTP %d", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("api returned %d%s", resp.StatusCode, redirectNotFollowed(resp))
	}
	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&result); err != nil {
		return nil, fmt.Errorf("parse models: %w", err)
	}
	names := make([]string, 0, len(result.Data))
	for _, m := range result.Data {
		if m.ID != "" {
			names = append(names, m.ID)
		}
	}
	if debugLog {
		log.Printf("[DEBUG] [Ollama/OpenAI] fetchModels: found %d model(s)", len(names))
	}
	return names, nil
}

// buildOllamaSystemPrompt builds the system prompt for an Ollama request.
// ctx is either "current" (live QueryLatest), "chart:…" (CSV data), or a
// pre-formatted metrics snapshot string cached by the frontend. Both come
// from the client: ctx is clamped to ollamaMaxContext runes, and a lang that
// is not a language tag is ignored. withTools tells the model whether it has
// the get_metrics tool, and what that does not cover.
func (s *Server) buildOllamaSystemPrompt(ctx, lang string, withTools bool) string {
	ctx = clampText(ctx, ollamaMaxContext)
	if !ollamaLangRe.MatchString(lang) {
		lang = ""
	}
	var sb strings.Builder
	// These are caveman style prompts, my english is broken, but not as bad :D
	sb.WriteString("U r linux monitoring expert.\n")
	sb.WriteString("Ur task is analyse metrics.\n")
	sb.WriteString("Be concise and keep it brief.\n")
	sb.WriteString("Use ✅ ok ⚠️ warn 🚨 crit\n")
	sb.WriteString("No look for problems if no any.\n")
	sb.WriteString("Answer in markdown: **bold**, *italic*, `code`, ``` code blocks, lists, tables, # headings. No links, no images, no HTML.\n")
	if withTools {
		fmt.Fprintf(&sb, "Tool get_metrics give history only for %s. No history of apps, containers, sensors.\n",
			strings.Join(metricsToolResources, ", "))
	} else {
		sb.WriteString("No history tool. Answer only from snapshot or chart data given.\n")
	}
	if lang != "" && lang != "en" {
		fmt.Fprintf(&sb, "Respond in the user's language: %s.\n", lang)
	}
	sb.WriteString("\n")

	if strings.HasPrefix(ctx, "chart:") {
		// Always include chart CSV for chart sessions — the model needs the raw
		// values on every turn since they aren't stored in the message history.
		sb.WriteString("The user has requested analysis of a specific chart. Here is the historical data:\n```csv\n")
		sb.WriteString(ctx)
		sb.WriteString("\n```\n")
	} else if ctx == "current" || ctx == "" {
		// Fallback when the frontend hasn't cached the snapshot yet.
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
		// Pre-formatted metrics snapshot cached by the frontend and re-sent on
		// every turn, so the model sees the same data across the whole session.
		sb.WriteString(ctx)
		sb.WriteString("\n")
	}

	// The time comes last, and to the minute: a backend reuses its work on
	// the part of a prompt that did not change since the previous turn, and
	// the time is the only line here that changes within a session.
	fmt.Fprintf(&sb, "\nServer time now: %s.\n", time.Now().Format(ollamaPromptTimeLayout))
	return sb.String()
}

// ollamaPromptTimeLayout is collector.AITimeLayout to the minute.
const ollamaPromptTimeLayout = "2006-01-02 15:04 MST (UTC-07:00)"

// handleOllamaContext returns the current metrics formatted for use as a
// session context snapshot. The frontend calls this once when a session starts
// and re-sends the result as the "context" field on every subsequent turn,
// so the model always sees the same snapshot rather than stale-vs-new values.
// ts is the time of the sample the snapshot was made from, so the dashboard
// can show its age; it is left out when there is no sample.
func (s *Server) handleOllamaContext(w http.ResponseWriter, r *http.Request) {
	if s.ollama == nil || !s.ollama.cfg.Enabled {
		aiError(w, aiCodeDisabled, "ollama is not enabled", http.StatusServiceUnavailable)
		return
	}
	ip := getClientIP(r, s.cfg.TrustProxy)
	if s.ollamaMetaLim != nil && !s.ollamaMetaLim.Allow(ip) {
		aiError(w, aiCodeRateLimited, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}
	var out struct {
		Context string `json:"context"`
		TS      string `json:"ts,omitempty"`
	}
	if s.store != nil {
		if agg, _ := s.store.QueryLatest(); agg != nil && agg.Data != nil {
			out.Context = agg.Data.FormatForAI()
			if !agg.Timestamp.IsZero() {
				out.TS = agg.Timestamp.Local().Format(time.RFC3339)
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// ---- Tool definitions --------------------------------------------------------

// metricsToolResources are the resources get_metrics offers.
var metricsToolResources = []string{"cpu", "memory", "swap", "load", "network", "disk_io", "disk_space", "gpu"}

// metricsToolDef returns the get_metrics tool schema.
func metricsToolDef() ollamaToolDef {
	return ollamaToolDef{
		Type: "function",
		Function: ollamaToolFunc{
			Name:        "get_metrics",
			Description: "Fetch historical time-series metrics for a given resource. Use this when the user asks about trends, history, or a specific past time range.",
			Parameters: ollamaToolParams{
				Type: "object",
				Properties: map[string]ollamaToolProp{
					"resource": {
						Type:        "string",
						Description: "The resource type to fetch.",
						Enum:        slices.Clone(metricsToolResources),
					},
					"from": {
						Type:        "string",
						Description: `Start of the time range. Use a relative duration like "-1h" or "-30m", or an RFC3339 timestamp. Defaults to one hour before "to" when omitted. The range may span at most 31 days.`,
					},
					"to": {
						Type:        "string",
						Description: `End of the time range. Use a relative duration or RFC3339 timestamp. Defaults to now when omitted.`,
					},
				},
				Required: []string{"resource"},
			},
		},
	}
}

// getMetricsArgs is the decoded argument struct for the get_metrics tool call.
type getMetricsArgs struct {
	Resource string `json:"resource"`
	From     string `json:"from"`
	To       string `json:"to"`
}

// executeGetMetrics implements the get_metrics tool: queries storage and returns
// a CSV time series for the requested resource.
func (s *Server) executeGetMetrics(argsJSON json.RawMessage) string {
	var args getMetricsArgs
	if err := json.Unmarshal(argsJSON, &args); err != nil {
		return fmt.Sprintf("error: invalid arguments: %v", err)
	}
	if !slices.Contains(metricsToolResources, args.Resource) {
		return fmt.Sprintf("unknown resource type: %s\n", args.Resource)
	}

	now := time.Now()
	to := now

	parseTime := func(raw string) (time.Time, bool) {
		if d, err := time.ParseDuration(raw); err == nil {
			return now.Add(d), true
		}
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			return t, true
		}
		return time.Time{}, false
	}

	// An unreadable bound is an error the model can correct, not a silent
	// fallback to the default hour presented as the range it asked for.
	if args.To != "" {
		t, ok := parseTime(args.To)
		if !ok {
			return fmt.Sprintf("error: invalid 'to' time %q", args.To)
		}
		to = t
	}
	from := to.Add(-time.Hour)
	if args.From != "" {
		t, ok := parseTime(args.From)
		if !ok {
			return fmt.Sprintf("error: invalid 'from' time %q", args.From)
		}
		from = t
	}
	if err := validateHistoryRange(from, to); err != nil {
		return "error: " + err.Error()
	}
	if s.store == nil {
		return "error: no storage available"
	}

	result, err := s.store.QueryRangeWithMeta(from, to, 100)
	if err != nil {
		return fmt.Sprintf("error: query failed: %v", err)
	}
	if len(result.Samples) == 0 {
		return "no data available for the requested time range"
	}

	return formatMetricsData(args.Resource, from, to, result.Resolution, result.Samples)
}

// formatMetricsData converts storage samples for a given resource into a CSV
// string suitable for LLM consumption. A comment line names the resource,
// the range [from, to] and the step between samples first: step is the
// query's, or the samples' median gap when it is "". Timestamps are local,
// with as much of the date as the range needs to be read unambiguously.
func formatMetricsData(resource string, from, to time.Time, step string, samples []*storage.AggregatedSample) string {
	if !slices.Contains(metricsToolResources, resource) {
		return fmt.Sprintf("unknown resource type: %s\n", resource)
	}
	now := time.Now()
	from, to = from.In(now.Location()), to.In(now.Location())
	layout := metricsTimeLayout(from, to, now)
	stamp := func(s *storage.AggregatedSample) string { return s.Timestamp.In(now.Location()).Format(layout) }
	if step == "" {
		step = medianGap(samples)
	}

	var sb strings.Builder
	sb.WriteString(metricsHeader(resource, from, to, now, layout, step))
	switch resource {
	case "cpu":
		sb.WriteString("Time,CPU%\n")
		for _, s := range samples {
			if s.Data == nil {
				continue
			}
			fmt.Fprintf(&sb, "%s,%.1f\n", stamp(s), s.Data.CPU.Total.Usage)
		}
	case "memory":
		sb.WriteString("Time,Used%,Used_GiB,Total_GiB\n")
		for _, s := range samples {
			if s.Data == nil {
				continue
			}
			fmt.Fprintf(&sb, "%s,%.1f,%.2f,%.2f\n",
				stamp(s),
				s.Data.Memory.UsedPercent,
				float64(s.Data.Memory.Used)/(1<<30),
				float64(s.Data.Memory.Total)/(1<<30))
		}
	case "swap":
		sb.WriteString("Time,Swap_Used%\n")
		for _, s := range samples {
			if s.Data == nil {
				continue
			}
			fmt.Fprintf(&sb, "%s,%.1f\n", stamp(s), s.Data.Swap.UsedPercent)
		}
	case "load":
		sb.WriteString("Time,Load1,Load5,Load15\n")
		for _, s := range samples {
			if s.Data == nil {
				continue
			}
			fmt.Fprintf(&sb, "%s,%.2f,%.2f,%.2f\n",
				stamp(s),
				s.Data.LoadAvg.Load1, s.Data.LoadAvg.Load5, s.Data.LoadAvg.Load15)
		}
	case "network":
		sb.WriteString("Time,Interface,RxMbps,TxMbps\n")
		for _, s := range samples {
			if s.Data == nil {
				continue
			}
			shown := len(s.Data.Network.Interfaces)
			if shown > ollamaToolMaxItems {
				shown = ollamaToolMaxItems
			}
			for _, iface := range s.Data.Network.Interfaces[:shown] {
				fmt.Fprintf(&sb, "%s,%s,%.2f,%.2f\n",
					stamp(s),
					iface.Name, iface.RxMbps, iface.TxMbps)
			}
		}
	case "disk_io":
		sb.WriteString("Time,Device,Read_MBps,Write_MBps,Util%\n")
		for _, s := range samples {
			if s.Data == nil {
				continue
			}
			shown := len(s.Data.Disks.Devices)
			if shown > ollamaToolMaxItems {
				shown = ollamaToolMaxItems
			}
			for _, d := range s.Data.Disks.Devices[:shown] {
				fmt.Fprintf(&sb, "%s,%s,%.2f,%.2f,%.1f\n",
					stamp(s),
					d.Name, d.ReadBytesPS/1e6, d.WriteBytesPS/1e6, d.Utilization)
			}
		}
	case "disk_space":
		sb.WriteString("Time,MountPoint,Used%,Used_GiB,Total_GiB\n")
		for _, s := range samples {
			if s.Data == nil {
				continue
			}
			shown := len(s.Data.Disks.FileSystems)
			if shown > ollamaToolMaxItems {
				shown = ollamaToolMaxItems
			}
			for _, fs := range s.Data.Disks.FileSystems[:shown] {
				fmt.Fprintf(&sb, "%s,%s,%.1f,%.2f,%.2f\n",
					stamp(s),
					fs.MountPoint, fs.UsedPct,
					float64(fs.Used)/(1<<30),
					float64(fs.Total)/(1<<30))
			}
		}
	case "gpu":
		sb.WriteString("Time,GPU,Load%,VRAM%\n")
		for _, s := range samples {
			if s.Data == nil {
				continue
			}
			shown := len(s.Data.GPU)
			if shown > ollamaToolMaxItems {
				shown = ollamaToolMaxItems
			}
			for _, g := range s.Data.GPU[:shown] {
				fmt.Fprintf(&sb, "%s,%s,%.1f,%.1f\n",
					stamp(s),
					g.Name, g.LoadPct, g.VRAMUsedPct)
			}
		}
	default:
		return fmt.Sprintf("unknown resource type: %s\n", resource)
	}
	return sb.String()
}

// Timestamp layouts of a get_metrics answer, from the most to the least
// precise: a range is at most 31 days, so the date matters more than seconds.
const (
	metricsLayoutTime = "15:04:05"
	metricsLayoutDate = "01-02 15:04:05"
	metricsLayoutYear = "2006-01-02 15:04"
)

// metricsTimeLayout is the timestamp layout for [from, to]: the time of day
// when both fall on one day, the date too when they fall in the year of now,
// and the year too otherwise. All three are in the same location.
func metricsTimeLayout(from, to, now time.Time) string {
	switch {
	case sameDay(from, to):
		return metricsLayoutTime
	case from.Year() == to.Year() && to.Year() == now.Year():
		return metricsLayoutDate
	default:
		return metricsLayoutYear
	}
}

// metricsHeader is the comment line a get_metrics answer starts with, as in
// "# cpu 21:14:03–22:14:03 CET, step 36s". A range of one day other than
// today names its date, which its timestamps leave out.
func metricsHeader(resource string, from, to, now time.Time, layout, step string) string {
	var sb strings.Builder
	sb.WriteString("# " + resource + " ")
	if layout == metricsLayoutTime && !sameDay(to, now) {
		sb.WriteString(from.Format("2006-01-02 "))
	}
	sb.WriteString(from.Format(layout))
	if fromZone, toZone := from.Format("MST"), to.Format("MST"); fromZone != toZone {
		sb.WriteString(" " + fromZone) // the range spans a DST change
	}
	sb.WriteString("–" + to.Format(layout) + " " + to.Format("MST"))
	if step != "" {
		sb.WriteString(", step " + step)
	}
	sb.WriteString("\n")
	return sb.String()
}

// sameDay reports whether a and b fall on one calendar day of a's location.
func sameDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.In(a.Location()).Date()
	return ay == by && am == bm && ad == bd
}

// medianGap is the median time between consecutive samples, "" for fewer
// than two.
func medianGap(samples []*storage.AggregatedSample) string {
	var gaps []time.Duration
	for i := 1; i < len(samples); i++ {
		if samples[i] == nil || samples[i-1] == nil {
			continue
		}
		if d := samples[i].Timestamp.Sub(samples[i-1].Timestamp); d > 0 {
			gaps = append(gaps, d)
		}
	}
	if len(gaps) == 0 {
		return ""
	}
	slices.Sort(gaps)
	gap := gaps[len(gaps)/2]
	if gap < time.Second {
		return gap.String()
	}
	// "36h", not "36h0m0s", as the query's own steps are written.
	s := gap.Round(time.Second).String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
