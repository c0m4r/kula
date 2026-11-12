// Package sandbox implements Landlock-based process sandboxing.
//
// After Enforce, `kula serve` is restricted to what it needs:
//   - /proc, /sys: read-only (system metrics collection)
//   - utmp and systemd-logind sessions: read-only (logged-in user count)
//   - config file: read-only
//   - storage directory: read-write
//   - web port: TCP bind only (or the web.unix_socket listener)
//   - the TCP ports and pathname Unix sockets the enabled application
//     collectors and the Ollama proxy connect to, and DNS only when one of
//     them is named by a host name the hosts file does not answer
//
// Every other UDP datagram and Unix socket connect is denied, and signals and
// abstract Unix sockets cannot reach processes outside the sandbox.
// EnforceTUI gives `kula tui` a ruleset of its own: the same reads and
// connections, the storage directory read-only, no writes and no listeners.
//
// Both request the newest Landlock ABI go-landlock knows and use BestEffort
// to degrade gracefully: an older kernel enforces the part of the ruleset it
// supports, and a kernel without Landlock (pre-5.13, or disabled) leaves the
// process unconfined with a log line. The sandbox is a hardening layer, not a
// requirement: a grant the config asks for that cannot be applied is logged
// and left out, and a kernel that refuses the ruleset leaves the process
// unconfined, with a warning from the caller.
package sandbox

import (
	"crypto/x509"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"kula/internal/collector"
	"kula/internal/config"

	"github.com/landlock-lsm/go-landlock/landlock"
	llsyscall "github.com/landlock-lsm/go-landlock/landlock/syscall"
)

// requested is the ruleset Kula asks the kernel for: every access right of
// the newest Landlock ABI go-landlock knows. V11 restricts files, TCP bind and
// connect (ABI 4+), signals and abstract Unix sockets (ABI 6+), connects to
// pathname Unix sockets (ABI 9+) and UDP (ABI 10+). BestEffort lowers it to
// what the running kernel supports. A go-landlock upgrade that knows a newer
// ABI does not raise this by itself: a new ABI restricts more operations, so
// run every collector and listener under it before moving.
var requested = landlock.V11

// Files Go's resolver reads; variables so tests can point them at fixtures.
var (
	hostsPath      = "/etc/hosts"
	resolvConfPath = "/etc/resolv.conf"
	nsswitchPath   = "/etc/nsswitch.conf"
)

// Access rights spelled out where the canned helpers grant more.
const (
	accessResolveUnix = landlock.AccessFSSet(llsyscall.AccessFSResolveUnix)
	// A Unix socket listener creates its socket file and removes a stale
	// one; closing it unlinks the file.
	accessSocketListener = landlock.AccessFSSet(llsyscall.AccessFSMakeSock | llsyscall.AccessFSRemoveFile)
	// Reading files and listing directories, without executing anything.
	accessRead = landlock.AccessFSSet(llsyscall.AccessFSReadFile | llsyscall.AccessFSReadDir)
	// Bubble Tea opens the controlling terminal when standard input is not
	// one, and puts it in raw mode with an ioctl.
	accessTerminal = landlock.AccessFSSet(llsyscall.AccessFSReadFile | llsyscall.AccessFSIoctlDev)
)

// ruleset gathers the grants of one process, and what its log line and the
// tests read back.
type ruleset struct {
	abi    int   // the kernel's Landlock ABI, 0 without Landlock
	abiErr error // why the kernel reports none
	rules  []landlock.Rule
	errs   []error // grants that cannot be applied, by config setting
	info   []string

	tcpConnect []uint16
	// sockets lists the Unix socket grants: a socket's directory (with a
	// trailing slash), or the socket file itself.
	sockets []string
	// dnsNames are the configured host names the hosts file does not
	// answer.
	dnsNames []string
	// abstractUsers name the targets reached through an abstract Unix
	// socket, which keep that scope open.
	abstractUsers []string

	hosts map[string]bool // names the hosts file answers, read on first use
}

func newRuleset() *ruleset {
	rs := &ruleset{}
	rs.abi, rs.abiErr = llsyscall.LandlockGetABIVersion()
	if rs.abiErr != nil || rs.abi < 0 {
		rs.abi = 0
	}
	return rs
}

func (rs *ruleset) add(rules ...landlock.Rule) {
	rs.rules = append(rs.rules, rules...)
}

