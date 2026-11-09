package web

import (
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"sync"

	"kula/internal/config"
)

// DNS rebinding: with auth disabled, a page on another site can point its own
// name at the address Kula listens on. The browser then treats Kula as that
// site and lets the page's script read the API, the dashboard and /ws (the
// Origin check passes too: Origin and Host name the same site). Such a request
// still carries the attacker's name in Host, so while auth is off Kula answers
// only Host names a public DNS server cannot answer for: IP addresses,
// localhost, single-label names, local-only domains, this machine's hostname
// and global.hostname, and the names in web.security.allowed_hosts or
// allowed_origins. With auth
// on, the page has no session to send; on a Unix socket, no browser connects
// directly. /health and /status stay open to probes, and so does /metrics
// behind a bearer token: a rebinding page cannot supply the token, and a
// Prometheus server reaches its targets by regular DNS names.

// localSuffixes are domains no public DNS server answers for: special-use
// names (RFC 6761, RFC 8375, ICANN's .internal) and the private suffixes home
// routers commonly hand out.
var localSuffixes = []string{"localhost", "local", "internal", "home.arpa", "lan", "home", "localdomain"}

// maxLoggedHosts bounds the set of refused names logged once each, so a flood
// of distinct names cannot grow memory or the log without limit.
const maxLoggedHosts = 64

type hostPolicy struct {
	names        map[string]bool // allowed beyond the built-in rules, normalized
	metricsToken bool            // /metrics asks for a bearer token

	mu     sync.Mutex
	logged map[string]bool
}

// newHostPolicy returns the Host check for cfg, or nil when none applies.
// machine lists this host's own names (os.Hostname, global.hostname), each
// matched exactly: a name with any suffix added is not this machine.
func newHostPolicy(cfg config.WebConfig, machine ...string) *hostPolicy {
	if cfg.Auth.Enabled || cfg.UnixSocket != "" {
		return nil
	}
	p := &hostPolicy{names: map[string]bool{}, logged: map[string]bool{}, metricsToken: cfg.PrometheusMetrics.Token != ""}
	for _, name := range cfg.Security.AllowedHosts {
		if name == "*" {
			return nil
		}
		p.names[normalizeHostName(name)] = true
	}
	for _, origin := range cfg.Security.AllowedOrigins {
		if host := originHost(origin); host != "" {
			p.names[host] = true
		}
	}
	for _, name := range machine {
		if name = normalizeHostName(name); name != "" {
			p.names[name] = true
		}
	}
	return p
}

func originHost(origin string) string {
	_, rest, ok := strings.Cut(origin, "://")
	if !ok {
		return ""
	}
	if host, _, err := net.SplitHostPort(rest); err == nil {
		return normalizeHostName(host)
	}
	return normalizeHostName(rest)
}

// normalizeHostName lowercases a host name and drops a trailing dot and IPv6
// brackets; it does not strip a port.
func normalizeHostName(name string) string {
	name = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
	return strings.TrimSuffix(strings.TrimPrefix(name, "["), "]")
}

// requestHostName returns the name part of a Host header, without its port.
func requestHostName(host string) string {
	if name, _, err := net.SplitHostPort(host); err == nil {
		host = name
	}
	return normalizeHostName(host)
}

func (p *hostPolicy) allows(host string) bool {
	name := requestHostName(host)
	if name == "" {
		return true // HTTP/1.0 without Host: not a browser
	}
	if _, err := netip.ParseAddr(name); err == nil {
		return true
	}
	if !strings.Contains(name, ".") || p.names[name] {
		return true
	}
	for _, suffix := range localSuffixes {
		if name == suffix || strings.HasSuffix(name, "."+suffix) {
			return true
		}
	}
	return false
}

// logRefusal logs each refused name once, up to maxLoggedHosts names.
func (p *hostPolicy) logRefusal(host string) {
	name := requestHostName(host)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.logged[name] || len(p.logged) > maxLoggedHosts {
		return
	}
	p.logged[name] = true
	if len(p.logged) > maxLoggedHosts {
		log.Printf("Host check: more refused host names; no longer logging them")
		return
	}
	log.Printf("Host check: refused Host %q (auth is disabled). If this is how you reach Kula, add it to web.security.allowed_hosts", name)
}

// hostCheckMiddleware refuses requests whose Host the policy does not allow.
// Paths are relative to the base path.
func hostCheckMiddleware(p *hostPolicy, next http.Handler) http.Handler {
	if p == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		open := r.URL.Path == "/health" || r.URL.Path == "/status" || (p.metricsToken && r.URL.Path == "/metrics")
		if !open && !p.allows(r.Host) {
			p.logRefusal(r.Host)
			jsonError(w, "host not allowed: add it to web.security.allowed_hosts", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// machineHostname is this host's name, or "" when it cannot be read.
func machineHostname() string {
	name, err := os.Hostname()
	if err != nil {
		return ""
	}
	return name
}
