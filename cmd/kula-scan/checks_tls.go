package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/fips140"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

// TLS posture checks (https targets only). A modern client's handshake gives
// the negotiated version and suite and the certificate; the checks then probe
// what must not work: handshakes capped at TLS 1.1 and at TLS 1.0, and a TLS
// 1.2 handshake offering only the suites Go lists as insecure, must all be
// refused. A refusal is the target's: an alert, or a close or reset after the
// ClientHello was sent. A probe the scanner's own client cannot make, as in
// FIPS 140 mode, asks the target nothing, which leaves the check an ERROR.
// Every probe goes through dial, so -connect, -unix and the run context
// apply, and closes its connection once the handshake is done. They are
// read-only and non-aggressive.

func tlsChecks() []check {
	return []check{{ids: []string{"TLS-001", "TLS-002", "TLS-003", "TLS-004"}, run: runTLSChecks}}
}

// verifyRoots is the pool the certificate is verified against when -insecure
// skips verification in the handshake; nil means the system roots. Tests set
// their own.
var verifyRoots *x509.CertPool

func tlsVersionName(v uint16) string {
	switch v {
	case tls.VersionTLS13:
		return "TLS 1.3"
	case tls.VersionTLS12:
		return "TLS 1.2"
	case tls.VersionTLS11:
		return "TLS 1.1"
	case tls.VersionTLS10:
		return "TLS 1.0"
	default:
		return fmt.Sprintf("0x%04x", v)
	}
}

func runTLSChecks(s *Scanner) []Finding {
	if !s.https {
		const plain = "target is plain HTTP; no TLS layer to inspect."
		first := finding("TLS-001", SevInfo, StatusSkip, plain)
		if s.expects("tls") {
			first = finding("TLS-001", SevHigh, StatusFail,
				"target is plain HTTP, but -expect tls declares it is served over TLS.").
				withRemediation("Scan the HTTPS URL, or serve kula over HTTPS (TLS-terminating reverse proxy).").
				expecting("tls")
		}
		return []Finding{first,
			finding("TLS-002", SevInfo, StatusSkip, plain),
			finding("TLS-003", SevInfo, StatusSkip, plain),
			finding("TLS-004", SevInfo, StatusSkip, plain),
		}
	}

	// A modern client's handshake, verifying the certificate unless
	// -insecure: what any browser gets. It ends with the run.
	conn, err := s.dialTLS(s.ctx)
	if err != nil {
		detail := "could not complete a TLS handshake with the target: " + err.Error()
		return []Finding{
			finding("TLS-001", SevInfo, StatusError, detail),
			finding("TLS-002", SevInfo, StatusError, detail),
			finding("TLS-003", SevInfo, StatusError, detail),
			finding("TLS-004", SevInfo, StatusError, detail),
		}
	}
	st := conn.ConnectionState()
	_ = conn.Close()

	return []Finding{
		legacyVersionFinding(s, st),
		weakSuiteFinding(s, st),
		certificateFinding(st.PeerCertificates, time.Now()),
		trustFinding(s, st),
	}
}

// handshake is how one probing handshake ended.
type handshake struct {
	state tls.ConnectionState // when it completed
	err   error               // nil when it completed
	// refused: the client sent its ClientHello and the target ended the
	// handshake, with an alert or by closing or resetting the connection.
	refused bool
	// local: the scanner's own TLS client failed before it sent anything,
	// so the target was never asked. In FIPS 140 mode, for one, Go's client
	// builds no handshake below TLS 1.2.
	local bool
	// A handshake that is none of these taught nothing: the dial failed,
	// the target did not answer in time, or it answered with something the
	// client could not take, which is no refusal.
}

// sentConn notes whether the TLS client wrote to the connection, which it
// first does to send its ClientHello.
type sentConn struct {
	net.Conn
	sent atomic.Bool
}

func (c *sentConn) Write(p []byte) (int, error) {
	c.sent.Store(true)
	return c.Conn.Write(p)
}

// remoteAlert reports whether err is a TLS alert the target sent: Go's
// client reports one as a *net.OpError whose Op is "remote error".
func remoteAlert(err error) bool {
	var opErr *net.OpError
	return errors.As(err, &opErr) && opErr.Op == "remote error"
}

