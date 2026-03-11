package web

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/rand"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"kula-szpiegula/internal/collector"
	"kula-szpiegula/internal/config"
	"kula-szpiegula/internal/storage"
)

//go:embed static
var staticFS embed.FS

type Server struct {
	cfg       config.WebConfig
	global    config.GlobalConfig
	collector *collector.Collector
	store     *storage.Store
	auth      *AuthManager
	hub       *wsHub
	httpSrv   *http.Server
}

func NewServer(cfg config.WebConfig, global config.GlobalConfig, c *collector.Collector, s *storage.Store, storageDir string) *Server {
	srv := &Server{
		cfg:       cfg,
		global:    global,
		collector: c,
		store:     s,
		auth:      NewAuthManager(cfg.Auth, storageDir, cfg.TrustProxy),
		hub:       newWSHub(),
	}
	return srv
}

// BroadcastSample sends a new sample to all WebSocket clients.
func (s *Server) BroadcastSample(sample *collector.Sample) {
	data, err := json.Marshal(sample)
	if err != nil {
		return
	}
	s.hub.broadcast(data)
}

// statusResponseWriter captures the HTTP status code for logging.
type statusResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusResponseWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

// Hijack exposes the underlying http.Hijacker to allow WebSockets to upgrade the connection.
func (w *statusResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("underlying response writer does not support hijacking")
	}
	return h.Hijack()
}

func loggingMiddleware(cfg config.WebConfig, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !cfg.Logging.Enabled {
			next.ServeHTTP(w, r)
			return
		}

		start := time.Now()
		sw := &statusResponseWriter{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(sw, r)

		duration := time.Since(start)
		clientIP := getClientIP(r, cfg.TrustProxy)

		log.Printf("[API] %s %s %s %d %v", clientIP, r.Method, r.URL.Path, sw.status, duration)
	})
}

type gzipResponseWriter struct {
	io.Writer
	http.ResponseWriter
	wroteHeader bool
}

func (w *gzipResponseWriter) WriteHeader(status int) {
	if !w.wroteHeader {
		w.wroteHeader = true
		w.ResponseWriter.Header().Del("Content-Length")
		w.ResponseWriter.WriteHeader(status)
	}
}

func (w *gzipResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.Writer.Write(b)
}

func gzipMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") ||
			!strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}

		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		defer func() { _ = gz.Close() }()

		gzw := &gzipResponseWriter{Writer: gz, ResponseWriter: w}
		next.ServeHTTP(gzw, r)
	})
}

func securityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		nonce := base64.StdEncoding.EncodeToString(b)

		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", fmt.Sprintf("default-src 'self'; style-src 'self' fonts.googleapis.com; font-src fonts.gstatic.com; script-src 'self' 'nonce-%s'; connect-src 'self' ws: wss:;", nonce))
		next.ServeHTTP(w, r)
	})
}

func (s *Server) Start() error {
	if err := s.auth.LoadSessions(); err != nil {
		log.Printf("Warning: failed to load sessions: %v", err)
	}
	if s.cfg.TrustProxy {
		log.Printf("Security Note: TrustProxy is enabled. Ensure Kula is behind a trusted reverse proxy that handles X-Forwarded-For.")
	}

	mux := http.NewServeMux()

	// API routes
	apiMux := http.NewServeMux()
	apiMux.HandleFunc("/api/current", s.handleCurrent)
	apiMux.HandleFunc("/api/history", s.handleHistory)
	apiMux.HandleFunc("/api/config", s.handleConfig)
	apiMux.HandleFunc("/api/login", s.handleLogin)
	apiMux.HandleFunc("/api/logout", s.handleLogout)
	apiMux.HandleFunc("/api/auth/status", s.handleAuthStatus)

	// Wrap apiMux with logging
	loggedApiMux := loggingMiddleware(s.cfg, apiMux)

	// WebSocket
	apiMux.HandleFunc("/ws", s.handleWebSocket)

	// Apply auth to API routes (except login and auth status)
	mux.Handle("/api/login", loggedApiMux)
	mux.Handle("/api/logout", loggedApiMux)
	mux.Handle("/api/auth/status", loggedApiMux)
	mux.Handle("/api/", s.auth.AuthMiddleware(loggedApiMux))
	mux.Handle("/ws", s.auth.AuthMiddleware(loggedApiMux))

	// Static files
	staticContent, err := fs.Sub(staticFS, "static")
	if err != nil {
		return fmt.Errorf("static fs: %w", err)
	}
	mux.Handle("/", http.FileServer(http.FS(staticContent)))

	go s.hub.run()
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			s.auth.CleanupSessions()
		}
	}()

	var handler = securityMiddleware(mux)
	if s.cfg.EnableCompression {
		handler = gzipMiddleware(handler)
	}

	s.httpSrv = &http.Server{Handler: handler}

	listeners, err := s.createListeners()
	if err != nil {
		return err
	}

	errCh := make(chan error, len(listeners))
	for _, ln := range listeners {
		log.Printf("Web UI listening on http://%s", ln.Addr())
		go func(ln net.Listener) {
			errCh <- s.httpSrv.Serve(ln)
		}(ln)
	}

	if err := <-errCh; err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// createListeners resolves the configured Listen address into one or two