// fail records a grant the config asks for that cannot be applied; key names
// the setting.
func (rs *ruleset) fail(key string, err error) {
	rs.errs = append(rs.errs, fmt.Errorf("%s: %w", key, err))
}

// addSystemReads grants the reads every Kula process needs.
func (rs *ruleset) addSystemReads() {
	rs.add(
		// System metrics
		landlock.RODirs("/proc"),
		landlock.RODirs("/sys").IgnoreIfMissing(),

		// Name resolution: Go's resolver reads these for every lookup,
		// including one that /etc/hosts answers.
		landlock.ROFiles(hostsPath).IgnoreIfMissing(),
		landlock.ROFiles(resolvConfPath).IgnoreIfMissing(),
		landlock.ROFiles(nsswitchPath).IgnoreIfMissing(),

		// Logged-in user count: utmp, or systemd-logind's session files
		// where a distribution no longer keeps utmp.
		landlock.ROFiles("/run/utmp").IgnoreIfMissing(),
		landlock.ROFiles("/var/run/utmp").IgnoreIfMissing(),
		landlock.RODirs("/run/systemd/sessions").IgnoreIfMissing(),
	)
}

// addReadOnly allows reading the file at path, or with dirOK the directory
// tree, which key names. A path that does not exist is skipped: a file that
// appears later stays unreadable until Kula restarts. The grant follows the
// file, so one renamed over it needs a restart too.
func (rs *ruleset) addReadOnly(key, path string, dirOK bool) {
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return
	case err != nil:
		rs.fail(key, err)
	case info.IsDir() && !dirOK:
		rs.fail(key, fmt.Errorf("%s is a directory, not a file", path))
	case info.IsDir():
		rs.add(landlock.PathAccess(accessRead, path))
	default:
		rs.add(landlock.PathAccess(accessRead&^llsyscall.AccessFSReadDir, path))
	}
}

// addTCPConnect allows connecting to a TCP port.
func (rs *ruleset) addTCPConnect(label string, port uint16) {
	entry := fmt.Sprintf("%s:connect-tcp/%d", label, port)
	if slices.Contains(rs.info, entry) {
		return
	}
	rs.info = append(rs.info, entry)
	if !slices.Contains(rs.tcpConnect, port) {
		rs.tcpConnect = append(rs.tcpConnect, port)
		rs.add(landlock.ConnectTCP(port))
	}
}

// addURL allows an HTTP client to reach raw: the TCP port it dials and, when
// the host is a name only DNS resolves, name resolution.
func (rs *ruleset) addURL(label, raw string) {
	port, ok := urlConnectPort(raw)
	if !ok {
		return
	}
	rs.addTCPConnect(label, port)
	if u, err := url.Parse(raw); err == nil {
		rs.addHostName(u.Hostname())
	}
}

// addHostName records a target host that may need DNS.
func (rs *ruleset) addHostName(host string) {
	if rs.hosts == nil {
		rs.hosts = localNames(hostsPath, nsswitchPath)
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if needsDNS(host, rs.hosts) && !slices.Contains(rs.dnsNames, host) {
		rs.dnsNames = append(rs.dnsNames, host)
	}
}

// addUnixSocket allows connect(2) to the pathname Unix socket at path; key
// names the config setting it comes from.
//
// A socket in a directory of its own (/run/postgresql, /run/mysqld,
// /run/podman) is granted through that directory, so a server that recreates
// the socket on restart stays reachable. A socket in a directory shared with
// other services' sockets (/run, /tmp, a user's runtime directory) is granted
// as the socket file that exists now: granting the directory would open every
// socket below it, such as the D-Bus system bus under /run.
//
// warnIfMissing logs when what is granted does not exist yet: a socket that
// appears later stays unreachable until Kula restarts.
func (rs *ruleset) addUnixSocket(key, label, path string, warnIfMissing bool) {
	path = filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	} else if realDir, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
		path = filepath.Join(realDir, filepath.Base(path))
	}
	dir := filepath.Dir(path)
	target, shown := path, path
	if dedicatedSocketDir(dir) {
		target, shown = dir, strings.TrimSuffix(dir, "/")+"/"
	}
	if slices.Contains(rs.sockets, shown) {
		return
	}
	if _, err := os.Stat(target); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			// A path through a file, or one Kula may not look into: no
			// socket there can be reached, sandbox or not.
			rs.fail(key, err)
			return
		}
		if warnIfMissing && rs.abi >= 9 {
			log.Printf("Warning: sandbox: %s socket %s does not exist; if it appears later, Kula can connect to it only after a restart", label, target)
		}
	}
	rs.add(landlock.PathAccess(accessResolveUnix, target).IgnoreIfMissing())
	rs.sockets = append(rs.sockets, shown)
	rs.info = append(rs.info, fmt.Sprintf("%s:unix(%s)", label, shown))
}