// tryHandshake connects through dial and attempts a TLS handshake under cfg,
// then closes the connection. -timeout bounds the dial and the handshake
// together, and an interrupt ends them.
func (s *Scanner) tryHandshake(cfg *tls.Config) handshake {
	ctx, cancel := context.WithTimeout(s.ctx, s.timeout)
	defer cancel()
	raw, err := s.dial(ctx)
	if err != nil {
		return handshake{err: err}
	}
	conn := &sentConn{Conn: raw}
	tlsConn := tls.Client(conn, cfg)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		h := handshake{err: err}
		switch {
		case ctx.Err() != nil || isReadTimeout(err):
			// no answer in time, or the run ended
		case !conn.sent.Load():
			h.local = true
		default:
			h.refused = remoteAlert(err) || isHangUp(err)
		}
		return h
	}
	state := tlsConn.ConnectionState()
	_ = tlsConn.Close()
	return handshake{state: state}
}

// errHelloRead ends the in-process handshake clientOffer reads a ClientHello
// with.
var errHelloRead = errors.New("ClientHello read")

// clientOffer returns the cipher suites a ClientHello built under cfg
// offers. Go's client leaves out, without an error, each configured suite it
// will not offer, as in FIPS 140 mode it does every suite that is not
// approved. The scanner's own TLS client builds the hello and sends it over
// a pipe to an in-process server that reads it and ends the handshake, so
// nothing reaches the target. err is the client's failure to build a hello.
func clientOffer(cfg *tls.Config) ([]uint16, error) {
	client, server := net.Pipe()
	deadline := time.Now().Add(5 * time.Second)
	_ = client.SetDeadline(deadline)
	_ = server.SetDeadline(deadline)
	offered := make(chan []uint16, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = server.Close() }()
		_ = tls.Server(server, &tls.Config{
			GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
				offered <- slices.Clone(hello.CipherSuites)
				return nil, errHelloRead
			},
		}).Handshake()
	}()
	err := tls.Client(client, cfg).Handshake()
	_ = client.Close() // a client that sent nothing leaves the server waiting
	<-done
	select {
	case suites := <-offered:
		return suites, nil
	default:
		return nil, err
	}
}

// clientLimits explains, after a probe the scanner's TLS client could not
// make, why it may not have: FIPS 140 mode restricts it.
func clientLimits() string {
	if fips140.Enabled() {
		return " The scanner runs in FIPS 140 mode (GODEBUG=fips140), whose TLS client offers no version below TLS 1.2 and only approved cipher suites; run it without that mode to make these probes."
	}
	return ""
}

// probeConfig returns a clone of the scanner's TLS configuration, with its
// server name, that offers only versions minVersion to maxVersion and the
// given cipher suites. It does not verify the certificate: a probe asks
// whether the target agrees to these terms, which it has done once it
// presents a certificate under them; the modern handshake and TLS-004 judge
// the certificate. A probe sends nothing over the connection.
func (s *Scanner) probeConfig(minVersion, maxVersion uint16, suites []uint16) *tls.Config {
	cfg := s.tlsConfig().Clone()
	cfg.MinVersion, cfg.MaxVersion = minVersion, maxVersion
	cfg.CipherSuites = suites
	// The probe sends no data and judges no certificate (see above).
	cfg.InsecureSkipVerify = true // nosemgrep: bypass-tls-verification -- a version/suite probe sends no data; the modern handshake and TLS-004 verify the certificate
	return cfg
}

// suiteIDs returns the IDs of suites.
func suiteIDs(suites []*tls.CipherSuite) []uint16 {
	ids := make([]uint16, 0, len(suites))
	for _, suite := range suites {
		ids = append(ids, suite.ID)
	}
	return ids
}

// legacyVersions are the protocol versions an endpoint must refuse, newest
// first. Go's client speaks nothing older.
var legacyVersions = []uint16{tls.VersionTLS11, tls.VersionTLS10}