// net.Listeners according to the following rules:
//
//   - ""        → dual-stack: one tcp4 on 0.0.0.0 + one tcp6 on [::]
//   - "[::]"    → single tcp6 listener (kernel decides v4/v6 based on net.ipv6.bindv6only)
//   - "0.0.0.0" → single tcp4 listener (v4 only)
//   - "1.2.3.4" → single tcp4 listener bound to that address
//   - "[::1]"   → single tcp6 listener bound to that address
func (s *Server) createListeners() ([]net.Listener, error) {
	port := s.cfg.Port
	listen := s.cfg.Listen

	// Empty string: explicit dual-stack, one listener per family
	if listen == "" {
		ln4, err := net.Listen("tcp4", fmt.Sprintf("0.0.0.0:%d", port))
		if err != nil {
			return nil, fmt.Errorf("ipv4 listen: %w", err)
		}
		ln6, err := net.Listen("tcp6", fmt.Sprintf("[::]:%d", port))
		if err != nil {
			_ = ln4.Close()
			return nil, fmt.Errorf("ipv6 listen: %w", err)
		}
		return []net.Listener{ln4, ln6}, nil
	}

	// Strip brackets from IPv6 addresses like "[::1]" or "[::]"
	// so we can pass them to net.Listen as "[::1]:port"
	addr := fmt.Sprintf("%s:%d", listen, port)

	// Determine network type: if the host (after bracket-stripping) contains a
	// colon it is an IPv6 address and we use "tcp6", otherwise "tcp4".
	host := listen
	if len(host) > 1 && host[0] == '[' && host[len(host)-1] == ']' {
		host = host[1 : len(host)-1]
	}

	network := "tcp4"
	if net.ParseIP(host) != nil && net.ParseIP(host).To4() == nil {
		// Pure IPv6 address (no IPv4 representation)
		network = "tcp6"
	} else if host == "::" {
		network = "tcp6"
	}

	ln, err := net.Listen(network, addr)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", addr, err)
	}
	return []net.Listener{ln}, nil
}

// Shutdown gracefully stops the web server.
func (s *Server) Shutdown(ctx context.Context) error {
	if err := s.auth.SaveSessions(); err != nil {
		log.Printf("Warning: failed to save sessions: %v", err)
	}

	if s.httpSrv != nil {
		return s.httpSrv.Shutdown(ctx)
	}
	return nil
}

