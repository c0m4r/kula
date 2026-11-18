package main

import (
	"bufio"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// silentFor serves raw requests one per connection: for a request line
// containing a key of answers it writes that answer and closes, for one
// containing a string in silent it says nothing and keeps the connection
// open, and any other it closes without an answer, as a server rejecting it
// at the protocol layer does.
func silentFor(t *testing.T, answers map[string]string, silent ...string) string {
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
				line, err := bufio.NewReader(conn).ReadString('\n')
				if err != nil {
					return
				}
				for _, s := range silent {
					if strings.Contains(line, s) {
						_, _ = conn.Read(make([]byte, 1)) // until the scanner gives up
						return
					}
				}
				for key, answer := range answers {
					if strings.Contains(line, key) {
						_, _ = conn.Write([]byte(answer))
						return
					}
				}
			}()
		}
	}()
	return "http://" + listener.Addr().String()
}

const styleAnswer = "HTTP/1.1 200 OK\r\nContent-Type: text/css\r\nContent-Length: 4\r\nConnection: close\r\n\r\nbody"

// TestRawSilenceIsNoAnswer: a raw request the target takes and neither
// answers nor closes within -timeout is no answer, which proves nothing: the
// traversal and framing probes report an ERROR, not a refusal at the
// protocol layer that passes them. A close without a response stays a
// refusal.
func TestRawSilenceIsNoAnswer(t *testing.T) {
	s := dosScanner(t, drainListener(t), 300*time.Millisecond, time.Second)
	if status, _, err := s.rawExchange("GET / HTTP/1.1\r\nHost: x\r\n\r\n"); !errors.Is(err, errNoAnswer) || status != 0 {
		t.Errorf("rawExchange to a target that never answers: status %d, error %v; want errNoAnswer", status, err)
	}

	traversal := runTraversalChecks(s)
	for _, id := range []string{"TRAV-000", "TRAV-001", "TRAV-002"} {
		wantStatus(t, traversal, id, StatusError)
	}
	if f, _ := byID(traversal, "TRAV-000"); !strings.Contains(f.Detail, "GET /style.css got no answer within -timeout (300ms)") {
		t.Errorf("TRAV-000 does not say the target was silent: %q", f.Detail)
	}

	s.fuzz = true
	f, _ := byID(runFuzzSmuggle(s), "FUZZ-SMUGGLE")
	if f.Status != StatusError || !strings.Contains(f.Detail, "3 of 3 ambiguously framed requests to /api/login got no answer within -timeout") {
		t.Errorf("FUZZ-SMUGGLE against a target that never answers = %s: %s; want an ERROR saying so", f.Status, f.Detail)
	}

	// The control is served, the payloads are refused by a close, but for
	// one the target keeps to itself; the directories are refused too.
	s = dosScanner(t, silentFor(t, map[string]string{"/style.css ": styleAnswer}, "go.mod"), 300*time.Millisecond, time.Second)
	traversal = runTraversalChecks(s)
	wantStatus(t, traversal, "TRAV-002", StatusPass)
	if _, reported := byID(traversal, "TRAV-000"); reported {
		t.Errorf("TRAV-000 reported with the control served: %+v", traversal)
	}
	f, _ = byID(traversal, "TRAV-001")
	if f.Status != StatusError || !strings.Contains(f.Detail, "2 of 8 traversal payloads got no answer within -timeout") {
		t.Errorf("TRAV-001 with two payloads unanswered = %s: %s; want an ERROR saying so", f.Status, f.Detail)
	}

	// One payload is not delivered at all, another is not answered.
	var lost undelivered
	lost.add(errors.New("connection refused"))
	lost.add(errors.Join(errNoAnswer))
	if got, want := lost.detail(8, "payloads"), "2 of 8 payloads drew nothing: 1 could not be delivered over a raw socket (connection refused), and 1 got no answer"; got != want {
		t.Errorf("detail = %q, want %q", got, want)
	}
}
