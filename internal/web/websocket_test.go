package web

	import (
		"net/http"
		"net/http/httptest"
		"strings"
		"testing"
		"time"

		"kula/internal/collector"
		"kula/internal/config"
		"github.com/gorilla/websocket"
	)

func TestWebSocketConnectionLimits(t *testing.T) {
	cfg := config.WebConfig{
		MaxWebsocketConns:      3,
		MaxWebsocketConnsPerIP: 2,
	}
	c := collector.New(config.GlobalConfig{}, config.CollectionConfig{}, config.ApplicationsConfig{}, "")
	s := NewServer(cfg, config.GlobalConfig{}, c, nil, t.TempDir(), config.OllamaConfig{})
	
	// Start hub to process registration/unregistration
	go s.hub.run()
	server := httptest.NewServer(http.HandlerFunc(s.handleWebSocket))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")

	dialer := websocket.Dialer{}

	// Helper to open a connection
	openConn := func() (*websocket.Conn, *http.Response, error) {
		return dialer.Dial(wsURL, nil)
	}

	// 1. Open 2 connections from same IP (should succeed)
	c1, _, err := openConn()
	if err != nil {
		t.Fatalf("Failed to open first connection: %v", err)
	}
	defer func() { _ = c1.Close() }()

	c2, _, err := openConn()
	if err != nil {
		t.Fatalf("Failed to open second connection: %v", err)
	}
	defer func() { _ = c2.Close() }()

	// 2. Open 3rd connection from same IP (should fail due to per-IP limit)
	_, resp, err := openConn()
	if err == nil {
		t.Fatal("Expected third connection to fail due to per-IP limit, but it succeeded")
	}
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("Expected status 429, got %d", resp.StatusCode)
	}

	// 3. Close one connection and try again (should succeed)
	_ = c1.Close()
	// Wait a bit for the unregister logic to run (hub is async but counts are sync in unregister func)
	// Actually unregister is called in defer in handleWebSocket which runs when the pumps exit.
	// Since we closed c1, the pumps should exit soon.
	// Let's force a bit of delay or check in a loop.
	
	retryCount := 0
	var c3 *websocket.Conn
	for retryCount < 10 {
		c3, _, err = openConn()
		if err == nil {
			break
		}
		retryCount++
		// Wait for goroutines to clean up
		// Small sleep is usually enough for local tests
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("Failed to open connection after closing one: %v", err)
	}
	defer func() { _ = c3.Close() }()

	// 4. Test global limit
	// Current connections: c2, c3 (Total: 2, Limit: 3)
	// We need another IP to bypass per-IP limit or just increase IP limit for this test.
	s.wsMu.Lock()
	s.cfg.MaxWebsocketConnsPerIP = 10
	s.wsMu.Unlock()

	c4, _, err := openConn()
	if err != nil {
		t.Fatalf("Failed to open fourth connection: %v", err)
	}
	defer func() { _ = c4.Close() }()

	// 5. Next one should fail global limit (Total: 3, Limit: 3)
	_, resp, err = openConn()
	if err == nil {
		t.Fatal("Expected fifth connection to fail due to global limit, but it succeeded")
	}
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("Expected status 429, got %d", resp.StatusCode)
	}
}
