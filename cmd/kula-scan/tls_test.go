package main

import (
	"bytes"
	"crypto/fips140"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// These tests cover the TLS checks against Go TLS servers whose versions,
// suites and certificates each test chooses.

// tlsTarget serves a plain page over TLS with cfg (nil: Go's defaults and
// httptest's certificate).
func tlsTarget(t *testing.T, cfg *tls.Config) *httptest.Server {
	t.Helper()
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("kula is healthy"))
	}))
	ts.TLS = cfg
	// Each refused probe logs a handshake error; keep the test output quiet.
	ts.Config.ErrorLog = discardLog()
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return ts
}

// tlsScanner returns a scanner for ts, as -insecure unless verify.
func tlsScanner(t *testing.T, target string, opt Options) *Scanner {
	t.Helper()
	opt.Target = target
	if opt.Timeout == 0 {
		opt.Timeout = 5 * time.Second
	}
	s, err := NewScanner(opt)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// tlsFindings runs the TLS checks against ts with -insecure.
func tlsFindings(t *testing.T, ts *httptest.Server) []Finding {
	t.Helper()
	return runTLSChecks(tlsScanner(t, ts.URL, Options{Insecure: true}))
}

func TestTLSLegacyVersionsMustBeRefused(t *testing.T) {
	legacy := tlsTarget(t, &tls.Config{MinVersion: tls.VersionTLS10})
	findings := tlsFindings(t, legacy)
	wantStatus(t, findings, "TLS-001", StatusFail)
	if f, _ := byID(findings, "TLS-001"); !strings.Contains(f.Detail, "TLS 1.1 and TLS 1.0") ||
		!strings.Contains(f.Evidence, "a modern client negotiates TLS 1.3") {
		t.Errorf("TLS-001 does not name the versions accepted and the modern one: %q | %q", f.Detail, f.Evidence)
	}

	onlyTLS11 := tlsTarget(t, &tls.Config{MinVersion: tls.VersionTLS11})
	findings = tlsFindings(t, onlyTLS11)
	wantStatus(t, findings, "TLS-001", StatusFail)
	if f, _ := byID(findings, "TLS-001"); !strings.HasSuffix(f.Detail, ": TLS 1.1.") || !strings.Contains(f.Evidence, "TLS 1.0 refused") {
		t.Errorf("TLS-001 against a TLS 1.1 server: %q | %q", f.Detail, f.Evidence)
	}

	modern := tlsTarget(t, nil)
	findings = tlsFindings(t, modern)
	for _, id := range []string{"TLS-001", "TLS-002", "TLS-003"} {
		wantStatus(t, findings, id, StatusPass)
	}
	if f, _ := byID(findings, "TLS-001"); !strings.Contains(f.Evidence, "TLS 1.1 refused") || !strings.Contains(f.Evidence, "TLS 1.0 refused") {
		t.Errorf("TLS-001 evidence: %q", f.Evidence)
	}

	tls13 := tlsTarget(t, &tls.Config{MinVersion: tls.VersionTLS13})
	findings = tlsFindings(t, tls13)
	wantStatus(t, findings, "TLS-001", StatusPass)
	wantStatus(t, findings, "TLS-002", StatusPass) // it refuses TLS 1.2 altogether
}

func TestTLSWeakSuitesAreNamed(t *testing.T) {
	weak := tlsTarget(t, &tls.Config{
		MaxVersion: tls.VersionTLS12,
		CipherSuites: []uint16{
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA256,
		},
	})
	findings := tlsFindings(t, weak)
	wantStatus(t, findings, "TLS-002", StatusWarn)
	f, _ := byID(findings, "TLS-002")
	if !strings.Contains(f.Detail, "TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA256") || strings.Contains(f.Detail, "GCM") {
		t.Errorf("TLS-002 does not name the insecure suite alone: %q", f.Detail)
	}
	if !strings.Contains(f.Evidence, "a modern client negotiates TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256") {
		t.Errorf("TLS-002 evidence lacks the modern suite: %q", f.Evidence)
	}
	wantStatus(t, findings, "TLS-001", StatusPass)

	broken := tlsTarget(t, &tls.Config{
		MaxVersion: tls.VersionTLS12,
		CipherSuites: []uint16{
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_3DES_EDE_CBC_SHA,
			tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA256,
		},
	})
	findings = tlsFindings(t, broken)
	wantStatus(t, findings, "TLS-002", StatusFail)
	f, _ = byID(findings, "TLS-002")
	for _, suite := range []string{"TLS_ECDHE_RSA_WITH_3DES_EDE_CBC_SHA", "TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA256"} {
		if !strings.Contains(f.Detail, suite) {
			t.Errorf("TLS-002 does not name %s: %q", suite, f.Detail)
		}
	}
}

// selfSigned returns a certificate for 127.0.0.1 and kula.test, valid from
// notBefore to notAfter, with an RSA key of bits signed with alg (0: SHA-256).
func selfSigned(t *testing.T, notBefore, notAfter time.Time, bits int, alg x509.SignatureAlgorithm) tls.Certificate {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "kula.test"},
		DNSNames:              []string{"kula.test"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		SignatureAlgorithm:    alg,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

func TestTLSCertificateValidityAndStrength(t *testing.T) {
	now := time.Now()
	day := 24 * time.Hour
	cases := []struct {
		name   string
		cert   tls.Certificate
		status Status
		says   string
	}{
		{"valid", selfSigned(t, now.Add(-day), now.Add(90*day), 2048, 0), StatusPass, "within its validity window"},
		{"expired", selfSigned(t, now.Add(-90*day), now.Add(-day), 2048, 0), StatusFail, "it expired on"},
		{"not yet valid", selfSigned(t, now.Add(day), now.Add(90*day), 2048, 0), StatusFail, "it is not valid until"},
		{"expiring", selfSigned(t, now.Add(-day), now.Add(7*day), 2048, 0), StatusWarn, "expires on"},
		{"small key", selfSigned(t, now.Add(-day), now.Add(90*day), 1024, 0), StatusFail, "1024-bit RSA key"},
		{"sha-1", selfSigned(t, now.Add(-day), now.Add(90*day), 2048, x509.SHA1WithRSA), StatusFail, "signed with SHA1-RSA"},
	}
	for _, c := range cases {
		ts := tlsTarget(t, &tls.Config{Certificates: []tls.Certificate{c.cert}})
		f, ok := byID(tlsFindings(t, ts), "TLS-003")
		if !ok || f.Status != c.status || !strings.Contains(f.Detail, c.says) {
			t.Errorf("%s: TLS-003 = %s %q, want %s saying %q", c.name, f.Status, f.Detail, c.status, c.says)
		}
	}

	// Several problems are all named, the worst one decides.
	chain := []*x509.Certificate{
		selfSigned(t, now.Add(-90*day), now.Add(-day), 1024, x509.SHA1WithRSA).Leaf,
	}
	f := certificateFinding(chain, now)
	if f.Status != StatusFail || f.Severity != SevHigh ||
		!strings.Contains(f.Detail, "expired") || !strings.Contains(f.Detail, "1024-bit") || !strings.Contains(f.Detail, "SHA1-RSA") {
		t.Errorf("an expired, small, SHA-1 certificate: %s %s %q", f.Status, f.Severity, f.Detail)
	}
	// A self-signed root sent along is not judged; an intermediate is.
	root := selfSigned(t, now.Add(-day), now.Add(90*day), 2048, x509.SHA1WithRSA).Leaf
	leaf := selfSigned(t, now.Add(-day), now.Add(90*day), 2048, 0).Leaf
	if f := certificateFinding([]*x509.Certificate{leaf, root}, now); f.Status != StatusPass {
		t.Errorf("a SHA-1 root sent along with the chain: %s %q", f.Status, f.Detail)
	}
	intermediate := *root
	intermediate.RawIssuer = []byte("another issuer")
	if f := certificateFinding([]*x509.Certificate{leaf, &intermediate}, now); f.Status != StatusFail || !strings.Contains(f.Detail, "the intermediate") {
		t.Errorf("a SHA-1 intermediate: %s %q", f.Status, f.Detail)
	}
}

// TestTLSTrustIsVerifiedWithInsecure: -insecure skips verification in the
// handshake, so the scanner verifies the chain and the name itself: a
// certificate a browser refuses is a WARN that -expect tls turns into a FAIL.
func TestTLSTrustIsVerifiedWithInsecure(t *testing.T) {
	ts := tlsTarget(t, nil) // httptest's self-signed certificate

	f, _ := byID(tlsFindings(t, ts), "TLS-004")
	if f.Status != StatusWarn || f.Expectation != "tls" || !strings.Contains(f.Detail, "self-signed") {
		t.Errorf("TLS-004 for a self-signed certificate = %s, expectation %q, %q; want a WARN tagged tls", f.Status, f.Expectation, f.Detail)
	}

	s := tlsScanner(t, ts.URL, Options{Insecure: true, Expect: []string{"tls"}})
	findings := runTLSChecks(s)
	s.applyExpectations(findings)
	if f, _ := byID(findings, "TLS-004"); f.Status != StatusFail || f.Expectation != "tls" {
		t.Errorf("TLS-004 under -expect tls = %s, expectation %q; want a FAIL against tls", f.Status, f.Expectation)
	}

	// Trusted, it passes; under another name, it does not.
	pool := x509.NewCertPool()
	pool.AddCert(ts.Certificate())
	verifyRoots = pool
	t.Cleanup(func() { verifyRoots = nil })
	wantStatus(t, tlsFindings(t, ts), "TLS-004", StatusPass)

	s = tlsScanner(t, "https://kula-scan.invalid/", Options{Insecure: true, Connect: ts.Listener.Addr().String()})
	f, _ = byID(runTLSChecks(s), "TLS-004")
	if f.Status != StatusWarn || !strings.Contains(f.Detail, "kula-scan.invalid") || !strings.Contains(f.Detail, "names other hosts") {
		t.Errorf("TLS-004 under a name the certificate lacks = %s %q", f.Status, f.Detail)
	}

	// Without -insecure the handshake verified the chain already.
	verifyRoots = nil
	s = tlsScanner(t, ts.URL, Options{})
	s.insecure = false
	if f := trustFinding(s, tls.ConnectionState{}); f.Status != StatusPass {
		t.Errorf("TLS-004 without -insecure = %s %q", f.Status, f.Detail)
	}
}

func TestTLSChecksOnPlainHTTP(t *testing.T) {
	s := tlsScanner(t, "http://kula-scan.invalid", Options{})
	findings := runTLSChecks(s)
	for _, id := range []string{"TLS-001", "TLS-002", "TLS-003", "TLS-004"} {
		wantStatus(t, findings, id, StatusSkip)
	}

	s = tlsScanner(t, "http://kula-scan.invalid", Options{Expect: []string{"tls"}})
	findings = runTLSChecks(s)
	s.applyExpectations(findings)
	wantStatus(t, findings, "TLS-001", StatusFail)
	for _, id := range []string{"TLS-002", "TLS-003", "TLS-004"} {
		wantStatus(t, findings, id, StatusSkip)
	}
}

// TestTLSChecksWithoutAHandshakeAreErrors: a target the scanner cannot
// connect to, or that hangs up before any handshake, gives no PASS.
func TestTLSChecksWithoutAHandshakeAreErrors(t *testing.T) {
	for name, address := range map[string]string{
		"closed port": closedAddress(t),
		"hang-up":     hangUpListener(t, 0),
	} {
		findings := runTLSChecks(tlsScanner(t, "https://"+address, Options{Insecure: true}))
		for _, id := range []string{"TLS-001", "TLS-002", "TLS-003", "TLS-004"} {
			if f, _ := byID(findings, id); f.Status != StatusError {
				t.Errorf("%s: %s = %s %q, want ERROR", name, id, f.Status, f.Detail)
			}
		}
	}
}

// TestTLSProbesThatGetNoAnswerAreErrors: a probing handshake that times out
// learns nothing, so it is not counted as refused.
func TestTLSProbesThatGetNoAnswerAreErrors(t *testing.T) {
	s := tlsScanner(t, "https://"+silentListener(t).Addr().String(), Options{Timeout: 200 * time.Millisecond})
	if h := s.tryHandshake(s.probeConfig(tls.VersionTLS10, tls.VersionTLS10, nil)); h.err == nil || h.refused {
		t.Errorf("a handshake the target never answered: err %v, refused %v; want an error, not a refusal", h.err, h.refused)
	}
	s = tlsScanner(t, "https://"+closedAddress(t), Options{})
	if h := s.tryHandshake(s.tlsConfig()); h.err == nil || h.refused {
		t.Errorf("a connection that could not be made: err %v, refused %v; want an error, not a refusal", h.err, h.refused)
	}
	s = tlsScanner(t, "https://"+hangUpListener(t, 0), Options{})
	if h := s.tryHandshake(s.tlsConfig()); h.err == nil || !h.refused {
		t.Errorf("a handshake the target hung up on: err %v, refused %v; want a refusal", h.err, h.refused)
	}
}

// countingListener accepts connections, reads what each sends for up to a
// second, then answers it with answer (nothing when empty) and holds it open
// a little longer: it never refuses a handshake. received counts the bytes
// read.
func countingListener(t *testing.T, answer string) (string, *atomic.Int64) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	received := &atomic.Int64{}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				_ = conn.SetDeadline(time.Now().Add(time.Second))
				n, _ := conn.Read(make([]byte, 4096))
				received.Add(int64(n))
				if answer != "" {
					_, _ = conn.Write([]byte(answer))
				}
				time.Sleep(time.Second)
			}()
		}
	}()
	return listener.Addr().String(), received
}

