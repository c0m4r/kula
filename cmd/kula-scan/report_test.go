package main

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// rewrite moves the cursor up a line, erases it and prints a fake verdict: a
// target that gets it into the report can turn a FAIL above into a PASS.
const rewrite = "\x1b[1A\x1b[2K  PASS  AUTH-001   fake"

// osc52 asks the terminal to put "rm -rf ~" on the clipboard.
const osc52 = "\x1b]52;c;cm0gLXJmIH4K\x07"

func TestPrintableEscapesTerminalControls(t *testing.T) {
	cases := map[string]string{
		"plain":       "Server: nginx/1.27 · café",
		"osc52":       osc52,
		"rewrite":     rewrite,
		"c1 csi":      "a\u009b31m",
		"bidi":        "a\u202eb\u2066",
		"del and nul": "a\x7f\x00",
		"invalid":     "a\xffb",
		"newline":     "a\r\nb",
	}
	want := map[string]string{
		"plain":       "Server: nginx/1.27 · café",
		"osc52":       `\x1b]52;c;cm0gLXJmIH4K\x07`,
		"rewrite":     `\x1b[1A\x1b[2K  PASS  AUTH-001   fake`,
		"c1 csi":      `a\u009b31m`,
		"bidi":        `a\u202eb\u2066`,
		"del and nul": `a\x7f\x00`,
		"invalid":     `a\xffb`,
		"newline":     `a\x0d\x0ab`,
	}
	for name, input := range cases {
		if got := printable(input); got != want[name] {
			t.Errorf("%s: printable(%q) = %q, want %q", name, input, got, want[name])
		}
	}
}

func TestTextReportEscapesTargetBytes(t *testing.T) {
	findings := []Finding{
		finding("AUTH-001", "auth", "Protected API requires auth", SevHigh, StatusFail, "detail "+osc52).
			withEvidence("Server: x%s", rewrite),
		finding("HDR-009", "headers", "title \u009b2J", SevInfo, StatusWarn, "").
			withEvidence("Location: \u202eelpmaxe.live").
			withRemediation("fix \x07"),
	}
	report := newReport("https://target.example\x1b]0;PWNED\x07", findings)

	for _, noColor := range []bool{true, false} {
		var output bytes.Buffer
		report.writeText(&output, noColor)
		text := output.String()
		if bad, found := firstTerminalControl(text); found {
			t.Fatalf("noColor=%v: the report writes %q to the terminal:\n%s", noColor, bad, text)
		}
		for _, want := range []string{`\x1b]52;c;cm0gLXJmIH4K\x07`, `\x1b[1A\x1b[2K`, `\u009b2J`, `\u202eelpmaxe`, `fix \x07`, `\x1b]0;PWNED\x07`} {
			if !strings.Contains(text, want) {
				t.Errorf("noColor=%v: report does not show %q:\n%s", noColor, want, text)
			}
		}
	}

	// The JSON report escapes the same characters and decodes to the
	// target's text unchanged, for tools to read.
	var encoded bytes.Buffer
	if err := report.writeJSON(&encoded); err != nil {
		t.Fatal(err)
	}
	if bad, found := firstTerminalControl(encoded.String()); found {
		t.Fatalf("the JSON report writes %q to the terminal:\n%s", bad, encoded.String())
	}
	var decoded Report
	if err := json.Unmarshal(encoded.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	for index, want := range findings {
		got := decoded.Findings[index]
		if got.Title != want.Title || got.Detail != want.Detail ||
			got.Evidence != want.Evidence || got.Remediation != want.Remediation {
			t.Errorf("JSON finding %d decodes to %+v, want %+v", index, got, want)
		}
	}
	if decoded.Target != report.Target {
		t.Errorf("JSON target = %q, want %q", decoded.Target, report.Target)
	}
}

// hostileTarget answers every request with an escape sequence in the status
// line's reason phrase, which Go's client keeps, and C1 controls in a header
// value, which it does not reject either.
func hostileTarget(t *testing.T) string {
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
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				if _, err := http.ReadRequest(bufio.NewReader(conn)); err != nil {
					return
				}
				_, _ = io.WriteString(conn, "HTTP/1.1 418 "+osc52+rewrite+"\r\n"+
					"Server: evil\u009b1A\u009b2K  PASS  AUTH-001   fake\r\n"+
					"Content-Length: 0\r\nConnection: close\r\n\r\n")
			}()
		}
	}()
	return "http://" + listener.Addr().String()
}

func TestHostileResponsesCannotRewriteTheReport(t *testing.T) {
	target := hostileTarget(t)
	s, err := NewScanner(Options{Target: target, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	findings := s.discover()
	findings = append(findings, runHeaderChecks(s)...)
	findings = append(findings, runWSChecks(s)...)

	if f, ok := byID(findings, "HDR-009"); !ok || !strings.Contains(f.Evidence, "\u009b") {
		t.Fatalf("the Server header did not reach the evidence: %+v", f)
	}
	if f, ok := byID(findings, "WS-002"); !ok || !strings.Contains(f.Evidence, "\x1b") {
		t.Fatalf("the status line did not reach the evidence: %+v", f)
	}
	var output bytes.Buffer
	newReport(target, findings).writeText(&output, true)
	text := output.String()
	if bad, found := firstTerminalControl(text); found {
		t.Fatalf("the report writes %q to the terminal:\n%s", bad, text)
	}
	for _, want := range []string{`Server: evil\u009b1A\u009b2K`, `418 \x1b]52;c;cm0gLXJmIH4K\x07\x1b[1A\x1b[2K`} {
		if !strings.Contains(text, want) {
			t.Errorf("the report does not show %q:\n%s", want, text)
		}
	}
}

func TestHostileCertificateNamesCannotRewriteTheReport(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "hostile"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"evil" + osc52 + ".example"},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	ts.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	ts.Config.ErrorLog = log.New(io.Discard, "", 0) // the refused handshakes
	ts.StartTLS()
	defer ts.Close()

	// The name check comes before the chain check, so the certificate's
	// names reach the error without -insecure.
	target := strings.Replace(ts.URL, "127.0.0.1", "localhost", 1)
	s, err := NewScanner(Options{Target: target, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	findings := s.discover()
	if f, ok := byID(findings, "DISC-001"); !ok || !strings.Contains(f.Detail, "\x1b") {
		t.Fatalf("the certificate's name did not reach the finding: %+v", f)
	}
	var output bytes.Buffer
	newReport(target, findings).writeText(&output, true)
	if bad, found := firstTerminalControl(output.String()); found {
		t.Fatalf("the report writes %q to the terminal:\n%s", bad, output.String())
	}
	if !strings.Contains(output.String(), `evil\x1b]52;c;cm0gLXJmIH4K\x07.example`) {
		t.Fatalf("the report does not show the escaped certificate name:\n%s", output.String())
	}
}

// firstTerminalControl returns the first thing in text, other than a newline
// or an SGR colour sequence, that a terminal would act on instead of drawing.
func firstTerminalControl(text string) (string, bool) {
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		switch {
		case r == '\x1b':
			end := i + 1
			if end < len(text) && text[end] == '[' {
				end++
				for end < len(text) && (text[end] >= '0' && text[end] <= '9' || text[end] == ';') {
					end++
				}
				if end < len(text) && text[end] == 'm' {
					i = end + 1
					continue
				}
			}
			return text[i:min(len(text), i+16)], true
		case r == '\n':
		case r == utf8.RuneError && size == 1, terminalControl(r):
			return text[i : i+size], true
		}
		i += size
	}
	return "", false
}