// sharedSocketDirs hold the sockets of several unrelated services, or the
// root-equivalent ones of the service manager and the system bus.
var sharedSocketDirs = []string{
	"/", "/run", "/var/run", "/tmp", "/var/tmp", "/dev", "/dev/shm",
	"/var", "/var/lib", "/run/user", "/home", "/root", "/etc", "/usr",
	"/run/systemd", "/run/dbus", "/var/run/dbus",
}

// dedicatedSocketDir reports whether dir holds only one service's sockets, so
// a grant on it reaches no other service.
func dedicatedSocketDir(dir string) bool {
	if slices.Contains(sharedSocketDirs, dir) {
		return false
	}
	// A user's runtime directory holds their session bus, agents and
	// systemd manager; a home directory, agents and runtimes of all sorts.
	for _, parent := range []string{"/run/user/", "/home/"} {
		if rest, ok := strings.CutPrefix(dir, parent); ok && rest != "" && !strings.Contains(rest, "/") {
			return false
		}
	}
	info, err := os.Stat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return true // nothing is granted until it exists
	}
	// Anyone may create sockets in a world-writable directory.
	return err == nil && info.IsDir() && info.Mode()&(os.ModeSticky|0o002) == 0
}

// addApplications grants what the enabled application collectors read and
// connect to.
func (rs *ruleset) addApplications(appCfg config.ApplicationsConfig) {
	// Nginx and Apache2: the status URL.
	if appCfg.Nginx.Enabled && appCfg.Nginx.StatusURL != "" {
		rs.addURL("nginx", appCfg.Nginx.StatusURL)
	}
	if appCfg.Apache2.Enabled && appCfg.Apache2.StatusURL != "" {
		rs.addURL("apache2", appCfg.Apache2.StatusURL)
	}

	// Containers: the runtime socket the collector will use, chosen from the
	// collector's own candidate list.
	if appCfg.Containers.Enabled {
		if socketPath := pickContainerSocket(collector.ContainerSocketPaths(appCfg.Containers.SocketPath)); socketPath != "" {
			rs.addUnixSocket("applications.containers.socket_path", "containers", socketPath, true)
		}
	}

	if appCfg.Postgres.Enabled {
		rs.addPostgres(appCfg.Postgres)
	}
	if appCfg.Mysql.Enabled {
		rs.addMysql(appCfg.Mysql)
	}
}

// addPostgres grants what lib/pq connects to for the configured host and
// port: a socket <dir>/.s.PGSQL.<port> for each host that is an absolute
// path, TCP to the port otherwise. Kula always sends the configured host,
// so PGHOST does not apply; a port of 0 leaves the port to PGPORT, else 5432;
// PGHOSTADDR replaces the host with IP addresses dialled over TCP. A host may
// be a comma-separated list.
func (rs *ruleset) addPostgres(pg config.PostgresConfig) {
	const key = "applications.postgres.host"
	ports := postgresPorts(pg.Port)
	if os.Getenv("PGHOSTADDR") != "" {
		for _, port := range ports {
			rs.addTCPConnect("postgres", port)
		}
	} else {
		for i, host := range strings.Split(pg.Host, ",") {
			switch {
			case filepath.IsAbs(host):
				for _, port := range ports {
					rs.addUnixSocket(key, "postgres", filepath.Join(host, ".s.PGSQL."+strconv.Itoa(int(port))), true)
				}
			case strings.HasPrefix(host, "@"):
				rs.abstractUsers = append(rs.abstractUsers, "postgres")
			default:
				if host == "" && i > 0 {
					host = "localhost" // as lib/pq fills an empty list entry
				}
				for _, port := range ports {
					rs.addTCPConnect("postgres", port)
				}
				rs.addHostName(host)
			}
		}
	}

	// TLS: lib/pq reads its CA, client certificate and key from
	// ~/.postgresql (root.crt, postgresql.crt, postgresql.key) or from the
	// PGSSL* environment paths, at connect time. (It refuses PGSSLCRL, so
	// that path needs no rule.)
	if postgresUsesTLS(pg.SSLMode) {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			rs.addReadOnly("~/.postgresql", filepath.Join(home, ".postgresql"), true)
		}
		for _, env := range []string{"PGSSLROOTCERT", "PGSSLCERT", "PGSSLKEY"} {
			if path := os.Getenv(env); path != "" && path != "system" {
				rs.addReadOnly(env, path, false)
			}
		}
	}
}

