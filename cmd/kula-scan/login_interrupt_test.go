package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestInterruptDuringLoginLogsTheSessionOut: an interrupt that arrives while
// the run's login is in flight does not abandon it. Kula opens the session
// whether or not the client waits for the cookie, so the scanner reads the
// answer through, within -timeout, and the closing logout ends the session.
// Once interrupted, it sends no login at all.
func TestInterruptDuringLoginLogsTheSessionOut(t *testing.T) {
	var logins, created atomic.Int64
	var loggedOut atomic.Value
	started := make(chan struct{}, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte("kula is healthy"))
		case "/api/auth/status":
			writeJSONResp(w, map[string]any{"auth_required": true, "authenticated": false})
		case "/api/login":
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			logins.Add(1)
			select {
			case started <- struct{}{}:
			default:
			}
			// Argon2id takes its time, and Kula's CreateSession does not
			// look at the request's context.
			time.Sleep(300 * time.Millisecond)
			created.Add(1)
			http.SetCookie(w, &http.Cookie{Name: "kula_session", Value: "tok"})
			writeJSONResp(w, map[string]string{"csrf_token": "c"})
		case "/api/logout":
			loggedOut.Store(r.Header.Get("Cookie"))
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer ts.Close()

	s, err := NewScanner(Options{Target: ts.URL, Username: "admin", Password: "pw", Timeout: 2 * time.Second, Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-started
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	s.Run(ctx, nil)

	if !s.interrupted {
		t.Fatal("the run was not interrupted")
	}
	// A scanner that hung up on the login has returned before Kula is done.
	for deadline := time.Now().Add(2 * time.Second); created.Load() == 0 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if created.Load() != 1 {
		t.Fatalf("%d sessions created on the target, want the one login", created.Load())
	}
	if got, _ := loggedOut.Load().(string); got != "kula_session=tok" {
		t.Errorf("the session opened while the interrupt arrived was not logged out (logout Cookie %q)", got)
	}

	// After the interrupt, no login is sent.
	s, err = NewScanner(Options{Target: ts.URL, Username: "admin", Password: "pw", Timeout: 2 * time.Second, Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	s.ctx = ctx
	before := logins.Load()
	if s.ensureSession() || logins.Load() != before {
		t.Errorf("an interrupted run logged in: session %v, %d more logins", s.loggedIn, logins.Load()-before)
	}
}