func (s *Server) handleCurrent(w http.ResponseWriter, r *http.Request) {
	sample := s.collector.Latest()
	if sample == nil {
		http.Error(w, `{"error":"no data yet"}`, http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(sample); err != nil {
		log.Printf("JSON encode error: %v", err)
	}
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	fromStr := r.URL.Query().Get("from")
	toStr := r.URL.Query().Get("to")

	var from, to time.Time
	var err error

	if toStr == "" {
		to = time.Now()
	} else {
		to, err = time.Parse(time.RFC3339, toStr)
		if err != nil {
			http.Error(w, `{"error":"invalid 'to' time"}`, http.StatusBadRequest)
			return
		}
	}

	if fromStr == "" {
		from = to.Add(-5 * time.Minute)
	} else {
		from, err = time.Parse(time.RFC3339, fromStr)
		if err != nil {
			http.Error(w, `{"error":"invalid 'from' time"}`, http.StatusBadRequest)
			return
		}
	}

	if to.Sub(from) > 31*24*time.Hour {
		http.Error(w, `{"error":"time range too large, max 31 days allowed"}`, http.StatusBadRequest)
		return
	}
	if to.Sub(from) < 0 {
		http.Error(w, `{"error":"time range inverted"}`, http.StatusBadRequest)
		return
	}

	pointsStr := r.URL.Query().Get("points")
	points := 450
	if pointsStr != "" {
		_, _ = fmt.Sscanf(pointsStr, "%d", &points)
	}

	startLoad := time.Now()
	result, err := s.store.QueryRangeWithMeta(from, to, points)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err), http.StatusInternalServerError)
		return
	}
	loadDuration := time.Since(startLoad)

	// level "perf": log DB fetch performance for /api/history
	// level "debug": also enables collector auto-discovery logging (see collector.debugf)
	if s.cfg.Logging.Enabled && s.cfg.Logging.Level == "perf" {
		log.Printf("[API History] loaded %d samples from tier %d (resolution: %s) for window %s in %v", len(result.Samples), result.Tier, result.Resolution, to.Sub(from).Round(time.Second), loadDuration)
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		log.Printf("JSON encode error: %v", err)
	}
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	hostname := s.global.Hostname
	if hostname == "" {
		hostname, _ = os.Hostname()
	}

	info := map[string]interface{}{
		"auth_enabled":     s.cfg.Auth.Enabled,
		"version":          s.cfg.Version,
		"join_metrics":     s.cfg.JoinMetrics,
		"os":               s.cfg.OS,
		"kernel":           s.cfg.Kernel,
		"arch":             s.cfg.Arch,
		"hostname":         hostname,
		"show_system_info": s.global.ShowSystemInfo,
		"theme":            s.global.DefaultTheme,
		"aggregation":      s.cfg.DefaultAggregation,
		"graphs": map[string]interface{}{
			"cpu_temp": map[string]interface{}{
				"mode":  s.cfg.Graphs.CPUTemp.MaxMode,
				"value": s.cfg.Graphs.CPUTemp.MaxValue,
				"auto":  collector.DetectTjMax(),
			},
			"disk_temp": map[string]interface{}{
				"mode":  s.cfg.Graphs.DiskTemp.MaxMode,
				"value": s.cfg.Graphs.DiskTemp.MaxValue,
				"auto":  collector.DetectDiskTjMax(),
			},
			"network": map[string]interface{}{
				"mode":  s.cfg.Graphs.Network.MaxMode,
				"value": s.cfg.Graphs.Network.MaxValue,
				"auto":  collector.DetectLinkSpeed(),
			},
		},
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(info); err != nil {
		log.Printf("JSON encode error: %v", err)
	}
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	ip := getClientIP(r, s.cfg.TrustProxy)

	if !s.auth.Limiter.Allow(ip) {
		http.Error(w, `{"error":"too many requests"}`, http.StatusTooManyRequests)
		return
	}

	var creds struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&creds); err != nil {
		http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
		return
	}

	if !s.auth.ValidateCredentials(creds.Username, creds.Password) {
		http.Error(w, `{"error":"invalid credentials"}`, http.StatusUnauthorized)
		return
	}

	token, err := s.auth.CreateSession(creds.Username, ip, r.UserAgent())
	if err != nil {
		http.Error(w, `{"error":"internal server error"}`, http.StatusInternalServerError)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "kula_session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil || (s.cfg.TrustProxy && r.Header.Get("X-Forwarded-Proto") == "https"),
		MaxAge:   int(s.cfg.Auth.SessionTimeout.Seconds()),
		SameSite: http.SameSiteStrictMode,
	})

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]string{"token": token}); err != nil {
		log.Printf("JSON encode error: %v", err)
	}
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	cookie, err := r.Cookie("kula_session")
	if err == nil && cookie.Value != "" {
		s.auth.RevokeSession(cookie.Value)
	}

	// Delete the cookie on the client side
	http.SetCookie(w, &http.Cookie{
		Name:     "kula_session",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil || (s.cfg.TrustProxy && r.Header.Get("X-Forwarded-Proto") == "https"),
		MaxAge:   -1,
		SameSite: http.SameSiteStrictMode,
	})

	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "logged out"})
}

func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	status := map[string]interface{}{
		"auth_required": s.cfg.Auth.Enabled,
		"authenticated": false,
	}

	if !s.cfg.Auth.Enabled {
		status["authenticated"] = true
	} else {
		ip := getClientIP(r, s.cfg.TrustProxy)
		userAgent := r.UserAgent()

		cookie, err := r.Cookie("kula_session")
		if err == nil && s.auth.ValidateSession(cookie.Value, ip, userAgent) {
			status["authenticated"] = true
		}
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(status); err != nil {
		log.Printf("JSON encode error: %v", err)
	}
}

// wsHub manages WebSocket connections
type wsHub struct {
	mu      sync.RWMutex
	clients map[*wsClient]bool
	regCh   chan *wsClient
	unregCh chan *wsClient
}

func newWSHub() *wsHub {
	return &wsHub{
		clients: make(map[*wsClient]bool),
		regCh:   make(chan *wsClient, 16),
		unregCh: make(chan *wsClient, 16),
	}
}

func (h *wsHub) run() {
	for {
		select {
		case client := <-h.regCh:
			h.mu.Lock()
			h.clients[client] = true
			h.mu.Unlock()
		case client := <-h.unregCh:
			h.mu.Lock()
			delete(h.clients, client)
			h.mu.Unlock()
		}
	}
}

func (h *wsHub) broadcast(data []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	for client := range h.clients {
		if !client.paused {
			select {
			case client.sendCh <- data:
			default:
				// Client too slow, skip
			}
		}
	}
}

// getClientIP extracts the real client IP, considering proxies and stripping ephemeral ports.
func getClientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// Take only the first (leftmost) IP from the chain
			return strings.TrimSpace(strings.SplitN(xff, ",", 2)[0])
		}
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr // fallback if it doesn't have a port for some reason
	}
	return host
}