// postgresPorts returns the ports lib/pq connects to: the configured one, or
// PGPORT (a comma-separated list), or 5432.
func postgresPorts(configured int) []uint16 {
	if configured != 0 {
		if configured > 0 && configured <= 65535 {
			return []uint16{uint16(configured)}
		}
		return nil
	}
	env := os.Getenv("PGPORT")
	if env == "" {
		return []uint16{5432}
	}
	var ports []uint16
	for _, field := range strings.Split(env, ",") {
		if field == "" {
			field = "5432"
		}
		if p, err := strconv.ParseUint(field, 10, 16); err == nil && p > 0 && !slices.Contains(ports, uint16(p)) {
			ports = append(ports, uint16(p))
		}
	}
	return ports
}

// addMysql grants what the MySQL driver connects to: the socket file named by
// host when port is 0, TCP to the port otherwise; and the sslrootcert it
// reads for each TLS connection.
func (rs *ruleset) addMysql(my config.MysqlConfig) {
	if my.SSLRootCert != "" {
		rs.addReadOnly("applications.mysql.sslrootcert", my.SSLRootCert, false)
	}
	switch {
	case my.Port == 0 && strings.HasPrefix(my.Host, "@"):
		rs.abstractUsers = append(rs.abstractUsers, "mysql")
	case my.Port == 0 && my.Host != "":
		if path, err := filepath.Abs(my.Host); err == nil {
			rs.addUnixSocket("applications.mysql.host", "mysql", path, true)
		}
	case my.Port > 0 && my.Port <= 65535:
		rs.addTCPConnect("mysql", uint16(my.Port))
		rs.addHostName(my.Host)
	}
}

// addDNS allows name resolution when a target is a host name the hosts file
// does not answer: UDP datagrams to port 53 from an ephemeral port (the
// socket autobinds), and TCP to port 53 for answers too large for a datagram.
// Landlock rules name ports, not addresses: this reaches port 53 of any host.
func (rs *ruleset) addDNS() {
	if len(rs.dnsNames) == 0 {
		return
	}
	rs.add(landlock.BindUDP(0), landlock.ConnectSendUDP(53), landlock.ConnectTCP(53))
	// Go rereads resolv.conf when it changes. A file replaced by rename is a
	// new inode the rule on resolv.conf does not cover; when resolv.conf is a
	// symlink into a directory of its own (systemd-resolved's
	// /run/systemd/resolve, resolvconf, NetworkManager), allow reading the
	// files there.
	if target, err := filepath.EvalSymlinks(resolvConfPath); err == nil {
		if dir := filepath.Dir(target); dir != filepath.Dir(resolvConfPath) && !slices.Contains(sharedSocketDirs, dir) {
			rs.add(landlock.PathAccess(landlock.AccessFSSet(llsyscall.AccessFSReadFile), dir).IgnoreIfMissing())
		}
	}
	rs.info = append(rs.info, fmt.Sprintf("dns:udp+tcp/53(%s)", strings.Join(rs.dnsNames, ",")))
}

// loadSystemRoots loads the system CA roots before confinement when an
// enabled target verifies TLS against them. crypto/x509 loads them once, on
// first use, and then caches them; the CA bundle, and the
// distribution-specific tree its symlinks point into, is not readable under
// the sandbox, so every verification would fail for lack of roots.
func (rs *ruleset) loadSystemRoots(appCfg config.ApplicationsConfig, ollamaCfg config.OllamaConfig) {
	if !needsSystemRoots(appCfg, ollamaCfg) {
		return
	}
	if pool, err := x509.SystemCertPool(); err != nil || len(pool.Subjects()) == 0 { //nolint:staticcheck // Subjects only counts the roots here
		log.Printf("Warning: no system CA certificates loaded (%v); TLS connections to monitored targets will fail to verify", err)
	} else {
		rs.info = append(rs.info, "tls:system-roots")
	}
}

