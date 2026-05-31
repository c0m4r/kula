package main

import (
	"crypto/tls"
	"fmt"
	"net"
	"strings"
	"time"
)

// TLS posture checks (https targets only): negotiated protocol version, cipher
// strength, and certificate expiry. These are read-only and non-aggressive.

func tlsChecks() []check {
	return []check{{id: "TLS", category: "tls", run: runTLSChecks}}
}

// weakCiphers are TLS 1.2 cipher suites Go still supports for compatibility but
// that are considered weak (RC4, 3DES, CBC-SHA without AEAD).
func isWeakCipher(id uint16) bool {
	for _, w := range tls.InsecureCipherSuites() {
		if w.ID == id {
			return true
		}
	}
	return false
}

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
		return []Finding{finding("TLS-001", "tls", "TLS posture", SevInfo, StatusSkip,
			"target is plain HTTP; no TLS layer to inspect.")}
	}

	host := s.base.Host
	if !strings.Contains(host, ":") {
		host += ":443"
	}
	dialer := &net.Dialer{Timeout: s.timeout}
	// Verify the certificate chain unless -insecure; either way we can read the
	// negotiated parameters and the leaf certificate.
	// nosemgrep: missing-ssl-minversion
	conn, err := tls.DialWithDialer(dialer, "tcp", host, &tls.Config{InsecureSkipVerify: s.insecure}) //nolint:gosec // -insecure is an explicit opt-in
	if err != nil {
		return []Finding{finding("TLS-001", "tls", "TLS handshake", SevMedium, StatusWarn,
			"could not complete a TLS handshake with the target.").withEvidence("%v", err)}
	}
	defer func() { _ = conn.Close() }()
	st := conn.ConnectionState()

	var out []Finding

	// Protocol version.
	if st.Version >= tls.VersionTLS12 {
		out = append(out, finding("TLS-001", "tls", "TLS protocol version", SevMedium, StatusPass,
			"the connection negotiated a modern TLS version.").withEvidence("%s", tlsVersionName(st.Version)))
	} else {
		out = append(out, finding("TLS-001", "tls", "TLS protocol version", SevHigh, StatusFail,
			"the server negotiated a deprecated TLS version.").withEvidence("%s", tlsVersionName(st.Version)).
			withRemediation("Terminate TLS with a proxy configured for TLS 1.2+ only."))
	}

	// Cipher strength (only meaningful for TLS 1.2; 1.3 suites are all strong).
	if st.Version == tls.VersionTLS12 && isWeakCipher(st.CipherSuite) {
		out = append(out, finding("TLS-002", "tls", "TLS cipher strength", SevMedium, StatusWarn,
			"the negotiated TLS 1.2 cipher suite is on Go's insecure list.").
			withEvidence("%s", tls.CipherSuiteName(st.CipherSuite)).
			withRemediation("Restrict the proxy to AEAD cipher suites (or prefer TLS 1.3)."))
	} else {
		out = append(out, finding("TLS-002", "tls", "TLS cipher strength", SevInfo, StatusPass,
			"the negotiated cipher suite is strong.").withEvidence("%s", tls.CipherSuiteName(st.CipherSuite)))
	}

	// Certificate expiry.
	if len(st.PeerCertificates) > 0 {
		leaf := st.PeerCertificates[0]
		now := time.Now()
		switch {
		case now.After(leaf.NotAfter):
			out = append(out, finding("TLS-003", "tls", "Certificate validity", SevHigh, StatusFail,
				"the server certificate is expired.").withEvidence("expired %s", leaf.NotAfter.Format("2006-01-02")).
				withRemediation("Renew the TLS certificate."))
		case now.Add(14 * 24 * time.Hour).After(leaf.NotAfter):
			out = append(out, finding("TLS-003", "tls", "Certificate validity", SevMedium, StatusWarn,
				"the server certificate expires within 14 days.").withEvidence("expires %s", leaf.NotAfter.Format("2006-01-02")).
				withRemediation("Renew the certificate before it expires."))
		default:
			out = append(out, finding("TLS-003", "tls", "Certificate validity", SevInfo, StatusPass,
				"the server certificate is within its validity window.").withEvidence("expires %s", leaf.NotAfter.Format("2006-01-02")))
		}
	}

	return out
}
