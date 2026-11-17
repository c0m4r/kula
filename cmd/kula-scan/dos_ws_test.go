package main

import (
	"context"
	"io"
	"net"
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

// ---- DOS-CONNFLOOD / DOS-SLOWLORIS -----------------------------------------------

// TestConnFloodWaitsOnceForAll: a target that never reaps idle connections
// costs one -dos-wait, not one per connection.
func TestConnFloodWaitsOnceForAll(t *testing.T) {
	ts := healthTarget(t, nil)
	start := time.Now()
	f, _ := byID(runConnFloodCheck(dosScanner(t, ts.URL, 5*time.Second, 300*time.Millisecond)), "DOS-CONNFLOOD")
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("DOS-CONNFLOOD took %s at a 300ms -dos-wait", elapsed)
	}
	if f.Status != StatusWarn || !strings.Contains(f.Detail, "64/64 idle connections were still open") {
		t.Errorf("DOS-CONNFLOOD against a server that never reaps = %s %q", f.Status, f.Detail)
	}

	reaping := httptest.NewUnstartedServer(ts.Config.Handler)
	reaping.Config.ReadTimeout = 300 * time.Millisecond
	reaping.Start()
	defer reaping.Close()
	wantStatus(t, runConnFloodCheck(dosScanner(t, reaping.URL, 5*time.Second, 3*time.Second)), "DOS-CONNFLOOD", StatusPass)
}

// TestSlowConnectionProbesEndWithTheRun: an interrupt ends the wait for
// reaping at once, whatever -dos-wait is.
func TestSlowConnectionProbesEndWithTheRun(t *testing.T) {
	ts := healthTarget(t, nil)
	for name, run := range map[string]func(*Scanner) []Finding{
		"DOS-CONNFLOOD": runConnFloodCheck,
		"DOS-SLOWLORIS": runSlowlorisCheck,
	} {
		s := dosScanner(t, ts.URL, 5*time.Second, time.Minute)
		ctx, cancel := context.WithCancel(context.Background())
		s.ctx = ctx
		timer := time.AfterFunc(300*time.Millisecond, cancel)
		start := time.Now()
		run(s)
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("%s took %s after the interrupt", name, elapsed)
		}
		timer.Stop()
		cancel()
	}
}

// ---- DOS-HEADERBOMB ---------------------------------------------------------------

// drainListener reads everything each connection sends and never answers.
func drainListener(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
				_, _ = io.Copy(io.Discard, conn)
			}()
		}
	}()
	return "http://" + listener.Addr().String()
}

func TestHeaderBombTellsATimeoutFromARefusal(t *testing.T) {
	f, _ := byID(runHeaderBombCheck(dosScanner(t, drainListener(t), time.Second, time.Second)), "DOS-HEADERBOMB")
	if f.Status != StatusWarn || !strings.Contains(f.Detail, "took a whole 2 MiB header block") {
		t.Errorf("DOS-HEADERBOMB against a server that reads it all and never answers = %s %q, want WARN", f.Status, f.Detail)
	}

	f, _ = byID(runHeaderBombCheck(dosScanner(t, "http://"+silentListener(t).Addr().String(), time.Second, time.Second)), "DOS-HEADERBOMB")
	if f.Status != StatusWarn {
		t.Errorf("DOS-HEADERBOMB against a server that never reads = %s %q, want WARN", f.Status, f.Detail)
	}

	ts := healthTarget(t, nil) // Go's default MaxHeaderBytes: 1 MiB
	wantStatus(t, runHeaderBombCheck(dosScanner(t, ts.URL, 5*time.Second, time.Second)), "DOS-HEADERBOMB", StatusPass)

	endless, _ := endlessHeaderTarget(t)
	wantStatus(t, runHeaderBombCheck(dosScanner(t, endless, 20*time.Second, time.Second)), "DOS-HEADERBOMB", StatusError)
}

// TestDoSProbesWithoutAConnectionAreErrors: probes that open no connection
// report an ERROR that says why, never a PASS.
func TestDoSProbesWithoutAConnectionAreErrors(t *testing.T) {
	s := dosScanner(t, "http://"+closedAddress(t), 5*time.Second, time.Second)
	for name, run := range map[string]func(*Scanner) []Finding{
		"DOS-SLOWLORIS":  runSlowlorisCheck,
		"DOS-HEADERBOMB": runHeaderBombCheck,
		"DOS-CONNFLOOD":  runConnFloodCheck,
	} {
		if f, _ := byID(run(s), name); f.Status != StatusError || !strings.Contains(f.Detail, "refused") {
			t.Errorf("%s against a closed port = %s %q, want an ERROR saying why", name, f.Status, f.Detail)
		}
	}
}
