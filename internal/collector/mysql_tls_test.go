package collector

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// testCA issues certificates for the fake MySQL server.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newTestCA(t *testing.T, name string) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// issue returns a server certificate for the given names (IP addresses or
// DNS names).
func (ca *testCA) issue(t *testing.T, names ...string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: names[0]},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, name := range names {
		if ip := net.ParseIP(name); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, name)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// writeFile writes data to a new file in dir and returns its path.
func writeFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// MySQL protocol constants the fake server needs.
const (
	mysqlCapLongPassword  = 1 << 0
	mysqlCapProtocol41    = 1 << 9
	mysqlCapSSL           = 1 << 11
	mysqlCapTransactions  = 1 << 13
	mysqlCapSecureConn    = 1 << 15
	mysqlCapPluginAuth    = 1 << 19
	mysqlSSLRequestLength = 32
)

func writeMysqlPacket(w io.Writer, seq byte, payload []byte) error {
	header := []byte{byte(len(payload)), byte(len(payload) >> 8), byte(len(payload) >> 16), seq}
	_, err := w.Write(append(header, payload...))
	return err
}

func readMysqlPacket(r io.Reader) (byte, []byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, err
	}
	payload := make([]byte, int(header[0])|int(header[1])<<8|int(header[2])<<16)
	_, err := io.ReadFull(r, payload)
	return header[3], payload, err
}

// mysqlHandshake is a protocol 10 greeting offering mysql_native_password,
// and TLS when offerTLS is set.
func mysqlHandshake(offerTLS bool) []byte {
	caps := uint32(mysqlCapLongPassword | mysqlCapProtocol41 | mysqlCapTransactions | mysqlCapSecureConn | mysqlCapPluginAuth)
	if offerTLS {
		caps |= mysqlCapSSL
	}
	scramble := []byte("ABCDEFGHIJKLMNOPQRST")
	hs := []byte{10}
	hs = append(hs, "8.0.36-fake\x00"...)
	hs = binary.LittleEndian.AppendUint32(hs, 42) // connection id
	hs = append(hs, scramble[:8]...)
	hs = append(hs, 0)
	hs = binary.LittleEndian.AppendUint16(hs, uint16(caps))
	hs = append(hs, 0x21)                        // character set
	hs = binary.LittleEndian.AppendUint16(hs, 2) // status: autocommit
	hs = binary.LittleEndian.AppendUint16(hs, uint16(caps>>16))
	hs = append(hs, 21)                  // auth data length
	hs = append(hs, make([]byte, 10)...) // reserved
	hs = append(hs, scramble[8:]...)
	hs = append(hs, 0)
	return append(hs, "mysql_native_password\x00"...)
}

var mysqlOK = []byte{0, 0, 0, 2, 0, 0, 0}

// fakeMysqlTLSServer speaks enough of MySQL's connection phase, TLS included,
// for the driver to connect: it accepts any credentials and answers every
// query with OK. It records what the client sent where.
type fakeMysqlTLSServer struct {
	ln       net.Listener
	offerTLS bool
	tls      *tls.Config

	mu sync.Mutex
	// plaintextLogin and tlsLogin count the login packets (which carry the
	// user name and the password's proof) received in the clear and over TLS.
	plaintextLogin, tlsLogin int
	wg                       sync.WaitGroup
}

func startFakeMysqlTLS(t *testing.T, offerTLS bool, cert tls.Certificate) *fakeMysqlTLSServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeMysqlTLSServer{
		ln:       ln,
		offerTLS: offerTLS,
		tls:      &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				defer func() { _ = conn.Close() }()
				_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
				s.serve(conn)
			}()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		s.wg.Wait()
	})
	return s
}

func (s *fakeMysqlTLSServer) serve(conn net.Conn) {
	if writeMysqlPacket(conn, 0, mysqlHandshake(s.offerTLS)) != nil {
		return
	}
	var rw io.ReadWriter = conn
	seq, login, err := readMysqlPacket(rw)
	if err != nil {
		return
	}
	encrypted := false
	if len(login) == mysqlSSLRequestLength && binary.LittleEndian.Uint32(login)&mysqlCapSSL != 0 {
		tlsConn := tls.Server(conn, s.tls)
		if tlsConn.Handshake() != nil {
			return
		}
		rw, encrypted = tlsConn, true
		if seq, login, err = readMysqlPacket(rw); err != nil {
			return
		}
	}
	s.mu.Lock()
	if encrypted {
		s.tlsLogin++
	} else {
		s.plaintextLogin++
	}
	s.mu.Unlock()
	if !bytes.Contains(login, []byte("kula\x00")) || writeMysqlPacket(rw, seq+1, mysqlOK) != nil {
		return
	}
	for {
		seq, cmd, err := readMysqlPacket(rw)
		if err != nil || len(cmd) == 0 || cmd[0] != 3 { // COM_QUERY; COM_QUIT or anything else ends it
			return
		}
		if writeMysqlPacket(rw, seq+1, mysqlOK) != nil {
			return
		}
	}
}

func (s *fakeMysqlTLSServer) port() int { return s.ln.Addr().(*net.TCPAddr).Port }

func (s *fakeMysqlTLSServer) logins() (plaintext, encrypted int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.plaintextLogin, s.tlsLogin
}