// TestTLSProbesTheClientCannotMakeAreNotRefusals: only an alert from the
// target, or a close or reset after the ClientHello, is a refusal. A probe
// the scanner's own client gives up on before sending anything was never
// put to the target; an answer that is not TLS refuses nothing either.
func TestTLSProbesTheClientCannotMakeAreNotRefusals(t *testing.T) {
	address, received := countingListener(t, "")
	s := tlsScanner(t, "https://"+address, Options{Timeout: 500 * time.Millisecond})
	// No version is both at least TLS 1.2 and at most TLS 1.1: the client
	// fails before it sends anything, as in FIPS 140 mode for TLS 1.0.
	h := s.tryHandshake(s.probeConfig(tls.VersionTLS12, tls.VersionTLS11, nil))
	time.Sleep(100 * time.Millisecond)
	if h.err == nil || h.refused || !h.local || received.Load() != 0 {
		t.Errorf("a handshake the client could not build: err %v, refused %v, local %v, %d bytes sent; want a local failure, no refusal, nothing sent",
			h.err, h.refused, h.local, received.Load())
	}

	plain, _ := countingListener(t, "HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n")
	s = tlsScanner(t, "https://"+plain, Options{Timeout: 500 * time.Millisecond})
	if h := s.tryHandshake(s.probeConfig(tls.VersionTLS10, tls.VersionTLS10, nil)); h.err == nil || h.refused || h.local {
		t.Errorf("a ClientHello answered in plain HTTP: err %v, refused %v, local %v; want neither a refusal nor a local failure", h.err, h.refused, h.local)
	}

	// TLS-001 with a version the client cannot offer is an ERROR that says
	// the probe could not be made, not a pass.
	modern := tlsTarget(t, nil)
	saved := legacyVersions
	legacyVersions = []uint16{0x0300} // SSL 3.0, which Go's client never offers
	t.Cleanup(func() { legacyVersions = saved })
	f, _ := byID(tlsFindings(t, modern), "TLS-001")
	if f.Status != StatusError || !strings.Contains(f.Detail, "could not be made") || !strings.Contains(f.Evidence, "not sent") {
		t.Errorf("TLS-001 with a probe the client cannot make = %s %q | %q; want an ERROR", f.Status, f.Detail, f.Evidence)
	}
}