// legacyVersionFinding is TLS-001: handshakes capped at TLS 1.1 and at TLS
// 1.0, each offering every suite Go implements, must be refused. Accepted is
// the finding; refused, the pass; a probe that could not be made, by the
// scanner's own client or for want of an answer, leaves the check an ERROR.
// modern is the modern client's handshake, for the evidence.
func legacyVersionFinding(s *Scanner, modern tls.ConnectionState) Finding {
	everySuite := append(suiteIDs(tls.CipherSuites()), suiteIDs(tls.InsecureCipherSuites())...)
	evidence := []string{"a modern client negotiates " + tlsVersionName(modern.Version)}
	var accepted, unmade, unknown []string
	for _, version := range legacyVersions {
		name := tlsVersionName(version)
		h := s.tryHandshake(s.probeConfig(version, version, everySuite))
		switch {
		case h.err == nil:
			accepted = append(accepted, name)
			evidence = append(evidence, fmt.Sprintf("%s accepted with %s", name, tls.CipherSuiteName(h.state.CipherSuite)))
		case h.refused:
			evidence = append(evidence, fmt.Sprintf("%s refused (%v)", name, h.err))
		case h.local:
			unmade = append(unmade, name)
			evidence = append(evidence, fmt.Sprintf("%s not sent (%v)", name, h.err))
		default:
			unknown = append(unknown, name)
			evidence = append(evidence, fmt.Sprintf("%s not learnt (%v)", name, h.err))
		}
	}
	switch {
	case len(accepted) > 0:
		return finding("TLS-001", SevHigh, StatusFail,
			"the server completes handshakes at deprecated protocol versions: "+strings.Join(accepted, " and ")+".").
			withEvidence("%s", strings.Join(evidence, "; ")).
			withRemediation("Terminate TLS with a proxy configured for TLS 1.2+ only (nginx: ssl_protocols TLSv1.2 TLSv1.3).")
	case len(unmade) > 0:
		return finding("TLS-001", SevInfo, StatusError,
			"the probe for "+strings.Join(unmade, " and ")+" could not be made: the scanner's own TLS client failed before it sent anything, so the server was not asked."+clientLimits()).
			withEvidence("%s", strings.Join(evidence, "; "))
	case len(unknown) > 0:
		return finding("TLS-001", SevInfo, StatusError,
			"could not learn whether the server accepts "+strings.Join(unknown, " or ")+": the probing handshake ended without the server refusing or completing it.").
			withEvidence("%s", strings.Join(evidence, "; "))
	default:
		return finding("TLS-001", SevMedium, StatusPass,
			"the server refuses handshakes at TLS 1.1 and TLS 1.0.").
			withEvidence("%s", strings.Join(evidence, "; "))
	}
}

// brokenSuite reports whether a suite's cipher is broken outright (RC4,
// 3DES), not merely weaker than the AEAD suites.
func brokenSuite(id uint16) bool {
	name := tls.CipherSuiteName(id)
	return strings.Contains(name, "_RC4_") || strings.Contains(name, "_3DES_")
}

// suiteNames returns the names of the suites ids.
func suiteNames(ids []uint16) []string {
	names := make([]string, 0, len(ids))
	for _, id := range ids {
		names = append(names, tls.CipherSuiteName(id))
	}
	return names
}