// config returns the Landlock configuration to restrict with.
func (rs *ruleset) config() landlock.Config {
	cfg := requested
	if len(rs.abstractUsers) > 0 {
		// A database reached at an abstract address ("@name") is a process
		// outside the sandbox: keep abstract Unix sockets unscoped.
		cfg.Scoped &^= llsyscall.ScopeAbstractUnixSocket
	}
	return cfg.BestEffort()
}

// unsupported logs that the kernel has no Landlock and reports whether that
// is so, in which case the caller leaves the process unconfined.
func (rs *ruleset) unsupported(what string) bool {
	if rs.abi >= 1 {
		return false
	}
	log.Printf("Landlock not supported or disabled by kernel (skipping sandbox enforcement%s): %v", what, rs.abiErr)
	return true
}

// restrict applies the ruleset to the whole process. A grant the config asks
// for that cannot be applied is logged and left out: what it names cannot be
// reached, sandbox or not. An error means the kernel did not accept the
// ruleset, and the process is not confined.
func (rs *ruleset) restrict(what, details string) error {
	for _, err := range rs.errs {
		log.Printf("Warning: sandbox: %v (left out of the sandbox)", err)
	}
	// The local time zone loads on first use, from /etc/localtime, which
	// the sandbox does not grant: load it now, or log times turn UTC.
	_, _ = time.Now().Zone()
	if err := rs.config().Restrict(rs.rules...); err != nil {
		return fmt.Errorf("sandbox: enforcing landlock: %w", err)
	}

	var apps string
	if len(rs.info) > 0 {
		apps = fmt.Sprintf(", apps: %v", rs.info)
	}
	confined, missing := rs.coverage()
	coverage := "confines " + strings.Join(confined, ", ")
	if len(missing) > 0 {
		coverage += "; not confined: " + strings.Join(missing, ", ")
	}
	log.Printf("Landlock sandbox enforced%s (ABI v%d %s; %s%s)", what, rs.abi, coverage, details, apps)
	return nil
}

// coverage names what the running kernel's ABI confines and what it cannot.
func (rs *ruleset) coverage() (confined, missing []string) {
	for _, feature := range []struct {
		abi  int
		name string
	}{
		{1, "files"},
		{4, "TCP"},
		{6, "signals"},
		{6, "abstract Unix sockets"},
		{9, "Unix sockets"},
		{10, "UDP"},
	} {
		switch {
		case feature.name == "abstract Unix sockets" && len(rs.abstractUsers) > 0:
			missing = append(missing, feature.name+" (used by "+strings.Join(rs.abstractUsers, ", ")+")")
		case rs.abi >= feature.abi:
			confined = append(confined, feature.name)
		default:
			missing = append(missing, feature.name+" (needs ABI v"+strconv.Itoa(feature.abi)+")")
		}
	}
	return confined, missing
}

