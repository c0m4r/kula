package main

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// endlessBytes is what the hostile targets below try to send: far past every
// bound the scanner keeps. A scanner that stops reading at its bound lets the
// target write that bound plus what the loopback socket buffers hold.
const (
	endlessBytes  = 64 << 20
	writtenBefore = 32 << 20 // the target wrote less than this before the scanner hung up
)

// endlessHeaderTarget answers each connection with a response whose one
// header never ends, and reports how many bytes it managed to write.
func endlessHeaderTarget(t *testing.T) (string, <-chan int) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	written := make(chan int, 4)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
				// The request line is enough; the header bomb probe is still
				// writing the rest of its request.
				if _, err := bufio.NewReader(conn).ReadString('\n'); err != nil {
					return
				}
				total, err := io.WriteString(conn, "HTTP/1.1 200 OK\r\nX-Endless: ")
				chunk := bytes.Repeat([]byte("A"), 64<<10)
				for err == nil && total < endlessBytes {
					var n int
					n, err = conn.Write(chunk)
					total += n
				}
				written <- total
			}()
		}
	}()
	return "http://" + listener.Addr().String(), written
}

func TestEndlessResponseHeadersAreBounded(t *testing.T) {
	target, written := endlessHeaderTarget(t)
	s, err := NewScanner(Options{Target: target, Timeout: 20 * time.Second})
	if err != nil {
		t.Fatal(err)
	}

	probes := map[string]func(){
		"raw request":    func() { s.rawTCP("GET / HTTP/1.1") },
		"raw exchange":   func() { s.rawExchange("GET / HTTP/1.1\r\nHost: x\r\n\r\n") },
		"header bomb":    func() { runHeaderBombCheck(s) },
		"ws handshake":   func() { _, _, _ = s.wsDial("/ws", nil) },
		"client request": func() { s.do(http.MethodGet, "/", nil, "") },
	}
	for name, probe := range probes {
		probe()
		select {
		case total := <-written:
			if total >= writtenBefore {
				t.Errorf("%s: the target wrote %d MiB of one header before the scanner stopped reading", name, total>>20)
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("%s: the target never finished writing", name)
		}
	}
}

func TestOversizedWebSocketMessagesAreNotRead(t *testing.T) {
	written := make(chan int, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
		// Accept the probe's message, then stream one that never ends, in
		// fragments, so the scanner's limit has to stop it.
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		message, err := conn.NextWriter(websocket.TextMessage)
		if err != nil {
			return
		}
		chunk := bytes.Repeat([]byte("A"), 64<<10)
		total := 0
		for err == nil && total < endlessBytes {
			var n int
			n, err = message.Write(chunk)
			total += n
		}
		written <- total
	}))
	defer ts.Close()
	s, err := NewScanner(Options{Target: ts.URL, Timeout: 20 * time.Second})
	if err != nil {
		t.Fatal(err)
	}

	findings := runWSMsgBombCheck(s)
	wantStatus(t, findings, "WS-MSGBOMB", StatusError)
	select {
	case total := <-written:
		if total >= writtenBefore {
			t.Errorf("the target wrote %d MiB of one message before the scanner stopped reading", total>>20)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the target never finished writing")
	}
}
