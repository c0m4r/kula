package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// These tests cover the DoS, WebSocket and rate-limit probes against small
// targets of their own, each missing or keeping one safeguard.

// healthTarget answers /health as Kula does, with header added to every
// response, and 404 elsewhere. It sets no server timeouts: it never reaps an
// idle or half-open connection.
func healthTarget(t *testing.T, header http.Header) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for name, values := range header {
			w.Header()[name] = values
		}
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("kula is healthy"))
	}))
	t.Cleanup(ts.Close)
	return ts
}

// dosScanner returns a scanner for target with the given -timeout and
// -dos-wait.
func dosScanner(t *testing.T, target string, timeout, dosWait time.Duration) *Scanner {
	t.Helper()
	s, err := NewScanner(Options{Target: target, Timeout: timeout, DoSWait: dosWait, Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// ---- WS-MSGBOMB ---------------------------------------------------------------

// streamingWSTarget serves /ws, streaming a small frame every 20ms until the
// connection ends. readLimit bounds a client message as Kula's
// SetReadLimit(4096) does; 0 sets no limit.
func streamingWSTarget(t *testing.T, readLimit int64) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		conn.SetReadLimit(readLimit)
		ended := make(chan struct{})
		go func() {
			defer close(ended)
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ended:
				return
			case <-ticker.C:
				_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
				if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"cpu":1}`)); err != nil {
					return
				}
			}
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

// TestWSMessageBombTellsATimeoutFromAClose: a server with no read limit that
// keeps streaming is still open at the deadline, which is no close.
func TestWSMessageBombTellsATimeoutFromAClose(t *testing.T) {
	unlimited := streamingWSTarget(t, 0)
	f, _ := byID(runWSMsgBombCheck(dosScanner(t, unlimited.URL, time.Second, time.Second)), "WS-MSGBOMB")
	if f.Status != StatusWarn || !strings.Contains(f.Detail, "still open") {
		t.Errorf("WS-MSGBOMB against a server with no read limit = %s %q | %q, want WARN", f.Status, f.Detail, f.Evidence)
	}

	// gorilla answers a message past the limit with a 1009 close, and the
	// scanner may still be writing it when the connection goes.
	limited := streamingWSTarget(t, 4096)
	f, _ = byID(runWSMsgBombCheck(dosScanner(t, limited.URL, 5*time.Second, time.Second)), "WS-MSGBOMB")
	if f.Status != StatusPass || !strings.Contains(f.Evidence, "1009") && !strings.Contains(f.Evidence, "write:") {
		t.Errorf("WS-MSGBOMB against SetReadLimit(4096) = %s %q | %q, want PASS on a close", f.Status, f.Detail, f.Evidence)
	}
}

func TestWSMessageBombWithoutAConnectionIsAnError(t *testing.T) {
	wantStatus(t, runWSMsgBombCheck(dosScanner(t, "http://"+closedAddress(t), 5*time.Second, time.Second)), "WS-MSGBOMB", StatusError)

	refused := healthTarget(t, nil) // answers /ws with 404
	f, _ := byID(runWSMsgBombCheck(dosScanner(t, refused.URL, 5*time.Second, time.Second)), "WS-MSGBOMB")
	if f.Status != StatusError || !strings.Contains(f.Evidence, "404") {
		t.Errorf("WS-MSGBOMB with the upgrade refused = %s %q | %q, want ERROR", f.Status, f.Detail, f.Evidence)
	}
}
