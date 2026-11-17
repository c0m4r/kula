package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

// Denial-of-service / resource-exhaustion probes. Every check here is
// disruptive (holds many sockets open, sends multi-megabyte requests, floods
// connections) so they are all aggressive and only run with -aggressive. They
// assert that Kula's protocol-level safeguards actually fire:
//
//   - slow-request reaping        → http.Server.ReadTimeout (30s)
//   - oversized request headers   → http.Server MaxHeaderBytes (Go default 1 MiB)
//   - idle-connection resilience  → ReadTimeout/IdleTimeout reaping
//   - oversized WebSocket message → conn.SetReadLimit(4096)
//
// The slow / idle probes wait up to Scanner.dosWait for the server to drop a
// stalled connection; raise -dos-wait if the target runs longer read timeouts.

func dosChecks() []check {
	return []check{
		{ids: []string{"DOS-SLOWLORIS"}, run: runSlowlorisCheck, hint: dosWaitHint},
		{ids: []string{"DOS-HEADERBOMB"}, run: runHeaderBombCheck},
		{ids: []string{"DOS-CONNFLOOD"}, run: runConnFloodCheck, hint: dosWaitHint},
		{ids: []string{"WS-MSGBOMB"}, run: runWSMsgBombCheck},
	}
}

// dosWaitHint tells the progress line that a probe waits out -dos-wait.
func dosWaitHint(s *Scanner) string {
	return "waits up to -dos-wait " + s.dosWait.String()
}

// isReadTimeout reports whether err is a deadline the scanner set expiring:
// the connection is still open.
func isReadTimeout(err error) bool {
	var ne net.Error
	return errors.Is(err, os.ErrDeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())
}

// isHangUp reports whether err, from a read or write on a connection, means
// the target ended the connection: an end of file, a reset or an abort.
func isHangUp(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.EPIPE)
}

// awaitReaping waits, against one deadline wait from now, for the target to
// drop each of conns, and returns how many it dropped. A read that returns
// data (a 408 before the close) or that ends in anything but the deadline
// means the target stopped waiting on that connection. An interrupt closes
// the connections (dialRaw), which ends the wait at once.
func awaitReaping(conns []net.Conn, wait time.Duration) int {
	deadline := time.Now().Add(wait)
	var reaped atomic.Int64
	var wg sync.WaitGroup
	for _, c := range conns {
		wg.Add(1)
		go func(c net.Conn) {
			defer wg.Done()
			_ = c.SetReadDeadline(deadline)
			if _, err := c.Read(make([]byte, 64)); !isReadTimeout(err) {
				reaped.Add(1)
			}
		}(c)
	}
	wg.Wait()
	return int(reaped.Load())
}

// proxyHint is added to the detail of a slow-connection probe whose
// connections outlived -dos-wait when the target's response h names a server
// or a proxy. Kula sends neither header, so a reverse proxy likely answers,
// and its own header and idle timeouts, not Kula's, decide when a stalled
// connection is dropped.
func (s *Scanner) proxyHint(h http.Header) string {
	var named []string
	for _, name := range []string{"Server", "Via"} {
		if v := h.Get(name); v != "" {
			named = append(named, name+": "+truncate(v, 60))
		}
	}
	if len(named) == 0 {
		return ""
	}
	hint := fmt.Sprintf(" The responses carry %s, so a reverse proxy may sit in front of Kula: its own header and idle timeouts then apply (nginx waits 60s by default), and a longer -dos-wait shows whether it drops the connections",
		strings.Join(named, " and "))
	if s.dosWait < proxyDoSWait {
		hint += fmt.Sprintf(" (e.g. -dos-wait %.0fs)", proxyDoSWait.Seconds())
	}
	return hint + "."
}

// proxyDoSWait is the -dos-wait proxyHint suggests: past nginx's default 60s
// client_header_timeout.
const proxyDoSWait = 75 * time.Second

