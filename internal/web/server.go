package web

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha512"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"kula/internal/collector"
	"kula/internal/config"
	"kula/internal/i18n"
	"kula/internal/storage"
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
	templates *template.Template
	sriHashes map[string]string

	wsMu       sync.Mutex
	wsCount    int
	wsIPCounts map[string]int
}

func NewServer(cfg config.WebConfig, global config.GlobalConfig, c *collector.Collector, s *storage.Store, storageDir string) *Server {
	srv := &Server{
		cfg:        cfg,
		global:     global,
		collector:  c,
		store:      s,
		auth:       NewAuthManager(cfg.Auth, storageDir, cfg.TrustProxy),
		hub:        newWSHub(),
		sriHashes:  make(map[string]string),
		wsIPCounts: make(map[string]int),
	}
	srv.initializeTemplates()
	srv.calculateSRIs()
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

// jsonError writes a properly marshalled JSON error response, preventing injection
// from special characters (quotes, backslashes, newlines) in error strings.
func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	b, _ := json.Marshal(map[string]string{"error": msg})
	_, _ = w.Write(b)
}

type contextKey string

const nonceKey contextKey = "csp_nonce"

func (s *Server) securityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		// Nonce for CloudFlare's JS challenge
		nonce := base64.StdEncoding.EncodeToString(b)

		// Inject nonce into context
		ctx := context.WithValue(r.Context(), nonceKey, nonce)

		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", fmt.Sprintf("default-src 'self'; script-src 'self' 'nonce-%s'; style-src 'self' 'unsafe-inline'; frame-ancestors 'none';", nonce))
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("Permissions-Policy", "geolocation=(), microphone=(), camera=()")
		if r.TLS != nil || (s.cfg.TrustProxy && r.Header.Get("X-Forwarded-Proto") == "https") {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) Start() error {
	if !s.cfg.Enabled {
		return nil
	}
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
	apiMux.HandleFunc("/api/i18n", s.handleI18n)

	// Wrap apiMux with logging and CSRF protection
	loggedApiMux := s.auth.CSRFMiddleware(loggingMiddleware(s.cfg, apiMux))

	// Register /ws separately and explicitly through auth, CSRF and logging
	wsHandler := s.auth.AuthMiddleware(
		s.auth.CSRFMiddleware(
			loggingMiddleware(s.cfg, http.HandlerFunc(s.handleWebSocket)),
		),
	)

	if s.cfg.UI {
		// Apply auth to API routes (except login and auth status)
		mux.Handle("/api/login", loggedApiMux)
		mux.Handle("/api/logout", loggedApiMux)
		mux.Handle("/api/auth/status", loggedApiMux)
		mux.Handle("/api/", s.auth.AuthMiddleware(loggedApiMux))
		mux.Handle("/ws", wsHandler)

		// Templated HTML files
		mux.HandleFunc("/", s.handleIndex)
		mux.HandleFunc("/index.html", s.handleIndex)
		mux.HandleFunc("/game.html", s.handleGame)

		// Static assets handler
		mux.HandleFunc("/js/", s.handleStatic)
		mux.HandleFunc("/fonts/", s.handleStatic)
		mux.HandleFunc("/style.css", s.handleStatic)
		mux.HandleFunc("/game.css", s.handleStatic)
		mux.HandleFunc("/game.js", s.handleStatic)
		mux.HandleFunc("/kula.svg", s.handleStatic)
		mux.HandleFunc("/favicon.ico", s.handleStatic)

		log.Printf("Web UI and API enabled")
	} else {
		log.Printf("Web UI and API disabled")
	}

	if s.cfg.PrometheusMetrics.Enabled {
		mux.Handle("/metrics", loggingMiddleware(s.cfg, http.HandlerFunc(s.handleMetrics)))
		if s.cfg.PrometheusMetrics.Token != "" {
			log.Printf("Prometheus metrics enabled at /metrics with bearer token authentication")
		} else {
			log.Printf("Prometheus metrics enabled at /metrics without authentication")
		}
	}

	// Apply request logging to liveness endpoints when enabled
	if s.cfg.Logging.Enabled {
		mux.Handle("/health", loggingMiddleware(s.cfg, http.HandlerFunc(s.handleHealth)))
		mux.Handle("/status", loggingMiddleware(s.cfg, http.HandlerFunc(s.handleHealth)))
	} else {
		// Fallback registrations when logging is disabled
		mux.HandleFunc("/health", s.handleHealth)
		mux.HandleFunc("/status", s.handleHealth)
	}

	go s.hub.run()
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			s.auth.CleanupSessions()
		}
	}()

	var handler = s.securityMiddleware(mux)
	if s.cfg.EnableCompression {
		handler = gzipMiddleware(handler)
	}

	s.httpSrv = &http.Server{
		Handler:      handler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second, // allow longer for /api/history large payloads
		IdleTimeout:  120 * time.Second,
	}

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
		jsonError(w, "no data yet", http.StatusServiceUnavailable)
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
			jsonError(w, "invalid 'to' time", http.StatusBadRequest)
			return
		}
	}

	if fromStr == "" {
		from = to.Add(-5 * time.Minute)
	} else {
		from, err = time.Parse(time.RFC3339, fromStr)
		if err != nil {
			jsonError(w, "invalid 'from' time", http.StatusBadRequest)
			return
		}
	}

	if to.Sub(from) > 31*24*time.Hour {
		jsonError(w, "time range too large, max 31 days allowed", http.StatusBadRequest)
		return
	}
	if to.Sub(from) < 0 {
		jsonError(w, "time range inverted", http.StatusBadRequest)
		return
	}

	pointsStr := r.URL.Query().Get("points")
	points := 450
	if pointsStr != "" {
		_, _ = fmt.Sscanf(pointsStr, "%d", &points)
	}

	// Cap points to prevent resource exhaustion
	if points > 5000 {
		points = 5000
	}
	if points < 1 {
		points = 1
	}

	startLoad := time.Now()
	result, err := s.store.QueryRangeWithMeta(from, to, points)
	if err != nil {
		log.Printf("[API History] query error: %v", err)
		jsonError(w, "internal storage error", http.StatusInternalServerError)
		return
	}
	loadDuration := time.Since(startLoad)

	// level "perf": log DB fetch performance for /api/history
	// level "debug": also enables collector auto-discovery logging (see collector.debugf)
	if s.cfg.Logging.Enabled && (s.cfg.Logging.Level == "perf" || s.cfg.Logging.Level == "debug") {
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
		"join_metrics":     s.cfg.JoinMetrics,
		"os":               s.cfg.OS,
		"kernel":           s.cfg.Kernel,
		"arch":             s.cfg.Arch,
		"hostname":         hostname,
		"show_system_info": s.global.ShowSystemInfo,
		"show_version":     s.global.ShowVersion,
		"theme":            s.global.DefaultTheme,
		"aggregation":      s.cfg.DefaultAggregation,
		"graphs": map[string]interface{}{
			"cpu_temp": map[string]interface{}{
				"mode":  s.cfg.Graphs.CPUTemp.MaxMode,
				"value": s.cfg.Graphs.CPUTemp.MaxValue,
				"auto":  s.collector.DetectTjMax(),
			},
			"disk_temp": map[string]interface{}{
				"mode":  s.cfg.Graphs.DiskTemp.MaxMode,
				"value": s.cfg.Graphs.DiskTemp.MaxValue,
				"auto":  s.collector.DetectDiskTjMax(),
			},
			"network": map[string]interface{}{
				"mode":  s.cfg.Graphs.Network.MaxMode,
				"value": s.cfg.Graphs.Network.MaxValue,
				"auto":  s.collector.DetectLinkSpeed(),
			},
			"split": map[string]interface{}{
				"network":    s.cfg.Graphs.Split.Network,
				"disk_io":    s.cfg.Graphs.Split.DiskIo,
				"disk_space": s.cfg.Graphs.Split.DiskSpace,
				"disk_temp":  s.cfg.Graphs.Split.DiskTemp,
				"gpu":        s.cfg.Graphs.Split.Gpu,
			},
		},
		"lang": map[string]interface{}{
			"default": s.cfg.Lang.Default,
			"force":   s.cfg.Lang.Force,
		},
	}

	// Expose custom metric configs so the frontend knows units and maxima
	if customCfg := s.collector.CustomConfig(); len(customCfg) > 0 {
		cm := make(map[string]interface{}, len(customCfg))
		for group, metrics := range customCfg {
			var mList []map[string]interface{}
			for _, m := range metrics {
				mList = append(mList, map[string]interface{}{
					"name": m.Name,
					"unit": m.Unit,
					"max":  m.Max,
				})
			}
			cm[group] = mList
		}
		info["custom_metrics"] = cm
	}

	if s.global.ShowVersion {
		info["version"] = s.cfg.Version
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(info); err != nil {
		log.Printf("JSON encode error: %v", err)
	}
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ip := getClientIP(r, s.cfg.TrustProxy)

	if !s.auth.Limiter.Allow(ip) {
		jsonError(w, "too many requests", http.StatusTooManyRequests)
		return
	}

	var creds struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := json.NewDecoder(r.Body).Decode(&creds); err != nil {
		jsonError(w, "invalid request", http.StatusBadRequest)
		return
	}

	if !s.auth.UserLimiter.Allow(strings.ToLower(creds.Username)) {
		jsonError(w, "too many requests", http.StatusTooManyRequests)
		return
	}

	if !s.auth.ValidateCredentials(creds.Username, creds.Password) {
		jsonError(w, "invalid credentials", http.StatusUnauthorized)
		return
	}

	token, err := s.auth.CreateSession(creds.Username)
	if err != nil {
		jsonError(w, "internal server error", http.StatusInternalServerError)
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

	csrfToken := s.auth.GetCSRFToken(token)

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]string{
		"status":     "logged in",
		"csrf_token": csrfToken,
	}); err != nil {
		log.Printf("JSON encode error: %v", err)
	}
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
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
		cookie, err := r.Cookie("kula_session")
		if err == nil && s.auth.ValidateSession(cookie.Value) {
			status["authenticated"] = true
			status["csrf_token"] = s.auth.GetCSRFToken(cookie.Value)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(status); err != nil {
		log.Printf("JSON encode error: %v", err)
	}
}