// weakSuiteFinding is TLS-002: a TLS 1.2 handshake offering only the suites
// Go lists as insecure must be refused. Each one the server accepts is taken
// off the offer and the handshake tried again, so the finding names every
// such suite the server enables. A server that speaks only TLS 1.3 refuses
// the handshake, which is a pass. The scanner's client may leave some of the
// suites out of its offer (clientOffer); the server's answer then says
// nothing of those, and no pass is given without them.
func weakSuiteFinding(s *Scanner, modern tls.ConnectionState) Finding {
	insecure := suiteIDs(tls.InsecureCipherSuites())
	total := len(insecure)
	evidence := "a modern client negotiates " + tls.CipherSuiteName(modern.CipherSuite)
	offered, err := clientOffer(s.probeConfig(tls.VersionTLS12, tls.VersionTLS12, insecure))
	if err != nil {
		return finding("TLS-002", SevInfo, StatusError,
			"the probe for insecure cipher suites could not be made: the scanner's own TLS client could not build a TLS 1.2 handshake offering them, so the server was not asked."+clientLimits()).
			withEvidence("%s; %v", evidence, err)
	}
	offer := slices.DeleteFunc(slices.Clone(insecure), func(id uint16) bool { return !slices.Contains(offered, id) })
	unoffered := slices.DeleteFunc(slices.Clone(insecure), func(id uint16) bool { return slices.Contains(offered, id) })
	if len(unoffered) > 0 {
		evidence += "; not offered by the scanner's TLS client: " + strings.Join(suiteNames(unoffered), ", ")
	}
	if len(offer) == 0 {
		return finding("TLS-002", SevInfo, StatusError,
			fmt.Sprintf("the probe for insecure cipher suites could not be made: the scanner's own TLS client offers none of the %d suites Go lists as insecure, so the server was not asked.", total)+clientLimits()).
			withEvidence("%s", evidence)
	}

	var accepted []uint16
	var failed error // a probe that taught nothing
	for len(offer) > 0 {
		h := s.tryHandshake(s.probeConfig(tls.VersionTLS12, tls.VersionTLS12, offer))
		if h.err != nil {
			if !h.refused {
				failed = h.err
			}
			break
		}
		suite := h.state.CipherSuite
		if !slices.Contains(offer, suite) {
			break // the client refuses a suite it did not offer; never loop on one
		}
		accepted = append(accepted, suite)
		offer = slices.DeleteFunc(offer, func(id uint16) bool { return id == suite })
	}

	names := suiteNames(accepted)
	broken := slices.ContainsFunc(accepted, brokenSuite)
	switch {
	case broken:
		return finding("TLS-002", SevHigh, StatusFail,
			"the server accepts a broken cipher suite (RC4 or 3DES) at TLS 1.2: "+strings.Join(names, ", ")+".").
			withEvidence("%s; accepted at TLS 1.2: %s", evidence, strings.Join(names, ", ")).
			withRemediation("Restrict the proxy to AEAD cipher suites with forward secrecy (ECDHE with AES-GCM or ChaCha20-Poly1305), or serve TLS 1.3 only.")
	case len(accepted) > 0:
		return finding("TLS-002", SevMedium, StatusWarn,
			"the server still enables cipher suites Go lists as insecure at TLS 1.2: "+strings.Join(names, ", ")+".").
			withEvidence("%s; accepted at TLS 1.2: %s", evidence, strings.Join(names, ", ")).
			withRemediation("Restrict the proxy to AEAD cipher suites with forward secrecy (ECDHE with AES-GCM or ChaCha20-Poly1305), or serve TLS 1.3 only.")
	case failed != nil:
		return finding("TLS-002", SevInfo, StatusError,
			"could not learn whether the server accepts an insecure cipher suite: the probing handshake ended without the server refusing or completing it.").
			withEvidence("%s; %v", evidence, failed)
	case len(unoffered) > 0:
		return finding("TLS-002", SevInfo, StatusError,
			fmt.Sprintf("the server refuses the %d of the %d insecure cipher suites the scanner's TLS client offers, but the client does not offer the other %d, so whether the server accepts those is not known.",
				total-len(unoffered), total, len(unoffered))+clientLimits()).
			withEvidence("%s", evidence)
	default:
		return finding("TLS-002", SevInfo, StatusPass,
			fmt.Sprintf("the server refuses a TLS 1.2 handshake that offers only the %d cipher suites Go lists as insecure.", total)).
			withEvidence("%s", evidence)
	}
}

// certificateFinding is TLS-003, judged at now on the chain the server
// presented: the leaf must be within its validity window for 14 more days,
// and no certificate in the chain may carry an RSA key under 2048 bits or a
// SHA-1 (or MD5) signature. A self-signed root sent along with the chain is
// not judged: its signature verifies nothing.
func certificateFinding(chain []*x509.Certificate, now time.Time) Finding {
	if len(chain) == 0 {
		return finding("TLS-003", SevInfo, StatusError, "the server presented no certificate.")
	}
	leaf := chain[0]
	const day = "2006-01-02"
	var invalid, weak, expiring []string
	switch {
	case now.After(leaf.NotAfter):
		invalid = append(invalid, "it expired on "+leaf.NotAfter.Format(day))
	case now.Before(leaf.NotBefore):
		invalid = append(invalid, "it is not valid until "+leaf.NotBefore.Format(day))
	case now.Add(14 * 24 * time.Hour).After(leaf.NotAfter):
		expiring = append(expiring, "it expires on "+leaf.NotAfter.Format(day)+", within 14 days")
	}
	for i, cert := range chain {
		if i > 0 && bytes.Equal(cert.RawIssuer, cert.RawSubject) {
			continue
		}
		which := "it"
		if i > 0 {
			which = fmt.Sprintf("the intermediate %q", cert.Subject.CommonName)
		}
		if key, ok := cert.PublicKey.(*rsa.PublicKey); ok && key.N.BitLen() < 2048 {
			weak = append(weak, fmt.Sprintf("%s has a %d-bit RSA key", which, key.N.BitLen()))
		}
		if weakSignature(cert.SignatureAlgorithm) {
			weak = append(weak, fmt.Sprintf("%s is signed with %s", which, cert.SignatureAlgorithm))
		}
	}

	evidence := fmt.Sprintf("subject %q, issuer %q, valid %s to %s, %s key, %s signature",
		leaf.Subject.CommonName, leaf.Issuer.CommonName, leaf.NotBefore.Format(day), leaf.NotAfter.Format(day),
		keyDescription(leaf), leaf.SignatureAlgorithm)
	problems := strings.Join(append(append(invalid, weak...), expiring...), "; ")
	switch {
	case len(invalid) > 0:
		return finding("TLS-003", SevHigh, StatusFail, "the server certificate is not valid now: "+problems+".").
			withEvidence("%s", evidence).
			withRemediation("Renew the TLS certificate, and check the clock of the host that issued it.")
	case len(weak) > 0:
		return finding("TLS-003", SevMedium, StatusFail, "the server certificate is weak: "+problems+".").
			withEvidence("%s", evidence).
			withRemediation("Reissue the certificate with an RSA key of 2048 bits or more (or an ECDSA key) and a SHA-256 or stronger signature.")
	case len(expiring) > 0:
		return finding("TLS-003", SevMedium, StatusWarn, "the server certificate "+strings.TrimPrefix(problems, "it ")+".").
			withEvidence("%s", evidence).
			withRemediation("Renew the certificate before it expires.")
	default:
		return finding("TLS-003", SevInfo, StatusPass,
			"the server certificate is within its validity window, with a strong key and signature.").
			withEvidence("%s", evidence)
	}
}

