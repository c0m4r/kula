package collector

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
)

// mysqlUsesTLS reports whether an sslmode asks for TLS.
func mysqlUsesTLS(sslmode string) bool {
	return sslmode != "" && sslmode != "disable"
}

// mysqlTLSConfig builds the TLS configuration of one MySQL connection from the
// sslmode and sslrootcert options, which are named and behave as PostgreSQL's
// (libpq's), mapping to MySQL's own --ssl-mode values:
//
//   - require (REQUIRED): TLS without checking the certificate. It keeps the
//     traffic from eavesdroppers, not from whoever answers on the port, who
//     then receives the password. With an sslrootcert it checks the chain like
//     verify-ca, as libpq does.
//   - verify-ca (VERIFY_CA): the certificate must chain to sslrootcert. Its
//     name is not checked, so any certificate that CA issued is accepted.
//   - verify-full (VERIFY_IDENTITY): the certificate must chain to
//     sslrootcert, or to the system CAs without one, and name host.
//
// The CA file is read for each connection, so one rewritten in place needs no
// restart (the sandbox grants that file, not a new one renamed over it).
// A server that does not offer TLS is refused, never used in plaintext.
func mysqlTLSConfig(sslmode, sslrootcert, host string) (*tls.Config, error) {
	var roots *x509.CertPool
	if sslrootcert != "" {
		pem, err := os.ReadFile(sslrootcert)
		if err != nil {
			return nil, fmt.Errorf("reading sslrootcert: %w", err)
		}
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("sslrootcert %s holds no PEM certificate", sslrootcert)
		}
	}
	// InsecureSkipVerify is set below only where the mode asks for it: verify-ca
	// checks the chain itself in VerifyConnection, and require without a CA is
	// the documented opt-in to an unchecked certificate (the default is disable).
	// nosemgrep: bypass-tls-verification
	cfg := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	switch {
	case sslmode == "verify-full":
		cfg.RootCAs = roots
	case sslmode == "verify-ca" || (sslmode == "require" && roots != nil):
		// crypto/tls checks the chain only together with the name, so the
		// chain is checked here instead.
		cfg.InsecureSkipVerify = true // the chain is verified by VerifyConnection below
		cfg.VerifyConnection = func(cs tls.ConnectionState) error { return verifyMysqlChain(cs, roots) }
	case sslmode == "require":
		cfg.InsecureSkipVerify = true // require is the explicit opt-in to an unchecked certificate
	default:
		return nil, fmt.Errorf("sslmode %q is not one of disable, require, verify-ca, verify-full", sslmode)
	}
	return cfg, nil
}

// verifyMysqlChain checks that the server's certificate chains to roots (the
// system CAs when nil), without checking the name it was issued for.
func verifyMysqlChain(cs tls.ConnectionState, roots *x509.CertPool) error {
	if len(cs.PeerCertificates) == 0 {
		return errors.New("the server sent no certificate")
	}
	opts := x509.VerifyOptions{Roots: roots, Intermediates: x509.NewCertPool()}
	for _, cert := range cs.PeerCertificates[1:] {
		opts.Intermediates.AddCert(cert)
	}
	_, err := cs.PeerCertificates[0].Verify(opts)
	return err
}