func (s *Server) handleI18n(w http.ResponseWriter, r *http.Request) {
	lang := r.URL.Query().Get("lang")
	if lang == "" {
		lang = "en"
	}

	// Validate against the known set of supported languages.
	valid := false
	for _, l := range i18n.SupportedLangs {
		if lang == l {
			valid = true
			break
		}
	}
	if !valid {
		jsonError(w, "invalid language", http.StatusBadRequest)
		return
	}

	data, err := i18n.GetRawLocale(lang)
	if err != nil {
		// Fallback to English if language not found
		if lang != "en" {
			data, err = i18n.GetRawLocale("en")
		}
		if err != nil {
			jsonError(w, "translation not found", http.StatusNotFound)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
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
		client.mu.Lock()
		paused := client.paused
		client.mu.Unlock()
		if !paused {
			select {
			case client.sendCh <- data:
			default:
				// Client too slow, skip
			}
		}
	}
}

func (s *Server) initializeTemplates() {
	var err error
	s.templates = template.New("base").Funcs(template.FuncMap{
		"sri": func(path string) string {
			return s.sriHashes[path]
		},
	})
	s.templates, err = s.templates.ParseFS(staticFS, "static/*.html")
	if err != nil {
		log.Fatalf("failed to parse templates: %v", err)
	}
}

func (s *Server) calculateSRIs() {
	_ = fs.WalkDir(staticFS, "static", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".js") {
			return nil
		}

		data, err := staticFS.ReadFile(path)
		if err != nil {
			log.Printf("Warning: failed to read %s for SRI: %v", path, err)
			return nil
		}

		sum := sha512.Sum384(data)
		hash := "sha384-" + base64.StdEncoding.EncodeToString(sum[:])

		// Key 1: path relative to static/ (e.g. "js/app/main.js")
		key := strings.TrimPrefix(path, "static/")
		s.sriHashes[key] = hash

		// Key 2: filename only (e.g. "main.js") for backward compatibility
		s.sriHashes[filepath.Base(path)] = hash

		return nil
	})
}