// Enforce applies the `kula serve` Landlock restrictions to the current
// process.
//
// It restricts filesystem access to the paths Kula needs:
//   - /proc and /sys (read-only, for metrics collection)
//   - configPath (read-only)
//   - storageDir (read-write; created first if missing)
//   - the directory of web.UnixSocket (creating and removing the socket)
//
// It restricts network access to binding the web port (no TCP bind with
// web.UnixSocket set) and to the connections of the enabled application
// collectors and the Ollama proxy: their TCP ports, their pathname Unix
// sockets, and DNS when one of them is named by a host name the hosts file
// does not answer. UDP is otherwise denied, and signals and abstract Unix
// sockets cannot reach processes outside the sandbox.
//
// Call it before opening storage or starting anything that serves requests
// or connects out.
//
// On a kernel without Landlock it logs a line and returns nil, without
// touching anything. A grant the config asks for that cannot be applied, such
// as a socket path through a regular file, is logged and left out, and the
// rest is enforced. An error means the sandbox is not in force: the caller
// warns and runs unconfined.
func Enforce(configPath string, storageDir string, webCfg config.WebConfig, appCfg config.ApplicationsConfig, ollamaCfg config.OllamaConfig) error {
	rs := newRuleset()
	if rs.unsupported("") {
		return nil
	}

	// Resolve paths to absolute to satisfy Landlock requirements
	absConfigPath, err := filepath.Abs(configPath)
	if err != nil {
		return fmt.Errorf("sandbox: resolving config path: %w", err)
	}

	absStorageDir, err := filepath.Abs(storageDir)
	if err != nil {
		return fmt.Errorf("sandbox: resolving storage dir: %w", err)
	}

	rs.addSystemReads()
	// Config file: read-only
	rs.add(landlock.ROFiles(absConfigPath).IgnoreIfMissing())
	// The storage directory must exist to be granted.
	if err := os.MkdirAll(absStorageDir, 0750); err != nil {
		rs.fail("storage.directory", err)
	} else {
		// Data storage: read-write. Connecting to a socket there lets the
		// custom-metrics collector tell a live kula.sock from a stale one.
		rs.add(landlock.RWDirs(absStorageDir).WithResolveUnix())
	}

	webStatus := ", net: disabled"
	if webCfg.Enabled {
		if webCfg.UnixSocket != "" {
			absSock, err := filepath.Abs(webCfg.UnixSocket)
			if err != nil {
				return fmt.Errorf("sandbox: resolving web unix_socket: %w", err)
			}
			webStatus = ", web: unix:" + absSock
			sockDir := filepath.Dir(absSock)
			if info, err := os.Stat(sockDir); err != nil {
				rs.fail("web.unix_socket", fmt.Errorf("%s: its directory: %w", absSock, err))
			} else if !info.IsDir() {
				rs.fail("web.unix_socket", fmt.Errorf("%s: %s is not a directory", absSock, sockDir))
			} else {
				// Create the socket and remove a stale one; nothing else there.
				rs.add(landlock.PathAccess(accessSocketListener, sockDir))
				// Before removing a socket left there, the server connects to
				// it to make sure no other process still serves it.
				rs.addUnixSocket("web.unix_socket", "web", absSock, false)
			}
		} else if webCfg.Port > 0 {
			if webCfg.Port > 65535 {
				rs.fail("web.port", fmt.Errorf("invalid port %d", webCfg.Port))
			} else {
				rs.add(landlock.BindTCP(uint16(webCfg.Port)))
				webStatus = fmt.Sprintf(", net: bind TCP/%d", webCfg.Port)
			}
		}
	}

	rs.addApplications(appCfg)
	if ollamaCfg.Enabled && ollamaCfg.URL != "" {
		rs.addURL("ollama", ollamaCfg.URL)
	}
	rs.addDNS()
	rs.loadSystemRoots(appCfg, ollamaCfg)

	return rs.restrict("", fmt.Sprintf("paths: /proc[ro] /sys[ro] %s[ro] %s[rw]%s",
		absConfigPath, absStorageDir, webStatus))
}

// EnforceTUI applies the `kula tui` Landlock restrictions to the current
// process: the reads and connections of its collectors as under Enforce, the
// storage directory read-only for the History view, and the controlling
// terminal. It grants no writes, no listeners and no TCP bind. The TUI shows
// no custom metrics, so appCfg.Custom is ignored.
//
// A storage directory that does not exist yet stays unreadable until the TUI
// restarts. Semantics as for Enforce: without Landlock it logs and returns
// nil, a grant that cannot be applied is logged and left out, and an error
// means the sandbox is not in force.
func EnforceTUI(storageDir string, appCfg config.ApplicationsConfig) error {
	rs := newRuleset()
	if rs.unsupported(" for the TUI") {
		return nil
	}

	absStorageDir, err := filepath.Abs(storageDir)
	if err != nil {
		return fmt.Errorf("sandbox: resolving storage dir: %w", err)
	}

	if _, err := os.Stat(absStorageDir); errors.Is(err, fs.ErrNotExist) {
		log.Printf("Warning: sandbox: storage directory %s does not exist; the History view can read it only after the TUI restarts", absStorageDir)
	}

	rs.addSystemReads()
	rs.add(
		landlock.PathAccess(accessRead, absStorageDir).IgnoreIfMissing(),
		landlock.PathAccess(accessTerminal, "/dev/tty").IgnoreIfMissing(),
	)
	rs.addApplications(appCfg)
	rs.addDNS()
	rs.loadSystemRoots(appCfg, config.OllamaConfig{})

	return rs.restrict(" for the TUI", fmt.Sprintf("paths: /proc[ro] /sys[ro] %s[ro], net: no listener", absStorageDir))
}

