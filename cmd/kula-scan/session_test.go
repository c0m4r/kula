package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"
)

// loginLedger records the passwords a scan tries per username and the
// logouts it sends. Kula counts every login, right or wrong, against a limit
// of five per account and address in five minutes.
type loginLedger struct {
	mu       sync.Mutex
	attempts map[string][]string
	logouts  []http.Header
}

func (l *loginLedger) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/login" && r.Method == http.MethodPost:
			body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			r.Body = io.NopCloser(bytes.NewReader(body))
			var creds struct{ Username, Password string }
			if json.Unmarshal(body, &creds) == nil {
				l.mu.Lock()
				l.attempts[creds.Username] = append(l.attempts[creds.Username], creds.Password)
				l.mu.Unlock()
			}
		case r.URL.Path == "/api/logout":
			l.mu.Lock()
			l.logouts = append(l.logouts, r.Header.Clone())
			l.mu.Unlock()
		}
		next.ServeHTTP(w, r)
	})
}

func scanWithLedger(t *testing.T, aggressive bool, only map[string]bool) ([]Finding, *loginLedger) {
	t.Helper()
	ledger := &loginLedger{attempts: map[string][]string{}}
	ts := httptest.NewServer(ledger.wrap(secureMock()))
	t.Cleanup(ts.Close)
	s, err := NewScanner(Options{
		Target: ts.URL, Username: mockUser, Password: mockPass,
		Timeout: 5 * time.Second, Aggressive: aggressive, Seed: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s.Run(only), ledger
}

func TestDefaultRunLogsInOnceAndOut(t *testing.T) {
	findings, ledger := scanWithLedger(t, false, nil)

	wantStatus(t, findings, "AUTH-005", StatusPass)
	wantStatus(t, findings, "AUTH-006", StatusSkip)
	if got := ledger.attempts[mockUser]; len(got) != 1 || got[0] != mockPass {
		t.Errorf("a default run tried the passwords %q for %s, want one login with the right one", got, mockUser)
	}

	if len(ledger.logouts) == 0 {
		t.Fatal("the run did not log out")
	}
	last := ledger.logouts[len(ledger.logouts)-1]
	if last.Get("Cookie") != "kula_session="+mockToken || last.Get("X-CSRF-Token") != "csrftok" || last.Get("Origin") == "" {
		t.Errorf("the closing logout carries Cookie %q, X-CSRF-Token %q, Origin %q; want the session, its token and the origin",
			last.Get("Cookie"), last.Get("X-CSRF-Token"), last.Get("Origin"))
	}
}

func TestPasswordComesFromTheEnvironment(t *testing.T) {
	ledger := &loginLedger{attempts: map[string][]string{}}
	ts := httptest.NewServer(ledger.wrap(secureMock()))
	defer ts.Close()
	t.Setenv(passwordEnv, mockPass)

	// run prints the report to standard output.
	stdout := os.Stdout
	output, err := os.CreateTemp(t.TempDir(), "report")
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = output
	code := run([]string{"-username", mockUser, "-only", "auth", "-json", ts.URL})
	os.Stdout = stdout
	_ = output.Close()

	if code != 0 {
		t.Fatalf("run exited %d", code)
	}
	if got := ledger.attempts[mockUser]; len(got) != 1 || got[0] != mockPass {
		t.Fatalf("logged in with %q, want the password from %s", got, passwordEnv)
	}

	if got := resolvePassword("from-flag"); got != "from-flag" {
		t.Errorf("an explicit -password gave way to the environment: %q", got)
	}
	t.Setenv(passwordEnv, "")
	if got := resolvePassword(""); got != "" {
		t.Errorf("no flag and no variable gave the password %q", got)
	}
}

func TestAggressiveRunProbesUsernameEnumeration(t *testing.T) {
	findings, ledger := scanWithLedger(t, true, map[string]bool{"auth": true})

	wantStatus(t, findings, "AUTH-006", StatusPass)
	if got := ledger.attempts[mockUser]; len(got) != 2 {
		t.Errorf("an aggressive auth run tried %q for %s, want the login and one wrong password", got, mockUser)
	}
}
