package main

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
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
		{id: "DOS-SLOWLORIS", category: "dos", aggressive: true, run: runSlowlorisCheck},
		{id: "DOS-HEADERBOMB", category: "dos", aggressive: true, run: runHeaderBombCheck},
		{id: "DOS-CONNFLOOD", category: "dos", aggressive: true, run: runConnFloodCheck},
		{id: "WS-MSGBOMB", category: "ws", aggressive: true, run: runWSMsgBombCheck},
	}
}

// runSlowlorisCheck opens several connections that send a partial request and
// never finish it (a Slowloris). It asserts two things: the server keeps
// answering legitimate requests while the slow connections are held, and it
// reaps the stalled connections within dosWait (i.e. ReadTimeout is enforced).
func runSlowlorisCheck(s *Scanner) []Finding {
	const conns = 12
	reqHead := fmt.Sprintf("GET %s/ HTTP/1.1\r\nHost: %s\r\n", s.base.Path, s.base.Host)

	var opened []net.Conn
	defer func() {
		for _, c := range opened {
			_ = c.Close()
		}
	}()

	for i := 0; i < conns; i++ {
		c, err := s.dialRaw()
		if err != nil {
			continue
		}
		// Send the request line + Host, then the start of a header WITHOUT the
		// terminating CRLFCRLF, so the server keeps waiting for the rest.
		_ = c.SetWriteDeadline(time.Now().Add(s.timeout))
		if _, err := c.Write([]byte(reqHead + "X-Slowloris: keep-")); err != nil {
			_ = c.Close()
			continue
		}
		opened = append(opened, c)
	}
	if len(opened) == 0 {
		return []Finding{finding("DOS-SLOWLORIS", "dos", "Slow-request (Slowloris) reaping", SevInfo, StatusError,
			"could not open any connections to the target.")}
	}

	// Availability: a fresh, well-formed request must still succeed while the
	// slow connections are held open.
	health := s.do(http.MethodGet, "/health", nil, "")
	healthOK := health.err == nil && (health.status == http.StatusOK || health.status == http.StatusNotFound)

	// Wait (in parallel) for the server to drop each stalled connection. A read
	// that returns before our deadline means the server closed it (ReadTimeout);
	// a deadline-exceeded means it is still hanging open.
	start := time.Now()
	var wg sync.WaitGroup
	reaped := make([]bool, len(opened))
	for i, c := range opened {
		wg.Add(1)
		go func(i int, c net.Conn) {
			defer wg.Done()
			_ = c.SetReadDeadline(time.Now().Add(s.dosWait))
			buf := make([]byte, 64)
			_, err := c.Read(buf)
			// Any non-timeout outcome (EOF, reset, or a 408 then close) means the
			// server stopped waiting for the request — the connection was reaped.
			if err == nil {
				reaped[i] = true // server sent something (e.g. 408 Request Timeout) and will close
				return
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				reaped[i] = false // still hanging open at dosWait
				return
			}
			reaped[i] = true // EOF / connection reset
		}(i, c)
	}
	wg.Wait()
	elapsed := time.Since(start).Round(time.Second)

	reapedN := 0
	for _, r := range reaped {
		if r {
			reapedN++
		}
	}

	switch {
	case !healthOK:
		return []Finding{finding("DOS-SLOWLORIS", "dos", "Slow-request (Slowloris) reaping", SevHigh, StatusFail,
			fmt.Sprintf("while %d slow connections were held open, a normal request to /health failed — the server was starved.", len(opened))).
			withEvidence("health status=%d err=%v", health.status, health.err).
			withRemediation("Set http.Server.ReadTimeout so half-open requests are dropped, and front Kula with a reverse proxy.")}
	case reapedN == len(opened):
		return []Finding{finding("DOS-SLOWLORIS", "dos", "Slow-request (Slowloris) reaping", SevMedium, StatusPass,
			"the server reaped all stalled half-open requests and stayed responsive (ReadTimeout enforced).").
			withEvidence("%d/%d slow connections dropped within %s", reapedN, len(opened), elapsed)}
	case reapedN == 0:
		return []Finding{finding("DOS-SLOWLORIS", "dos", "Slow-request (Slowloris) reaping", SevHigh, StatusFail,
			fmt.Sprintf("none of %d half-open requests were dropped within %s; the server holds slow connections indefinitely (Slowloris DoS).", len(opened), s.dosWait)).
			withEvidence("0/%d reaped within %s", len(opened), s.dosWait).
			withRemediation("Set http.Server.ReadTimeout (Kula's default is 30s); ensure it is not disabled.")}
	default:
		return []Finding{finding("DOS-SLOWLORIS", "dos", "Slow-request (Slowloris) reaping", SevMedium, StatusWarn,
			fmt.Sprintf("only %d of %d stalled connections were reaped within %s.", reapedN, len(opened), s.dosWait)).
			withEvidence("%d/%d reaped", reapedN, len(opened)).
			withRemediation("Verify http.Server.ReadTimeout covers the whole request read.")}
	}
}