// TestClientOfferReadsTheClientHello: clientOffer reports the suites the
// scanner's client puts in its ClientHello, and the client's own failure.
func TestClientOfferReadsTheClientHello(t *testing.T) {
	s := tlsScanner(t, "https://kula-scan.invalid", Options{})
	insecure := suiteIDs(tls.InsecureCipherSuites())
	offered, err := clientOffer(s.probeConfig(tls.VersionTLS12, tls.VersionTLS12, insecure))
	if err != nil || !fips140.Enabled() && len(offered) != len(insecure) {
		t.Errorf("the insecure suites offered: %v (%v); want all %d", suiteNames(offered), err, len(insecure))
	}
	if _, err := clientOffer(s.probeConfig(tls.VersionTLS12, tls.VersionTLS11, nil)); err == nil {
		t.Errorf("a ClientHello the client cannot build was read")
	}
}

// fipsChildEnv marks the run of the test binary that
// TestTLSProbesInFIPSModeAreErrors starts in FIPS 140 mode.
const fipsChildEnv = "KULA_SCAN_TEST_FIPS_CHILD"

// TestTLSProbesInFIPSModeAreErrors runs the TLS checks in FIPS 140 mode, in
// a run of the test binary of its own: Go's client then builds no handshake
// below TLS 1.2 and offers only the approved insecure suites, so TLS-001 and
// TLS-002 cannot be judged, which is an ERROR, not a pass.
func TestTLSProbesInFIPSModeAreErrors(t *testing.T) {
	if os.Getenv(fipsChildEnv) == "1" {
		if !fips140.Enabled() {
			t.Skip("FIPS 140 mode is not available")
		}
		findings := tlsFindings(t, tlsTarget(t, nil))
		for _, id := range []string{"TLS-001", "TLS-002"} {
			if f, _ := byID(findings, id); f.Status != StatusError || !strings.Contains(f.Detail, "FIPS 140 mode") {
				t.Errorf("%s in FIPS 140 mode = %s %q | %q; want an ERROR that names the mode", id, f.Status, f.Detail, f.Evidence)
			}
		}
		wantStatus(t, findings, "TLS-003", StatusPass)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestTLSProbesInFIPSModeAreErrors$", "-test.v")
	cmd.Env = append(os.Environ(), fipsChildEnv+"=1", "GODEBUG=fips140=on")
	out, err := cmd.CombinedOutput()
	switch {
	case err != nil:
		t.Errorf("the TLS checks in FIPS 140 mode: %v\n%s", err, out)
	case bytes.Contains(out, []byte("--- SKIP")):
		t.Skipf("FIPS 140 mode is not available:\n%s", out)
	}
}

// discardLog is an error log for test servers whose errors are expected.
func discardLog() *log.Logger {
	return log.New(io.Discard, "", 0)
}
