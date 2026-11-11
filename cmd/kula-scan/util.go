package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"
)

// randToken returns n random hex characters, used to build unique throwaway
// usernames/origins so probes don't collide with real data on the target.
func randToken(n int) string {
	b := make([]byte, (n+1)/2)
	if _, err := rand.Read(b); err != nil {
		return "0000000000"[:n]
	}
	return hex.EncodeToString(b)[:n]
}

// jsonString JSON-encodes s into a quoted string literal, safely escaping any
// metacharacters so crafted usernames can't break out of the request body.
func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

// truncate shortens s to at most n runes, appending an ellipsis when cut.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// printable renders s for the text report. Control characters (C0, DEL, C1),
// bidirectional controls and bytes that are not UTF-8 are written as Go-style
// escapes (\x1b, \u009b, \u202e, \xff). Header values, certificate names and
// WebSocket close reasons come from the target: written raw, an escape
// sequence in one could set the operator's clipboard, or move the cursor up and
// rewrite an earlier FAIL line as a PASS.
func printable(s string) string {
	if !strings.ContainsFunc(s, func(r rune) bool { return r == utf8.RuneError || terminalControl(r) }) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case r < utf8.RuneSelf && terminalControl(r):
			fmt.Fprintf(&b, `\x%02x`, r)
		case terminalControl(r):
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

// terminalControl reports whether a terminal acts on r instead of drawing it.
func terminalControl(r rune) bool {
	return unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r)
}

// containsFold reports whether any element of vals equals target (case-insensitively).
func containsFold(vals []string, target string) bool {
	for _, v := range vals {
		// Vary may pack multiple tokens into one header value ("Origin, Accept").
		for _, part := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), target) {
				return true
			}
		}
	}
	return false
}

// cloneHeader returns a shallow copy of h (or a fresh header when h is nil) so a
// probe can add an Origin without mutating a shared header map.
func cloneHeader(h http.Header) http.Header {
	out := http.Header{}
	for k, vs := range h {
		for _, v := range vs {
			out.Add(k, v)
		}
	}
	return out
}

// statusOf renders a response status for evidence strings, tolerating nil.
func statusOf(resp *http.Response) string {
	if resp == nil {
		return "<no response>"
	}
	return resp.Status
}