// runSlowlorisCheck opens several connections that send a partial request and
// never finish it (a Slowloris). It asserts two things: the server keeps
// answering legitimate requests while the slow connections are held, and it
// reaps the stalled connections within dosWait (i.e. ReadTimeout is enforced).
func runSlowlorisCheck(s *Scanner) []Finding {
	const conns = 12
	reqHead := fmt.Sprintf("GET %s/ HTTP/1.1\r\nHost: %s\r\n%s", s.base.Path, s.base.Host, s.rawHeaderLines(nil))

	var opened []net.Conn
	defer func() {
		for _, c := range opened {
			_ = c.Close()
		}
	}()

	var firstErr error
	for i := 0; i < conns; i++ {
		c, err := s.dialRaw()
		if err == nil {
			// Send the request line + Host, then the start of a header WITHOUT
			// the terminating CRLFCRLF, so the server keeps waiting for the rest.
			_ = c.SetWriteDeadline(time.Now().Add(s.timeout))
			if _, err = c.Write([]byte(reqHead + "X-Slowloris: keep-")); err != nil {
				_ = c.Close()
			}
		}
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		opened = append(opened, c)
	}
	if len(opened) == 0 {
		return []Finding{finding("DOS-SLOWLORIS", SevInfo, StatusError,
			fmt.Sprintf("could not open any connections to the target: %v", firstErr))}
	}

	// Availability: a fresh, well-formed request must still succeed while the
	// slow connections are held open.
	health := s.do(http.MethodGet, "/health", nil, "")
	healthOK := health.err == nil && (health.status == http.StatusOK || health.status == http.StatusNotFound)

	// Wait for the server to drop the stalled connections, all against one
	// deadline: a read that returns before it means the server closed the
	// connection (ReadTimeout); one that reaches it, that it still hangs open.
	start := time.Now()
	reapedN := awaitReaping(opened, s.dosWait)
	elapsed := time.Since(start).Round(time.Second)

	switch {
	case !healthOK:
		return []Finding{finding("DOS-SLOWLORIS", SevHigh, StatusFail,
			fmt.Sprintf("while %d slow connections were held open, a normal request to /health failed — the server was starved.", len(opened))).
			withEvidence("health status=%d err=%v", health.status, health.err).
			withRemediation("Set http.Server.ReadTimeout so half-open requests are dropped, and front Kula with a reverse proxy.")}
	case reapedN == len(opened):
		return []Finding{finding("DOS-SLOWLORIS", SevMedium, StatusPass,
			"the server reaped all stalled half-open requests and stayed responsive (ReadTimeout enforced).").
			withEvidence("%d/%d slow connections dropped within %s", reapedN, len(opened), elapsed)}
	case reapedN == 0:
		return []Finding{finding("DOS-SLOWLORIS", SevHigh, StatusFail,
			fmt.Sprintf("none of %d half-open requests were dropped within %s; the server holds slow connections indefinitely (Slowloris DoS).", len(opened), s.dosWait)+s.proxyHint(health.header)).
			withEvidence("0/%d reaped within %s", len(opened), s.dosWait).
			withRemediation("Set http.Server.ReadTimeout (Kula's default is 30s); ensure it is not disabled.")}
	default:
		return []Finding{finding("DOS-SLOWLORIS", SevMedium, StatusWarn,
			fmt.Sprintf("only %d of %d stalled connections were reaped within %s.", reapedN, len(opened), s.dosWait)+s.proxyHint(health.header)).
			withEvidence("%d/%d reaped", reapedN, len(opened)).
			withRemediation("Verify http.Server.ReadTimeout covers the whole request read.")}
	}
}

// runHeaderBombCheck sends a request with a multi-megabyte header value and
// expects the server to reject it (Go's MaxHeaderBytes default is 1 MiB → 431)
// rather than buffering it into memory. A refusal ends the connection or
// answers it; a server that takes the whole block and says nothing within
// -timeout, or stops reading it without closing, has refused nothing.
func runHeaderBombCheck(s *Scanner) []Finding {
	conn, err := s.dialRaw()
	if err != nil {
		return []Finding{finding("DOS-HEADERBOMB", SevInfo, StatusError,
			"could not connect to the target: "+err.Error())}
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(s.timeout))

	// Write the bomb in a goroutine: the server may answer 431 and close the
	// connection before we finish writing 2 MiB, which would error our Write.
	// written reports whether the whole block went out.
	written := make(chan bool, 1)
	go func() {
		_, _ = fmt.Fprintf(conn, "GET %s/ HTTP/1.1\r\nHost: %s\r\n%sX-Bomb: ", s.base.Path, s.base.Host, s.rawHeaderLines(nil))
		chunk := strings.Repeat("A", 64*1024)
		for i := 0; i < 32; i++ { // 2 MiB total, past the 1 MiB default cap
			if _, err := conn.Write([]byte(chunk)); err != nil {
				written <- false
				return
			}
		}
		_, err := conn.Write([]byte("\r\n\r\n"))
		written <- err == nil
	}()

	resp, err := readResponse(conn)
	if err != nil {
		return []Finding{headerBombReadFinding(s, err, written)}
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusRequestHeaderFieldsTooLarge, http.StatusBadRequest, http.StatusRequestEntityTooLarge:
		return []Finding{finding("DOS-HEADERBOMB", SevMedium, StatusPass,
			"a 2 MiB header block is rejected (MaxHeaderBytes enforced).").withEvidence("status=%d", resp.StatusCode)}
	case http.StatusOK:
		return []Finding{finding("DOS-HEADERBOMB", SevHigh, StatusFail,
			"the server accepted a 2 MiB header block; large headers can be used for memory exhaustion.").
			withEvidence("status=200").
			withRemediation("Set http.Server.MaxHeaderBytes (Go defaults to 1 MiB; do not raise it without reason).")}
	default:
		return []Finding{finding("DOS-HEADERBOMB", SevMedium, StatusPass,
			"the oversized header request was not served normally.").withEvidence("status=%d", resp.StatusCode)}
	}
}

