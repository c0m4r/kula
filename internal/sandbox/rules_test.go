package sandbox

import (
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"kula/internal/config"
)

// useResolverFixtures points the hosts file and nsswitch.conf the rules read
// at fixtures, so the tests do not depend on the host's.
func useResolverFixtures(t *testing.T, hosts, nsswitch string) {
	t.Helper()
	dir := t.TempDir()
	hostsFile, nsswitchFile := filepath.Join(dir, "hosts"), filepath.Join(dir, "nsswitch.conf")
	if err := os.WriteFile(hostsFile, []byte(hosts), 0o600); err != nil {
		t.Fatal(err)
	}
	if nsswitch != "" {
		if err := os.WriteFile(nsswitchFile, []byte(nsswitch), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	oldHosts, oldNsswitch := hostsPath, nsswitchPath
	hostsPath, nsswitchPath = hostsFile, nsswitchFile
	t.Cleanup(func() { hostsPath, nsswitchPath = oldHosts, oldNsswitch })
}

const fixtureHosts = "127.0.0.1 localhost\n::1 localhost ip6-localhost\n10.0.0.9 db.lan # a database\n"

// grants is what a ruleset allows, as the tests compare it.
type grants struct {
	tcp      []uint16
	sockets  []string
	dns      []string
	abstract []string
}

func grantsOf(rs *ruleset) grants {
	return grants{rs.tcpConnect, rs.sockets, rs.dnsNames, rs.abstractUsers}
}

func (g grants) equal(o grants) bool {
	return slices.Equal(g.tcp, o.tcp) && slices.Equal(g.sockets, o.sockets) &&
		slices.Equal(g.dns, o.dns) && slices.Equal(g.abstract, o.abstract)
}

// TestPostgresGrants pins what the sandbox lets lib/pq reach for each way
// of naming the server: TCP to the port, DNS only for a name the hosts file
// does not answer, the socket directory for a path, and nothing for a host
// that cannot hold a socket but an error naming the setting.
func TestPostgresGrants(t *testing.T) {
	useResolverFixtures(t, fixtureHosts, "hosts: files dns\n")
	t.Setenv("PGHOSTADDR", "")
	t.Setenv("PGPORT", "")

	dir := t.TempDir()
	sockDir := filepath.Join(dir, "postgresql")
	if err := os.Mkdir(sockDir, 0o750); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name    string
		host    string
		port    int
		env     map[string]string
		want    grants
		wantErr string
	}{
		{name: "localhost", host: "localhost", port: 5432, want: grants{tcp: []uint16{5432}}},
		{name: "a name in the hosts file", host: "DB.lan.", port: 5432, want: grants{tcp: []uint16{5432}}},
		{name: "an IP address", host: "10.1.2.3", port: 6432, want: grants{tcp: []uint16{6432}}},
		{name: "a DNS name", host: "pg.example.com", port: 5433, want: grants{tcp: []uint16{5433}, dns: []string{"pg.example.com"}}},
		{name: "a host list", host: "pg1.example.com,db.lan,", port: 5432, want: grants{tcp: []uint16{5432}, dns: []string{"pg1.example.com"}}},
		{name: "PGHOSTADDR replaces the host", host: "pg.example.com", port: 5432, env: map[string]string{"PGHOSTADDR": "10.0.0.1"}, want: grants{tcp: []uint16{5432}}},
		{name: "port 0 takes PGPORT", host: "localhost", env: map[string]string{"PGPORT": "5433,,5434"}, want: grants{tcp: []uint16{5433, 5432, 5434}}},
		{name: "a socket directory", host: sockDir, want: grants{sockets: []string{sockDir + "/"}}},
		{name: "a socket directory with a port", host: sockDir, port: 5433, want: grants{sockets: []string{sockDir + "/"}}},
		{name: "a socket directory not created yet", host: filepath.Join(dir, "later"), want: grants{sockets: []string{filepath.Join(dir, "later") + "/"}}},
		{name: "an abstract socket", host: "@pg", want: grants{abstract: []string{"postgres"}}},
		{name: "a regular file", host: file, wantErr: "applications.postgres.host: "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			rs := &ruleset{}
			rs.addPostgres(config.PostgresConfig{Enabled: true, Host: tc.host, Port: tc.port, SSLMode: "disable"})
			if got := grantsOf(rs); !got.equal(tc.want) {
				t.Errorf("grants = %+v, want %+v", got, tc.want)
			}
			// Never rs.restrict here: it would confine the test process.
			err := rs.err()
			if tc.wantErr == "" && err != nil {
				t.Errorf("err() = %v, want none", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Errorf("err() = %v, want an error naming %q", err, tc.wantErr)
			}
		})
	}
}

// TestMysqlGrants: a socket path with port 0, TCP otherwise, DNS for a name.
func TestMysqlGrants(t *testing.T) {
	useResolverFixtures(t, fixtureHosts, "")
	dir := t.TempDir()
	sockDir := filepath.Join(dir, "mysqld")
	if err := os.Mkdir(sockDir, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		host string
		port int
		want grants
	}{
		{"loopback", "127.0.0.1", 3306, grants{tcp: []uint16{3306}}},
		{"localhost", "localhost", 3306, grants{tcp: []uint16{3306}}},
		{"a DNS name", "mysql.example.com", 3307, grants{tcp: []uint16{3307}, dns: []string{"mysql.example.com"}}},
		{"a socket in a directory of its own", filepath.Join(sockDir, "mysqld.sock"), 0, grants{sockets: []string{sockDir + "/"}}},
		{"an abstract socket", "@mysql", 0, grants{abstract: []string{"mysql"}}},
	} {
		rs := &ruleset{}
		rs.addMysql(config.MysqlConfig{Enabled: true, Host: tc.host, Port: tc.port})
		if got := grantsOf(rs); !got.equal(tc.want) || len(rs.errs) > 0 {
			t.Errorf("%s: grants = %+v, errors %v; want %+v", tc.name, got, rs.errs, tc.want)
		}
	}

	// The sslrootcert is granted as the file it names; a directory there
	// would open its whole tree, so it is an error naming the setting.
	ca := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(ca, []byte("ca"), 0o600); err != nil {
		t.Fatal(err)
	}
	for path, wantErr := range map[string]bool{ca: false, filepath.Join(dir, "missing.pem"): false, sockDir: true} {
		rs := &ruleset{}
		rs.addMysql(config.MysqlConfig{Enabled: true, Host: "127.0.0.1", Port: 3306, SSLMode: "verify-ca", SSLRootCert: path})
		if err := rs.err(); (err != nil) != wantErr || (wantErr && !strings.Contains(err.Error(), "applications.mysql.sslrootcert")) {
			t.Errorf("sslrootcert %s: err() = %v, want an error: %v", path, err, wantErr)
		}
	}
}

// TestUnixSocketGrants: a socket in a directory of its own is granted through
// the directory, one in a shared or world-writable directory as the file.
func TestUnixSocketGrants(t *testing.T) {
	dir := t.TempDir()
	own := filepath.Join(dir, "podman")
	open := filepath.Join(dir, "open")
	for _, d := range []string{own, open} {
		if err := os.Mkdir(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(open, 0o1777); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", filepath.Join(own, "podman.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	link := filepath.Join(dir, "link.sock")
	if err := os.Symlink(filepath.Join(own, "podman.sock"), link); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		path string
		want string
	}{
		{filepath.Join(own, "podman.sock"), own + "/"},
		{link, own + "/"}, // through the symlink, to where the socket is
		{filepath.Join(open, "mysql.sock"), filepath.Join(open, "mysql.sock")},
		{"/run/docker.sock", "/run/docker.sock"},
		{"/tmp/.s.PGSQL.5432", "/tmp/.s.PGSQL.5432"},
		{"/run/user/1000/docker.sock", "/run/user/1000/docker.sock"},
		{"/run/systemd/private", "/run/systemd/private"},
	} {
		rs := &ruleset{}
		rs.addUnixSocket("key", "test", tc.path, false)
		if len(rs.errs) > 0 || !slices.Equal(rs.sockets, []string{tc.want}) {
			t.Errorf("addUnixSocket(%s) grants %v, errors %v; want [%s]", tc.path, rs.sockets, rs.errs, tc.want)
		}
	}

	for dir, want := range map[string]bool{
		"/": false, "/run": false, "/var/run": false, "/tmp": false, "/run/user": false,
		"/run/user/1000": false, "/home/alice": false, "/run/dbus": false, "/run/systemd": false,
		own: true, open: false, filepath.Join(dir, "missing"): true,
	} {
		if got := dedicatedSocketDir(dir); got != want {
			t.Errorf("dedicatedSocketDir(%s) = %v, want %v", dir, got, want)
		}
	}
}

// TestDNSGrant: name resolution is allowed only when a target is named by a
// host name the resolver cannot answer from the hosts file, and the hosts
// file answers only when nsswitch.conf has it consulted.
func TestDNSGrant(t *testing.T) {
	for _, tc := range []struct {
		name     string
		nsswitch string
		urls     []string
		wantDNS  []string
	}{
		{"IP literals", "", []string{"http://127.0.0.1/status", "https://[::1]:8443/x"}, nil},
		{"names the hosts file lists", "hosts: files dns\n", []string{"http://localhost/status", "http://db.lan:8080/"}, nil},
		{"systemd-resolved order", "hosts: mymachines resolve [!UNAVAIL=return] files myhostname dns\n", []string{"http://localhost/"}, nil},
		{"DNS before files", "hosts: dns files\n", []string{"http://localhost/"}, nil},
		{"DNS only", "hosts: dns\n", []string{"http://localhost/"}, []string{"localhost"}},
		{"a DNS name", "", []string{"http://status.example.com/", "http://STATUS.example.com./"}, []string{"status.example.com"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useResolverFixtures(t, fixtureHosts, tc.nsswitch)
			rs := &ruleset{}
			for _, u := range tc.urls {
				rs.addURL("nginx", u)
			}
			rs.addDNS()
			if !slices.Equal(rs.dnsNames, tc.wantDNS) {
				t.Fatalf("DNS names = %v, want %v", rs.dnsNames, tc.wantDNS)
			}
			hasDNSRule := slices.ContainsFunc(rs.info, func(s string) bool { return strings.HasPrefix(s, "dns:") })
			if hasDNSRule != (len(tc.wantDNS) > 0) {
				t.Errorf("DNS granted = %v (info %v), want %v", hasDNSRule, rs.info, len(tc.wantDNS) > 0)
			}
		})
	}
}

func TestHostsFromFiles(t *testing.T) {
	dir := t.TempDir()
	for content, want := range map[string]bool{
		"":                                       true,
		"passwd: files\n":                        true,
		"hosts:\n":                               true,
		"hosts: files dns\n":                     true,
		"hosts: dns\n":                           false,
		"hosts: dns # files\n":                   false,
		"hosts: resolve [!UNAVAIL=return] dns\n": false,
		"hosts: dns\nhosts: files\n":             true,
	} {
		path := filepath.Join(dir, "nsswitch.conf")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := hostsFromFiles(path); got != want {
			t.Errorf("hostsFromFiles(%q) = %v, want %v", content, got, want)
		}
	}
	if !hostsFromFiles(filepath.Join(dir, "missing")) {
		t.Error("hostsFromFiles(missing) = false, want true (Go's default order)")
	}
}