// connectMysql opens one connection the way the collector does, through its
// connector, and closes it again.
func connectMysql(t *testing.T, port int, sslmode, sslrootcert string) error {
	t.Helper()
	mc := newMysqlCollector("127.0.0.1", port, "kula", "secret", "", sslmode, sslrootcert, false, time.Second)
	if mc.session.connector == nil {
		t.Fatal("no connector")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := mc.session.connector.Connect(ctx)
	if err == nil {
		_ = conn.Close()
	}
	return err
}

// TestMysqlTLSModes connects through the collector's own driver settings to a
// fake server over TLS. The verify modes accept only a certificate that
// chains to the configured CA (and, for verify-full, names the host); require
// accepts any certificate, but checks the chain when given a CA, as libpq
// does. A refused certificate, or a server that does not offer TLS, ends the
// connection before the login packet, which carries the user and the
// password's proof, is sent.
func TestMysqlTLSModes(t *testing.T) {
	dir := t.TempDir()
	ca, otherCA := newTestCA(t, "kula test CA"), newTestCA(t, "another CA")
	caFile := writeFile(t, dir, "ca.pem", ca.pem)
	otherCAFile := writeFile(t, dir, "other-ca.pem", otherCA.pem)
	notPEM := writeFile(t, dir, "not.pem", []byte("not a certificate"))

	forHost := ca.issue(t, "127.0.0.1")                         // names the address the collector dials
	forOtherHost := ca.issue(t, "db.example.test", "127.0.0.2") // a valid certificate for other names
	untrusted := otherCA.issue(t, "127.0.0.1")                  // names the host, from a CA nobody configured

	for _, tc := range []struct {
		name             string
		cert             tls.Certificate
		noTLS            bool
		sslmode, rootCA  string
		wantErr          string // empty: connects
		wantPlaintextLog bool
	}{
		{name: "disable", cert: forHost, sslmode: "disable", wantPlaintextLog: true},
		{name: "require: any certificate", cert: untrusted, sslmode: "require"},
		{name: "require with a CA: the chain is checked", cert: untrusted, sslmode: "require", rootCA: caFile, wantErr: "unknown authority"},
		{name: "require with a CA: the name is not", cert: forOtherHost, sslmode: "require", rootCA: caFile},
		{name: "verify-ca", cert: forOtherHost, sslmode: "verify-ca", rootCA: caFile},
		{name: "verify-ca: another CA", cert: untrusted, sslmode: "verify-ca", rootCA: caFile, wantErr: "unknown authority"},
		{name: "verify-full", cert: forHost, sslmode: "verify-full", rootCA: caFile},
		{name: "verify-full: another name", cert: forOtherHost, sslmode: "verify-full", rootCA: caFile, wantErr: "not 127.0.0.1"},
		{name: "verify-full: another CA", cert: untrusted, sslmode: "verify-full", rootCA: caFile, wantErr: "unknown authority"},
		{name: "verify-full: the system CAs", cert: forHost, sslmode: "verify-full", wantErr: "unknown authority"},
		{name: "verify-full: the CA file is another CA's", cert: forHost, sslmode: "verify-full", rootCA: otherCAFile, wantErr: "unknown authority"},
		{name: "a CA file without a certificate", cert: forHost, sslmode: "verify-full", rootCA: notPEM, wantErr: "holds no PEM certificate"},
		{name: "a missing CA file", cert: forHost, sslmode: "verify-full", rootCA: filepath.Join(dir, "missing.pem"), wantErr: "no such file"},
		{name: "a server without TLS", cert: forHost, noTLS: true, sslmode: "verify-full", rootCA: caFile, wantErr: "TLS requested but server does not support TLS"},
		{name: "require: a server without TLS", cert: forHost, noTLS: true, sslmode: "require", wantErr: "TLS requested but server does not support TLS"},
		{name: "an unknown mode", cert: forHost, sslmode: "prefer", wantErr: "is not one of"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := startFakeMysqlTLS(t, !tc.noTLS, tc.cert)
			err := connectMysql(t, srv.port(), tc.sslmode, tc.rootCA)
			plaintext, encrypted := srv.logins()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("connect: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("connect: %v; want an error containing %q", err, tc.wantErr)
			case tc.wantErr != "" && plaintext+encrypted != 0:
				t.Fatalf("a refused connection sent its login (%d in the clear, %d over TLS)", plaintext, encrypted)
			case tc.wantErr == "" && tc.wantPlaintextLog && (plaintext != 1 || encrypted != 0):
				t.Fatalf("logins: %d in the clear, %d over TLS; want one in the clear", plaintext, encrypted)
			case tc.wantErr == "" && !tc.wantPlaintextLog && (plaintext != 0 || encrypted != 1):
				t.Fatalf("logins: %d in the clear, %d over TLS; want one over TLS", plaintext, encrypted)
			}
		})
	}
}

// TestMysqlTLSRereadsTheCAFile: the CA file is read for each connection, so
// replacing it takes effect without a restart.
func TestMysqlTLSRereadsTheCAFile(t *testing.T) {
	dir := t.TempDir()
	ca, otherCA := newTestCA(t, "kula test CA"), newTestCA(t, "another CA")
	caFile := writeFile(t, dir, "ca.pem", otherCA.pem)
	srv := startFakeMysqlTLS(t, true, ca.issue(t, "127.0.0.1"))

	mc := newMysqlCollector("127.0.0.1", srv.port(), "kula", "secret", "", "verify-full", caFile, false, time.Second)
	connect := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		conn, err := mc.session.connector.Connect(ctx)
		if err == nil {
			_ = conn.Close()
		}
		return err
	}
	if err := connect(); err == nil || !strings.Contains(err.Error(), "unknown authority") {
		t.Fatalf("connect with the wrong CA: %v", err)
	}
	writeFile(t, dir, "ca.pem", ca.pem)
	if err := connect(); err != nil {
		t.Fatalf("connect after the CA file was replaced: %v", err)
	}
}