// runHeaderBombCheck sends a request with a multi-megabyte header value and
// expects the server to reject it (Go's MaxHeaderBytes default is 1 MiB → 431)
// rather than buffering it into memory.
func runHeaderBombCheck(s *Scanner) []Finding {
	conn, err := s.dialRaw()
	if err != nil {
		return []Finding{finding("DOS-HEADERBOMB", "dos", "Oversized request headers rejected", SevInfo, StatusError,
			"could not connect to the target: "+err.Error())}
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(s.timeout))

	// Write the bomb in a goroutine: the server may answer 431 and close the
	// connection before we finish writing 2 MiB, which would error our Write.
	go func() {
		_, _ = fmt.Fprintf(conn, "GET %s/ HTTP/1.1\r\nHost: %s\r\nX-Bomb: ", s.base.Path, s.base.Host)
		chunk := strings.Repeat("A", 64*1024)
		for i := 0; i < 32; i++ { // 2 MiB total, past the 1 MiB default cap
			if _, err := conn.Write([]byte(chunk)); err != nil {
				return
			}
		}
		_, _ = conn.Write([]byte("\r\n\r\n"))
	}()

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		// Server closed/reset the connection without a full response — it refused
		// the oversized headers. That is the safe outcome.
		return []Finding{finding("DOS-HEADERBOMB", "dos", "Oversized request headers rejected", SevMedium, StatusPass,
			"the server refused a 2 MiB header block (connection reset before completion).")}
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusRequestHeaderFieldsTooLarge, http.StatusBadRequest, http.StatusRequestEntityTooLarge:
		return []Finding{finding("DOS-HEADERBOMB", "dos", "Oversized request headers rejected", SevMedium, StatusPass,
			"a 2 MiB header block is rejected (MaxHeaderBytes enforced).").withEvidence("status=%d", resp.StatusCode)}
	case http.StatusOK:
		return []Finding{finding("DOS-HEADERBOMB", "dos", "Oversized request headers rejected", SevHigh, StatusFail,
			"the server accepted a 2 MiB header block; large headers can be used for memory exhaustion.").
			withEvidence("status=200").
			withRemediation("Set http.Server.MaxHeaderBytes (Go defaults to 1 MiB; do not raise it without reason).")}
	default:
		return []Finding{finding("DOS-HEADERBOMB", "dos", "Oversized request headers rejected", SevMedium, StatusPass,
			"the oversized header request was not served normally.").withEvidence("status=%d", resp.StatusCode)}
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

	for i := 0; i < conns; i++ {
		c, err := s.dialRaw()
		if err != nil {
			break
		}
		opened = append(opened, c)
	}
	if len(opened) == 0 {
		return []Finding{finding("DOS-CONNFLOOD", "dos", "Idle-connection flood resilience", SevInfo, StatusError,
			"could not open any connections to the target.")}
	}

	health := s.do(http.MethodGet, "/health", nil, "")
	healthOK := health.err == nil && (health.status == http.StatusOK || health.status == http.StatusNotFound)

	if !healthOK {
		return []Finding{finding("DOS-CONNFLOOD", "dos", "Idle-connection flood resilience", SevHigh, StatusFail,
			fmt.Sprintf("a normal request failed while %d idle connections were held open.", len(opened))).
			withEvidence("opened=%d health status=%d err=%v", len(opened), health.status, health.err).
			withRemediation("Front Kula with a reverse proxy that bounds concurrent connections, and keep read/idle timeouts enabled.")}
	}

	// Confirm the idle connections (no request sent) are reaped within dosWait.
	reaped := 0
	for _, c := range opened {
		_ = c.SetReadDeadline(time.Now().Add(s.dosWait))
		buf := make([]byte, 1)
		_, err := c.Read(buf)
		if err == nil {
			reaped++
			continue
		}
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			continue
		}
		reaped++
	}

	if reaped == len(opened) {
		return []Finding{finding("DOS-CONNFLOOD", "dos", "Idle-connection flood resilience", SevLow, StatusPass,
			"the server stayed responsive under an idle-connection flood and reaped the idle sockets (timeouts enforced).").
			withEvidence("%d idle connections opened, all reaped, /health OK", len(opened))}
	}
	return []Finding{finding("DOS-CONNFLOOD", "dos", "Idle-connection flood resilience", SevMedium, StatusWarn,
		fmt.Sprintf("the server stayed responsive, but %d/%d idle connections were still open after %s.", len(opened)-reaped, len(opened), s.dosWait)).
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
			return []Finding{finding("WS-MSGBOMB", "ws", "WebSocket message size limit", SevInfo, StatusSkip,
				"auth is enabled and no session is available; supply -username/-password to test the WebSocket read limit.")}
		}
		hdr = http.Header{}
		hdr.Set("Cookie", "kula_session="+s.session)
	} else {
		hdr = http.Header{}
	}
	hdr.Set("Origin", s.base.Scheme+"://"+s.base.Host)

	conn, _, err := s.wsDial("/ws", hdr)
	if err != nil || conn == nil {
		return []Finding{finding("WS-MSGBOMB", "ws", "WebSocket message size limit", SevInfo, StatusSkip,
			"could not establish a WebSocket connection to test the read limit.")}
	}
	defer func() { _ = conn.Close() }()

	// Send a message well past the 4 KiB server read limit.
	_ = conn.SetWriteDeadline(time.Now().Add(s.timeout))
	bomb := make([]byte, 64*1024)
	for i := range bomb {
		bomb[i] = 'A'
	}
	if err := conn.WriteMessage(websocket.TextMessage, bomb); err != nil {
		// Write failed because the server already tore the connection down — the
		// limit is enforced.
		return []Finding{finding("WS-MSGBOMB", "ws", "WebSocket message size limit", SevLow, StatusPass,
			"the server dropped the connection when an oversized message was sent (read limit enforced).")}
	}

	// The server normally streams metric frames; read past those until it closes
	// the connection (read-limit exceeded) or we hit the deadline.
	deadline := time.Now().Add(s.timeout)
	_ = conn.SetReadDeadline(deadline)
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return []Finding{finding("WS-MSGBOMB", "ws", "WebSocket message size limit", SevLow, StatusPass,
				"the server closed the connection after an oversized client message (SetReadLimit enforced).").
				withEvidence("close: %v", err)}
		}
		if time.Now().After(deadline) {
			return []Finding{finding("WS-MSGBOMB", "ws", "WebSocket message size limit", SevMedium, StatusWarn,
				"the connection stayed open after a 64 KiB client message; the read limit may not be enforced.").
				withRemediation("Call conn.SetReadLimit on the WebSocket to bound inbound message size.")}
		}
	}
}
