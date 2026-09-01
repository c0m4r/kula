package web

import (
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"kula/internal/collector"
	"kula/internal/config"
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

// TestHubHasClientsAndBroadcast covers the zero-client broadcast skip used by
// BroadcastSample: a fresh hub reports no clients (so the per-tick JSON marshal
// is skipped), and once a client is registered hasClients() flips to true and
// broadcast still delivers the payload — connected clients are unaffected.
func TestHubHasClientsAndBroadcast(t *testing.T) {
	h := newWSHub()
	if h.hasClients() {
		t.Fatal("fresh hub should report no clients")
	}

	c := &wsClient{sendCh: make(chan []byte, 1)}
	h.mu.Lock()
	h.clients[c] = true
	h.mu.Unlock()

	if !h.hasClients() {
		t.Fatal("hub should report a client after registration")
	}

	h.broadcast([]byte("payload"))
	select {
	case got := <-c.sendCh:
		if string(got) != "payload" {
			t.Fatalf("delivered %q, want %q", got, "payload")
		}
	default:
		t.Fatal("broadcast did not deliver to the registered client")
	}

	// A paused client must not receive broadcasts (unchanged behaviour).
	c.paused = true
	h.broadcast([]byte("second"))
	select {
	case got := <-c.sendCh:
		t.Fatalf("paused client unexpectedly received %q", got)
	default:
	}
}

// waitForHub blocks until cond (evaluated under the hub read lock) holds, or
// fails the test. Hub state transitions are driven by run()'s goroutine, so
// tests observe them by polling rather than by direct handoff.
func waitForHub(t *testing.T, h *wsHub, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.RLock()
		ok := cond()
		h.mu.RUnlock()
		if ok {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestHubUnregisterDoesNotRaceBroadcast is a regression test for a panic where
// a disconnecting client closed its own sendCh while a broadcast was still
// iterating the clients map. unregCh is buffered and drained asynchronously, so
// the client was still in the map when the channel closed, and broadcast's
// `client.sendCh <- data` panicked with "send on closed channel" — a send on a
// closed channel panics rather than taking the select's default branch. Because
// BroadcastSample runs on the collection goroutine with no recover, that killed
// the whole daemon on any WebSocket disconnect that landed in the window.
//
// The hub now owns sendCh's lifetime and closes it under the same lock that
// removes the client, so the two can no longer overlap.
//
// It drives real connections through handleWebSocket rather than simulating the
// unregister inline, so it still fails if the close is moved back onto the
// connection goroutine. Connection limits are set well above the concurrency
// used here so the upgrade gate never trips: a run in which dials are being
// rejected is not exercising the disconnect path, so the dial count is asserted
// rather than ignored.
func TestHubUnregisterDoesNotRaceBroadcast(t *testing.T) {
	const (
		dialers      = 16
		perDialer    = 10
		totalDials   = dialers * perDialer
		connLimit    = 64 // >> dialers, so no dial is ever rejected
		minSuccesses = totalDials / 2
	)

	cfg := config.WebConfig{
		MaxWebsocketConns:      connLimit,
		MaxWebsocketConnsPerIP: connLimit,
	}
	c := collector.New(config.GlobalConfig{}, config.CollectionConfig{}, config.ApplicationsConfig{}, "")
	s := NewServer(cfg, config.GlobalConfig{}, c, nil, t.TempDir(), config.OllamaConfig{})

	go s.hub.run()
	server := httptest.NewServer(http.HandlerFunc(s.handleWebSocket))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")

	// Broadcast continuously so a disconnect is likely to land while broadcast
	// holds the read lock and is iterating the clients map — the window the
	// panic lived in.
	stop := make(chan struct{})
	var pump sync.WaitGroup
	pump.Add(1)
	go func() {
		defer pump.Done()
		for {
			select {
			case <-stop:
				return
			default:
				s.hub.broadcast([]byte(`{"ts":0}`))
			}
		}
	}()

	var succeeded atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < dialers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perDialer; j++ {
				conn, _, err := (&websocket.Dialer{}).Dial(wsURL, nil)
				if err != nil {
					continue
				}
				succeeded.Add(1)
				_ = conn.Close()
			}
		}()
	}
	wg.Wait()

	close(stop)
	pump.Wait()

	if n := succeeded.Load(); n < minSuccesses {
		t.Fatalf("only %d of %d dials connected; the disconnect path was not exercised", n, totalDials)
	}
}

// TestHubUnregisterClosesSendCh pins the ownership contract directly: handing a
// client to the hub on unregCh must close its sendCh, since the write pump in
// handleWebSocket relies on that `!ok` receive to exit.
func TestHubUnregisterClosesSendCh(t *testing.T) {
	h := newWSHub()
	go h.run()

	c := &wsClient{sendCh: make(chan []byte, 1)}
	h.regCh <- c
	waitForHub(t, h, "client registration", func() bool {
		_, ok := h.clients[c]
		return ok
	})

	h.unregCh <- c
	select {
	case _, ok := <-c.sendCh:
		if ok {
			t.Fatal("expected sendCh to be closed, got a value")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("hub did not close sendCh on unregister; the write pump would block forever")
	}
}

// TestHubUnregisterBeforeRegister covers the ordering the hub cannot control:
// regCh and unregCh are distinct buffered channels and select picks at random
// among ready cases, so an unregister may be handled before its register. The
// client must stay out of the map — re-adding one whose sendCh is already
// closed would hand the next broadcast a closed channel to send on.
func TestHubUnregisterBeforeRegister(t *testing.T) {
	h := newWSHub()
	c := &wsClient{sendCh: make(chan []byte, 1)}

	// Step 1: queue the unregister with nothing else pending, then start the
	// hub. unregCh is the only ready case, so the tombstone is set before any
	// register is seen — the inverted order is forced, not hoped for.
	h.unregCh <- c
	go h.run()
	waitForHub(t, h, "unregister to close sendCh", func() bool { return c.closed })

	// Step 2: the late register, followed by a barrier client on the same
	// channel. Channels are FIFO, so once the barrier is in the map the hub has
	// necessarily already processed c's registration — and must have ignored it.
	barrier := &wsClient{sendCh: make(chan []byte, 1)}
	h.regCh <- c
	h.regCh <- barrier
	waitForHub(t, h, "barrier registration", func() bool {
		_, ok := h.clients[barrier]
		return ok
	})

	h.mu.RLock()
	_, resurrected := h.clients[c]
	h.mu.RUnlock()
	if resurrected {
		t.Fatal("hub re-added a client that was already unregistered; a broadcast would send on its closed channel")
	}

	// And the broadcast that would have panicked is a no-op for c.
	h.broadcast([]byte("sample"))
	select {
	case _, ok := <-c.sendCh:
		if ok {
			t.Fatal("unregistered client received a broadcast")
		}
	default:
		t.Fatal("sendCh should be closed")
	}
}