// headerBombReadFinding judges a header bomb that drew no response: err is
// how reading it ended, and written reports, once the writer is done, whether
// the whole block went out.
func headerBombReadFinding(s *Scanner, err error, written <-chan bool) Finding {
	var sent bool
	select {
	case sent = <-written:
	default: // still writing: the server stopped reading the block
	}
	switch {
	case errors.Is(err, errResponseTooLarge):
		// readResponse caps what the scanner reads; Kula's 431 is far smaller.
		return finding("DOS-HEADERBOMB", SevInfo, StatusError,
			"the server answered with response headers over 2 MiB, which the scanner does not read; this is not Kula's answer.")
	case isHangUp(err):
		// Closed or reset without a full response: it refused the oversized
		// headers. That is the safe outcome.
		how := "while it was being sent"
		if sent {
			how = "after it was sent, without an answer"
		}
		return finding("DOS-HEADERBOMB", SevMedium, StatusPass,
			"the server refused a 2 MiB header block: it closed the connection "+how+".").
			withEvidence("%v", err)
	case isReadTimeout(err) && sent:
		return finding("DOS-HEADERBOMB", SevMedium, StatusWarn,
			fmt.Sprintf("the server took a whole 2 MiB header block and neither answered nor closed the connection within %s; it may be buffering oversized headers.", s.timeout)).
			withEvidence("%v", err).
			withRemediation("Set http.Server.MaxHeaderBytes (Go defaults to 1 MiB) so an oversized header block is answered with 431.")
	case isReadTimeout(err):
		return finding("DOS-HEADERBOMB", SevMedium, StatusWarn,
			fmt.Sprintf("the server stopped reading a 2 MiB header block but neither refused it nor closed the connection within %s; the connection is held open.", s.timeout)).
			withEvidence("%v", err).
			withRemediation("Set http.Server.MaxHeaderBytes and ReadTimeout so an oversized header block is refused and its connection closed.")
	default:
		return finding("DOS-HEADERBOMB", SevInfo, StatusError,
			"reading the answer to a 2 MiB header block failed: "+err.Error())
	}
}

// runConnFloodCheck opens many concurrent idle connections (no request sent) and
// verifies the server stays responsive to legitimate traffic, then that the idle
// sockets are reaped within dosWait. Go serves each connection on its own
// goroutine, so the real protection against pile-up is the read/idle timeout.
func runConnFloodCheck(s *Scanner) []Finding {
	const conns = 64
	var opened []net.Conn
	defer func() {
		for _, c := range opened {
			_ = c.Close()
		}
	}()

	var dialErr error
	for i := 0; i < conns; i++ {
		c, err := s.dialRaw()
		if err != nil {
			dialErr = err
			break
		}
		opened = append(opened, c)
	}
	if len(opened) == 0 {
		return []Finding{finding("DOS-CONNFLOOD", SevInfo, StatusError,
			fmt.Sprintf("could not open any connections to the target: %v", dialErr))}
	}

	health := s.do(http.MethodGet, "/health", nil, "")
	healthOK := health.err == nil && (health.status == http.StatusOK || health.status == http.StatusNotFound)

	if !healthOK {
		return []Finding{finding("DOS-CONNFLOOD", SevHigh, StatusFail,
			fmt.Sprintf("a normal request failed while %d idle connections were held open.", len(opened))).
			withEvidence("opened=%d health status=%d err=%v", len(opened), health.status, health.err).
			withRemediation("Front Kula with a reverse proxy that bounds concurrent connections, and keep read/idle timeouts enabled.")}
	}

	// Confirm the idle connections (no request sent) are reaped within
	// dosWait, all waited for at once: the check takes about dosWait however
	// many the target keeps.
	reaped := awaitReaping(opened, s.dosWait)

	if reaped == len(opened) {
		return []Finding{finding("DOS-CONNFLOOD", SevLow, StatusPass,
			"the server stayed responsive under an idle-connection flood and reaped the idle sockets (timeouts enforced).").
			withEvidence("%d idle connections opened, all reaped, /health OK", len(opened))}
	}
	return []Finding{finding("DOS-CONNFLOOD", SevMedium, StatusWarn,
		fmt.Sprintf("the server stayed responsive, but %d/%d idle connections were still open after %s.", len(opened)-reaped, len(opened), s.dosWait)+s.proxyHint(health.header)).
		withEvidence("%d/%d idle sockets reaped", reaped, len(opened)).
		withRemediation("Ensure http.Server.IdleTimeout/ReadTimeout are set so idle sockets cannot accumulate.")}
}