// weakSignature reports whether alg hashes with SHA-1 or MD5 (or MD2).
func weakSignature(alg x509.SignatureAlgorithm) bool {
	switch alg {
	case x509.MD2WithRSA, x509.MD5WithRSA, x509.SHA1WithRSA, x509.DSAWithSHA1, x509.ECDSAWithSHA1:
		return true
	}
	return false
}

// keyDescription names a certificate's key type and size, for evidence.
func keyDescription(cert *x509.Certificate) string {
	switch key := cert.PublicKey.(type) {
	case *rsa.PublicKey:
		return fmt.Sprintf("%d-bit RSA", key.N.BitLen())
	case *ecdsa.PublicKey:
		return "ECDSA " + key.Curve.Params().Name
	}
	return cert.PublicKeyAlgorithm.String()
}

// trustFinding is TLS-004: the certificate chains to a trusted root and names
// the target URL's host. Without -insecure the handshake checked that. With
// -insecure it did not, so the scanner verifies the chain itself; a chain
// that does not verify is a WARN, since -insecure says the operator knows,
// which -expect tls turns into a FAIL.
func trustFinding(s *Scanner, st tls.ConnectionState) Finding {
	host := s.base.Hostname()
	if !s.insecure {
		return finding("TLS-004", SevMedium, StatusPass,
			fmt.Sprintf("the handshake verified the certificate chain against the system roots, for the name %s.", host))
	}
	if len(st.PeerCertificates) == 0 {
		return finding("TLS-004", SevInfo, StatusError, "the server presented no certificate.")
	}
	intermediates := x509.NewCertPool()
	for _, cert := range st.PeerCertificates[1:] {
		intermediates.AddCert(cert)
	}
	_, err := st.PeerCertificates[0].Verify(x509.VerifyOptions{
		DNSName:       host,
		Intermediates: intermediates,
		Roots:         verifyRoots,
	})
	var rootsErr x509.SystemRootsError
	switch {
	case err == nil:
		return finding("TLS-004", SevMedium, StatusPass,
			fmt.Sprintf("-insecure skipped verification in the handshake; the scanner verified the chain itself: it leads to a system root and names %s.", host))
	case errors.As(err, &rootsErr):
		return finding("TLS-004", SevInfo, StatusError,
			"-insecure skipped verification in the handshake, and the scanner could not load the system roots to verify the chain itself: "+err.Error())
	default:
		return finding("TLS-004", SevMedium, StatusWarn,
			fmt.Sprintf("the certificate does not verify for %s (%s); a browser refuses it. -insecure accepted it for this scan.", host, trustProblem(err))).
			withEvidence("%v", err).
			withRemediation("Serve a certificate from a CA your clients trust (e.g. Let's Encrypt) that names the host they use; for a private CA, install its root on the scanning host so the scanner can verify the chain.").
			expecting("tls")
	}
}

// trustProblem says in words why x509 verification failed.
func trustProblem(err error) string {
	var hostErr x509.HostnameError
	var authorityErr x509.UnknownAuthorityError
	var invalidErr x509.CertificateInvalidError
	switch {
	case errors.As(err, &hostErr):
		return "it names other hosts"
	case errors.As(err, &authorityErr):
		return "it does not lead to a trusted root: self-signed, or from a private CA"
	case errors.As(err, &invalidErr) && invalidErr.Reason == x509.Expired:
		return "it is expired or not yet valid"
	default:
		return "the chain does not verify"
	}
}
