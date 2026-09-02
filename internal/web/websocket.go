package web

import (
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// wsSessionRecheckInterval is how often an established WebSocket re-validates
// the session that opened it. Authentication is otherwise checked exactly once,
// at the upgrade, so without this a socket opened moments before a session's
// absolute deadline would keep streaming metrics for as long as it stayed
// connected — the connection would outlive the very ceiling
// session_max_lifetime advertises. It is a var so tests can shorten it.
var wsSessionRecheckInterval = 30 * time.Second

type wsClient struct {
	conn   *websocket.Conn
	sendCh chan []byte
	paused bool
	mu     sync.Mutex

	// closed reports whether sendCh has been closed. It is guarded by the
	// hub's mutex, not by mu: the hub owns sendCh's lifetime so that the close
	// and the removal from the clients map happen under one lock, out of reach
	// of a concurrent broadcast. See wsHub.run.
	closed bool
}

// newUpgrader builds a websocket.Upgrader whose CheckOrigin closure reads
// the server's security configuration. Defaults preserve the original
// strict host-match behavior; OriginValidation=false disables the check
// entirely; AllowedOrigins entries are accepted in addition to same-host.
func (s *Server) newUpgrader() websocket.Upgrader {
	return websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin: func(r *http.Request) bool {
			if !s.cfg.Security.OriginValidation {
				return true
			}

			origin := r.Header.Get("Origin")
			if origin == "" {
				// Explicitly allow non-browser clients (like CLI tools) which omit the Origin
				// header. Browsers always send an Origin header for WebSocket connections.
				return true
			}

			// Parse the Origin header securely using net/url to prevent crafted origin bypasses.
			u, err := url.ParseRequestURI(origin)
			if err != nil {
				log.Printf("WebSocket upgrade blocked: invalid Origin header format (%v)", err)
				return false
			}

			// Same-host match: prevents Cross-Site WebSocket Hijacking (CSWSH).
			if u.Host == r.Host {
				return true
			}

			// Accept origins explicitly allow-listed for cross-origin access.
			originScheme := strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host)
			for _, allowed := range s.cfg.Security.AllowedOrigins {
				if strings.EqualFold(originScheme, allowed) {
					return true
				}
			}

			log.Printf("WebSocket upgrade blocked: Origin (%s) does not match Host (%s) and is not in allowed_origins", u.Host, r.Host)
			return false
		},
	}
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	ip := getClientIP(r, s.cfg.TrustProxy)

	// Remember which session authorized this upgrade. AuthMiddleware has already
	// validated it; we keep the token so the write pump can keep checking it for
	// as long as the socket lives. An empty token when auth is enabled cannot
	// happen through the middleware, and would fail the first re-check anyway.
	var sessionToken string
	if s.cfg.Auth.Enabled {
		sessionToken = s.auth.authenticatedSessionToken(r)
	}

	s.wsMu.Lock()
	if s.wsCount >= s.cfg.MaxWebsocketConns {
		s.wsMu.Unlock()
		log.Printf("WebSocket upgrade rejected: global limit reached (%d)", s.cfg.MaxWebsocketConns)
		http.Error(w, "Global connection limit reached", http.StatusTooManyRequests)
		return
	}
	if s.wsIPCounts[ip] >= s.cfg.MaxWebsocketConnsPerIP {
		s.wsMu.Unlock()
		log.Printf("WebSocket upgrade rejected: IP limit reached for %s (%d)", ip, s.cfg.MaxWebsocketConnsPerIP)
		http.Error(w, "Per-IP connection limit reached", http.StatusTooManyRequests)
		return
	}
	s.wsCount++
	s.wsIPCounts[ip]++
	s.wsMu.Unlock()

	upg := s.newUpgrader()
	upg.EnableCompression = s.cfg.EnableCompression

	// Origin is validated by newUpgrader()'s CheckOrigin (Host + allowed_origins), which semgrep can't follow from here.
	// nosemgrep: websocket-missing-origin-check
	conn, err := upg.Upgrade(w, r, nil)
	if err != nil {
		s.wsMu.Lock()
		if s.wsCount > 0 {
			s.wsCount--
		}
		if s.wsIPCounts[ip] > 0 {
			s.wsIPCounts[ip]--
		}
		if s.wsIPCounts[ip] == 0 {
			delete(s.wsIPCounts, ip)
		}
		s.wsMu.Unlock()
		log.Printf("WebSocket upgrade error: %v", err)
		return
	}

	conn.SetReadLimit(4096) // Limit incoming JSON commands to prevent memory exhaustion

	client := &wsClient{
		conn:   conn,
		sendCh: make(chan []byte, 64),
	}

	var unregOnce sync.Once
	unregister := func() {
		unregOnce.Do(func() {
			s.wsMu.Lock()
			if s.wsCount > 0 {
				s.wsCount--
			}
			if s.wsIPCounts[ip] > 0 {
				s.wsIPCounts[ip]--
			}
			if s.wsIPCounts[ip] == 0 {
				delete(s.wsIPCounts, ip)
			}
			s.wsMu.Unlock()
			// Hand the client to the hub and let it close sendCh; closing it
			// here would race a broadcast that is still iterating the clients
			// map. The write pump below still learns of the close through the
			// `!ok` receive, just via the hub.
			s.hub.unregCh <- client
		})
	}

	s.hub.regCh <- client
	defer func() {
		unregister()
		_ = conn.Close()
	}()

	// Send initial current data
	if sample := s.collector.Latest(); sample != nil {
		data, err := json.Marshal(sample)
		if err == nil {
			_ = conn.WriteMessage(websocket.TextMessage, data)
		}
	}

	// Read pump (for pause/resume commands)
	go func() {
		// Set an initial read deadline
		_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		conn.SetPongHandler(func(string) error {
			_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))
			return nil
		})

		for {
			var cmd struct {
				Action string `json:"action"`
			}
			err := conn.ReadJSON(&cmd)
			if err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
					log.Printf("WebSocket read unexpected error: %v", err)
				}
				unregister()
				return
			}

			client.mu.Lock()
			switch cmd.Action {
			case "pause":
				client.paused = true
			case "resume":
				client.paused = false
			}
			client.mu.Unlock()
		}
	}()

	// Write pump
	ticker := time.NewTicker(50 * time.Second) // Must be less than read deadline
	defer ticker.Stop()

	// A nil channel blocks forever, so with auth disabled the session case below
	// simply never fires.
	var sessionCheck <-chan time.Time
	if s.cfg.Auth.Enabled {
		sessionTicker := time.NewTicker(wsSessionRecheckInterval)
		defer sessionTicker.Stop()
		sessionCheck = sessionTicker.C
	}

	for {
		select {
		case data, ok := <-client.sendCh:
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if !ok {
				// The hub closed the channel
				_ = conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
				log.Printf("WebSocket write error: %v", err)
				return
			}
		case <-ticker.C:
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		case <-sessionCheck:
			// Expired, revoked, or past its absolute lifetime: stop streaming and
			// hang up. Returning runs the deferred unregister and Close.
			if !s.auth.ValidateSession(sessionToken) {
				_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
				_ = conn.WriteMessage(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "session expired"))
				return
			}
		}
	}
}
