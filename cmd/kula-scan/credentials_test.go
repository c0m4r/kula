package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestHalfTheCredentialsIsAUsageError: a -username whose password is
// missing, as a CI secret that is not set leaves it, stops the scan before
// it starts instead of running it without a session; so does a -password
// without a -username. A password in the environment without a -username is
// a scan without a session, as the variable may be set for other scans.
func TestHalfTheCredentialsIsAUsageError(t *testing.T) {
	var requests atomic.Int64
	mock := secureMock()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		mock.ServeHTTP(w, r)
	}))
	defer ts.Close()

	for _, c := range []struct {
		name string
		env  string
		args []string
		want string
	}{
		{"-username, no password", "", []string{"-username", mockUser}, "-username has no password: pass it in " + passwordEnv},
		{"-password, no -username", "", []string{"-password", mockPass}, "-password has no -username"},
		{"-password, no -username, the variable set", mockPass, []string{"-password", mockPass}, "-password has no -username"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(passwordEnv, c.env)
			requests.Store(0)
			code, stdout, stderr := runCaptured(t, append(c.args, "-only", "auth", ts.URL)...)
			if code != exitUsage || stdout != "" || !strings.Contains(stderr, "error: "+c.want) {
				t.Errorf("exit %d, want %d with no report and %q on stderr\nstdout:\n%s\nstderr:\n%s",
					code, exitUsage, c.want, stdout, stderr)
			}
			if n := requests.Load(); n != 0 {
				t.Errorf("the target got %d requests from a scan that should not start", n)
			}
		})
	}

	// Both halves, from the flag or the environment, or neither, start it.
	for _, c := range []struct {
		name string
		env  string
		args []string
	}{
		{"-username and -password", "", []string{"-username", mockUser, "-password", mockPass}},
		{"-username and the variable", mockPass, []string{"-username", mockUser}},
		{"the variable alone", mockPass, nil},
		{"neither", "", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(passwordEnv, c.env)
			code, _, stderr := runCaptured(t, append(c.args, "-only", "auth", ts.URL)...)
			if code != exitClean {
				t.Errorf("exit %d, want %d; stderr:\n%s", code, exitClean, stderr)
			}
		})
	}
}
