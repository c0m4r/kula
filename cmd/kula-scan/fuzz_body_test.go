package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestFuzzBodiesAreTheCasesThatMatter: the login bodies fit the login
// budget, none decodes into a login (so none touches an account's limit or
// costs a hash), and all but the oversized one fit Kula's 4 KiB cap; no
// chat body decodes into a chat request, cap or not, and the chat bodies fit
// the chat limiter with room to spare.
func TestFuzzBodiesAreTheCasesThatMatter(t *testing.T) {
	login := loginFuzzBodies()
	if len(login) != fuzzLoginBodies || fuzzLoginBodies > kulaLoginAddressLimit-kulaLoginAccountLimit {
		t.Errorf("%d login bodies, fuzzLoginBodies %d; want them equal and well within the %d attempts an address has",
			len(login), fuzzLoginBodies, kulaLoginAddressLimit)
	}
	for _, b := range login {
		var creds struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if json.NewDecoder(bytes.NewReader([]byte(b.body))).Decode(&creds) == nil {
			t.Errorf("login body %q decodes into a login", b.name)
		}
		if oversized := strings.Contains(b.name, "past the 4 KiB cap"); oversized != (len(b.body) > 4096) {
			t.Errorf("login body %q is %d bytes", b.name, len(b.body))
		}
	}

	chat := chatFuzzBodies()
	if len(chat) != fuzzChatBodies || fuzzChatBodies > kulaChatLimit-2 {
		t.Errorf("%d chat bodies, fuzzChatBodies %d; want them equal and below the chat limit of %d with room to spare",
			len(chat), fuzzChatBodies, kulaChatLimit)
	}
	for _, b := range chat {
		if decodesAsChat([]byte(b.body)) {
			t.Errorf("chat body %q decodes into a chat request: it would start a chat", b.name)
		}
		if oversized := strings.Contains(b.name, "past the 32 KiB cap"); oversized != (len(b.body) > 32*1024) {
			t.Errorf("chat body %q is %d bytes", b.name, len(b.body))
		}
	}
	// The mirror is the right one: what Kula's decoder takes, it takes.
	for _, body := range []string{`null`, `{}`, `{"unknown":{"a":{"a":1}}}`, `{"prompt":"hi","messages":[{"role":"user","content":"x"}]}`} {
		if !decodesAsChat([]byte(body)) {
			t.Errorf("%s does not decode into the chat request mirror", body)
		}
	}
}

// TestFuzzBodyStaysInTheLoginBudget: the login half sends at most six
// bodies, stops at the first 429 and says how many reached the parser: all
// of them on a fresh address, two on an address with two attempts left (a
// WARN), none on a spent one (a SKIP saying when to retry).
func TestFuzzBodyStaysInTheLoginBudget(t *testing.T) {
	for _, c := range []struct {
		left     int
		want     Status
		attempts int
		detail   string
	}{
		{15, StatusPass, 6, "login: 6 planned, 6 sent: 6 reached its body parser"},
		{2, StatusWarn, 3, "login: 6 planned, 3 sent: 2 reached its body parser, 1 throttled by the login limiter (15 attempts per address, " +
			"5 per account and address, in five minutes; retry in five minutes); 3 not sent once throttled"},
		{0, StatusSkip, 1, "login: 6 planned, 1 sent: 0 reached its body parser, 1 throttled by the login limiter"},
	} {
		m := newKulaMock(true, false)
		m.loginLimit = c.left
		s, _ := m.serve(t, Options{})
		f, _ := byID(runFuzzBody(s), "FUZZ-BODY")
		if f.Status != c.want || !strings.Contains(f.Detail, c.detail) {
			t.Errorf("%d attempts left: FUZZ-BODY = %s: %s; want %s with %q", c.left, f.Status, f.Detail, c.want, c.detail)
		}
		if c.left == 0 && !strings.Contains(f.Detail, "retry in five minutes") {
			t.Errorf("a spent login budget does not say when to retry: %s", f.Detail)
		}
		if m.loginAttempts != c.attempts || len(m.accountTries) != 0 {
			t.Errorf("%d attempts left: %d login attempts, accounts tried %v; want %d and none", c.left, m.loginAttempts, m.accountTries, c.attempts)
		}
	}
}

// TestFuzzBodyNeverStartsAChat: the chat half sends only bodies the chat's
// decoder refuses, past the auth and CSRF gates, and no more than the chat
// limiter allows; behind auth it learns the assistant is on from
// /api/config with its session.
func TestFuzzBodyNeverStartsAChat(t *testing.T) {
	for _, auth := range []bool{true, false} {
		m := newKulaMock(auth, true)
		s, _ := m.serve(t, Options{Username: kmUser, Password: kmPass})
		f, _ := byID(runFuzzBody(s), "FUZZ-BODY")
		wantStatus(t, []Finding{f}, "FUZZ-BODY", StatusPass)
		if !strings.Contains(f.Detail, "AI chat: 6 planned, 6 sent: 6 reached its body decoder") {
			t.Errorf("auth %v: FUZZ-BODY does not cover the chat: %s", auth, f.Detail)
		}
		if len(m.chatDecoded) != 0 {
			t.Errorf("auth %v: %d chat bodies decode into a chat request; Kula would have started a chat with each", auth, len(m.chatDecoded))
		}
		if len(m.chatBodies) != fuzzChatBodies {
			t.Errorf("auth %v: %d bodies reached the chat, want %d", auth, len(m.chatBodies), fuzzChatBodies)
		}
		for _, r := range m.requests() {
			if r.path != "/api/ollama/chat" {
				continue
			}
			if !strings.HasPrefix(r.header.Get("Origin"), "http://127.0.0.1:") {
				t.Errorf("auth %v: a chat request carries Origin %q", auth, r.header.Get("Origin"))
			}
			if auth && (r.header.Get("Cookie") != "kula_session="+kmSession || r.header.Get("X-CSRF-Token") != kmCSRF) {
				t.Errorf("a chat request behind auth carries Cookie %q and X-CSRF-Token %q", r.header.Get("Cookie"), r.header.Get("X-CSRF-Token"))
			}
		}
	}

	// A chat limiter with two requests left: the half stops at its 429.
	m := newKulaMock(false, true)
	m.chatLimit = 2
	s, _ := m.serve(t, Options{})
	f, _ := byID(runFuzzBody(s), "FUZZ-BODY")
	if f.Status != StatusWarn || !strings.Contains(f.Detail,
		"AI chat: 6 planned, 3 sent: 2 reached its body decoder, 1 throttled by the AI chat limiter (10 requests a minute per address; retry in a minute)") {
		t.Errorf("FUZZ-BODY with two chats left = %s: %s", f.Status, f.Detail)
	}

	// Behind auth without a session, the chat half is left out, saying why.
	m = newKulaMock(true, true)
	s, _ = m.serve(t, Options{})
	f, _ = byID(runFuzzBody(s), "FUZZ-BODY")
	if len(m.chatBodies) != 0 || !strings.Contains(f.Detail, "Not covered: auth is enabled and no session is available to reach the AI chat") {
		t.Errorf("FUZZ-BODY without a session sent %d chat bodies: %s", len(m.chatBodies), f.Detail)
	}
}