// pickContainerSocket mirrors the container collector's choice: the first
// candidate that exists and accepts a connection, or else the first that
// exists, so the collector reports the real reason it cannot use it.
func pickContainerSocket(candidates []string) string {
	first := ""
	for _, path := range candidates {
		if _, err := os.Stat(path); err != nil {
			continue
		}
		if first == "" {
			first = path
		}
		if conn, err := net.DialTimeout("unix", path, time.Second); err == nil {
			_ = conn.Close()
			return path
		}
	}
	return first
}

// urlConnectPort returns the TCP port an HTTP client dials for raw: the
// explicit port, or the scheme's default (443 for https, 80 otherwise).
func urlConnectPort(raw string) (uint16, bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return 0, false
	}
	if u.Port() == "" {
		if strings.EqualFold(u.Scheme, "https") {
			return 443, true
		}
		return 80, true
	}
	p, err := strconv.ParseUint(u.Port(), 10, 16)
	if err != nil || p == 0 {
		return 0, false
	}
	return uint16(p), true
}

// localNames returns the host names Go's resolver answers from the hosts
// file without DNS, lower-cased: those it lists, unless nsswitch.conf leaves
// files out of the hosts lookup. Go consults the file before DNS, or after a
// DNS lookup fails ("hosts: dns files"), so a listed name resolves whatever
// the order.
func localNames(hostsFile, nsswitchFile string) map[string]bool {
	names := make(map[string]bool)
	if !hostsFromFiles(nsswitchFile) {
		return names
	}
	data, err := os.ReadFile(hostsFile)
	if err != nil {
		return names
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if _, err := netip.ParseAddr(fields[0]); err != nil {
			continue
		}
		for _, name := range fields[1:] {
			names[strings.TrimSuffix(strings.ToLower(name), ".")] = true
		}
	}
	return names
}

// hostsFromFiles reports whether Go's resolver (CGO_ENABLED=0) reads the
// hosts file for a lookup: when nsswitch.conf is missing or unreadable, has
// no hosts line or lists no source on it, or lists "files" among them.
func hostsFromFiles(nsswitchFile string) bool {
	data, err := os.ReadFile(nsswitchFile)
	if err != nil {
		return true
	}
	files := true
	for line := range strings.SplitSeq(string(data), "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		db, sources, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(db) != "hosts" {
			continue
		}
		fields := strings.Fields(sources)
		files = len(fields) == 0 || slices.Contains(fields, "files")
	}
	return files
}

// needsDNS reports whether Go's resolver needs DNS to resolve host: it is
// neither empty, nor an IP address, nor a name it answers from the hosts
// file.
func needsDNS(host string, local map[string]bool) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "" {
		return false
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return false
	}
	return !local[host]
}

// postgresUsesTLS reports whether a PostgreSQL sslmode can negotiate TLS.
func postgresUsesTLS(sslmode string) bool {
	return sslmode != "" && sslmode != "disable"
}

// needsSystemRoots reports whether an enabled outbound target verifies a TLS
// certificate against the system roots.
func needsSystemRoots(appCfg config.ApplicationsConfig, ollamaCfg config.OllamaConfig) bool {
	https := func(raw string) bool {
		u, err := url.Parse(raw)
		return err == nil && strings.EqualFold(u.Scheme, "https")
	}
	switch {
	case appCfg.Nginx.Enabled && https(appCfg.Nginx.StatusURL),
		appCfg.Apache2.Enabled && https(appCfg.Apache2.StatusURL),
		ollamaCfg.Enabled && https(ollamaCfg.URL),
		appCfg.Mysql.Enabled && appCfg.Mysql.SSLMode == "verify-full" && appCfg.Mysql.SSLRootCert == "":
		return true
	case appCfg.Postgres.Enabled:
		mode := appCfg.Postgres.SSLMode
		return mode == "verify-ca" || mode == "verify-full" || os.Getenv("PGSSLROOTCERT") == "system"
	}
	return false
}

// BuildRuleSummary returns a human-readable summary of the sandbox rules
// for logging and debugging purposes.
func BuildRuleSummary(configPath string, storageDir string, webPort int) string {
	absConfig, _ := filepath.Abs(configPath)
	absStorage, _ := filepath.Abs(storageDir)
	net := fmt.Sprintf("bind TCP/%d", webPort)
	if webPort == 0 {
		net = "disabled"
	}
	return fmt.Sprintf(
		"FS: /proc[ro] /sys[ro] %s[ro] %s[rw] | Net: %s",
		absConfig, absStorage, net,
	)
}
