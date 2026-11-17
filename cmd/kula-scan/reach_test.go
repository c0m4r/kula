package main

import (
	"context"
	"net"
	"testing"
	"time"
)

// These tests cover how the scanner reaches its target: the one dial path,
// the address it dials, and the TLS server name it sends.

func TestDialAddressFillsTheSchemesPort(t *testing.T) {
	for target, want := range map[string]string{
		"http://[::1]":              "[::1]:80",
		"https://[2001:db8::1]":     "[2001:db8::1]:443",
		"http://[::1]:8080/":        "[::1]:8080",
		"http://[fe80::1%25eth0]/":  "[fe80::1%eth0]:80",
		"http://127.0.0.1":          "127.0.0.1:80",
		"https://kula.example":      "kula.example:443",
		"https://kula.example:8443": "kula.example:8443",
		"http://kula.example:27960": "kula.example:27960",
	} {
		s, err := NewScanner(Options{Target: target})
		if err != nil {
			t.Errorf("%s: %v", target, err)
			continue
		}
		if s.network != "tcp" || s.address != want {
			t.Errorf("%s dials %s %s, want tcp %s", target, s.network, s.address, want)
		}
	}
}

func TestTLSServerNameIsTheURLsHostName(t *testing.T) {
	for target, want := range map[string]string{
		"https://kula.example:8443/kula": "kula.example",
		"https://[2001:db8::1]":          "2001:db8::1",
		"https://127.0.0.1":              "127.0.0.1",
	} {
		s, err := NewScanner(Options{Target: target})
		if err != nil {
			t.Fatal(err)
		}
		if got := s.tlsConfig().ServerName; got != want {
			t.Errorf("%s: ServerName %q, want %q", target, got, want)
		}
	}
}

// silentListener accepts connections and never answers on them.
func silentListener(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		var held []net.Conn
		defer func() {
			for _, c := range held {
				_ = c.Close()
			}
		}()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			held = append(held, conn)
		}
	}()
	return listener
}

// TestTLSCheckEndsWithTheRun: a handshake the target never answers is
// abandoned when the run is interrupted, not after -timeout.
func TestTLSCheckEndsWithTheRun(t *testing.T) {
	listener := silentListener(t)
	s, err := NewScanner(Options{Target: "https://" + listener.Addr().String(), Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.ctx = ctx
	timer := time.AfterFunc(200*time.Millisecond, cancel)
	defer timer.Stop()

	start := time.Now()
	findings := runTLSChecks(s)
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("the TLS check took %s after the interrupt", elapsed)
	}
	wantStatus(t, findings, "TLS-001", StatusWarn)
}