// runWSMsgBombCheck opens an authenticated, same-origin WebSocket and sends a
// message larger than the server's read limit (4096 bytes). The server must
// drop the connection rather than buffer an unbounded client message.
func runWSMsgBombCheck(s *Scanner) []Finding {
	var hdr http.Header
	if s.authEnabled {
		if !s.ensureSession() {
			return []Finding{finding("WS-MSGBOMB", SevInfo, StatusSkip,
				"auth is enabled and no session is available to test the WebSocket read limit: "+s.noSessionReason()+".")}
		}
		hdr = http.Header{}
		hdr.Set("Cookie", "kula_session="+s.session)
	} else {
		hdr = http.Header{}
	}
	hdr.Set("Origin", s.base.Scheme+"://"+s.base.Host)

	conn, resp, err := s.wsDial("/ws", hdr)
	if err != nil || conn == nil {
		if resp != nil {
			return []Finding{finding("WS-MSGBOMB", SevInfo, StatusError,
				"the WebSocket upgrade was refused, so no oversized message could be sent.").
				withEvidence("status=%s", statusOf(resp))}
		}
		return []Finding{finding("WS-MSGBOMB", SevInfo, StatusError,
			fmt.Sprintf("could not open a WebSocket connection to test the read limit: %v", err))}
	}
	defer func() { _ = conn.Close() }()

	// Send a message well past the 4 KiB server read limit.
	_ = conn.SetWriteDeadline(time.Now().Add(s.timeout))
	bomb := make([]byte, 64*1024)
	for i := range bomb {
		bomb[i] = 'A'
	}
	if err := conn.WriteMessage(websocket.TextMessage, bomb); err != nil {
		switch {
		case isHangUp(err):
			// The server tore the connection down while the message was
			// still arriving: the limit is enforced.
			return []Finding{finding("WS-MSGBOMB", SevLow, StatusPass,
				"the server dropped the connection when an oversized message was sent (read limit enforced).").
				withEvidence("write: %v", err)}
		case isReadTimeout(err):
			return []Finding{finding("WS-MSGBOMB", SevMedium, StatusWarn,
				fmt.Sprintf("the server stopped reading a 64 KiB client message but kept the connection open for %s; the read limit may not be enforced.", s.timeout)).
				withEvidence("write: %v", err).
				withRemediation("Call conn.SetReadLimit on the WebSocket to bound inbound message size.")}
		default:
			return []Finding{finding("WS-MSGBOMB", SevInfo, StatusError,
				"sending the oversized message failed, which says nothing of the read limit: "+err.Error())}
		}
	}

	// The server normally streams metric frames; read past those until it
	// closes the connection (read limit exceeded) or the deadline passes with
	// the connection still open.
	_ = conn.SetReadDeadline(time.Now().Add(s.timeout))
	for {
		_, _, err := conn.ReadMessage()
		if err == nil {
			continue
		}
		var closeErr *websocket.CloseError
		switch {
		case errors.Is(err, websocket.ErrReadLimit):
			// wsDial caps what the scanner reads; Kula's frames are far smaller.
			return []Finding{finding("WS-MSGBOMB", SevInfo, StatusError,
				"the server sent a WebSocket message over 1 MiB, which the scanner does not read; this is not Kula's live stream.")}
		case isReadTimeout(err):
			return []Finding{finding("WS-MSGBOMB", SevMedium, StatusWarn,
				fmt.Sprintf("the connection was still open %s after a 64 KiB client message; the read limit may not be enforced.", s.timeout)).
				withEvidence("%v", err).
				withRemediation("Call conn.SetReadLimit on the WebSocket to bound inbound message size.")}
		case errors.As(err, &closeErr), isHangUp(err):
			return []Finding{finding("WS-MSGBOMB", SevLow, StatusPass,
				"the server closed the connection after an oversized client message (SetReadLimit enforced).").
				withEvidence("close: %v", err)}
		default:
			return []Finding{finding("WS-MSGBOMB", SevInfo, StatusError,
				"reading from the WebSocket after the oversized message failed, which says nothing of the read limit: "+err.Error())}
		}
	}
}