// getClientIP extracts the real client IP, considering proxies and stripping ephemeral ports.
func getClientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			// The rightmost IP is the one appended by our trusted proxy.
			// Leftmost IPs are client-controlled and can be spoofed.
			return strings.TrimSpace(parts[len(parts)-1])
		}
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr // fallback if it doesn't have a port for some reason
	}
	return host
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("kula is healthy"))
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/index.html" {
		s.handleStatic(w, r)
		return
	}
	s.renderTemplate(w, r, "index.html")
}

func (s *Server) handleGame(w http.ResponseWriter, r *http.Request) {
	s.renderTemplate(w, r, "game.html")
}

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	if path == "" {
		s.handleIndex(w, r)
		return
	}

	fullPath := "static/" + path

	// Security: prevent directory listing
	stat, err := staticFS.Open(fullPath)
	if err == nil {
		info, _ := stat.Stat()
		_ = stat.Close()
		if info.IsDir() {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
	}

	data, err := staticFS.ReadFile(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "Not Found", http.StatusNotFound)
		} else {
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
		return
	}

	// Set content type
	contentType := "application/octet-stream"
	if strings.HasSuffix(path, ".js") {
		contentType = "application/javascript"
	} else if strings.HasSuffix(path, ".css") {
		contentType = "text/css"
	} else if strings.HasSuffix(path, ".svg") {
		contentType = "image/svg+xml"
	} else if strings.HasSuffix(path, ".ico") {
		contentType = "image/x-icon"
	} else if strings.HasSuffix(path, ".html") {
		contentType = "text/html; charset=utf-8"
	} else if strings.Contains(path, "/fonts/") {
		if strings.HasSuffix(path, ".woff2") {
			contentType = "font/woff2"
		} else if strings.HasSuffix(path, ".woff") {
			contentType = "font/woff"
		} else if strings.HasSuffix(path, ".ttf") {
			contentType = "font/ttf"
		}
	}
	w.Header().Set("Content-Type", contentType)

	// Check if it's a JS file and we have an SRI for it (optional: could also add SRI header but browser does it via script tag)
	// We just serve the content here.
	_, _ = w.Write(data)
}

func (s *Server) renderTemplate(w http.ResponseWriter, r *http.Request, templateName string) {
	nonce, ok := r.Context().Value(nonceKey).(string)
	if !ok {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	data := struct {
		Nonce       string
		SRI         map[string]string
		AuthEnabled bool
		LangForce   bool
	}{
		Nonce:       nonce,
		SRI:         s.sriHashes,
		AuthEnabled: s.cfg.Auth.Enabled,
		LangForce:   s.cfg.Lang.Force,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(w, templateName, data); err != nil {
		log.Printf("Template execution error: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}
