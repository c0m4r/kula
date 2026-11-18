package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode"
)

// DNS rebinding: a page on another site points its own name at the address
// Kula listens on, and its script reads Kula through the visitor's browser,
// which takes Kula for that site. Every such request names the attacker's
// host in Host, so while auth is off Kula answers only Host names no public
// DNS server answers for (internal/web/host_check.go): IP addresses,
// localhost, single-label names, local-only domains, the machine's own names
// and web.security.allowed_hosts. With auth on it guards only a /metrics
// served without a token, the one route with data and no credential of its
// own; on a Unix socket it guards nothing. HOST-001 sends such names itself.

func hostChecks() []check {
	return []check{{ids: []string{"HOST-001"}, run: runHostCheck}}
}

// ---- the scanner's own Host name refused (DISC-003) --------------------------

// hostRefusedFinding is DISC-003: Kula refuses the Host name the scanner
// sends, as discovery saw on path (answered r). The how-to is in the detail
// as well, since the text report prints no remediation for an ERROR.
func (s *Scanner) hostRefusedFinding(path string, r httpResult) Finding {
	how := fmt.Sprintf("Rescan by IP address, or with -connect <ip:port> and a URL naming a host Kula allows (localhost, an address), or add %s to web.security.allowed_hosts.", s.base.Hostname())
	return finding("DISC-003", SevInfo, StatusError,
		fmt.Sprintf("Kula's DNS-rebinding check refuses the Host name the scanner sends (%s): with auth off, Kula answers only local names (addresses, localhost, single-label names, local-only domains such as .lan or .internal) and web.security.allowed_hosts, and every route but /health and /status answers this refusal. "+
			"The checks that read the API, the UI page or the WebSocket would take it for their own safeguard holding, so they are skipped: Kula's safeguards behind them were not judged. %s",
			s.base.Host, how)).
		withEvidence("GET %s: HTTP %d %s", path, r.status, truncate(strings.TrimSpace(r.body), 120)).
		withRemediation(how)
}

// refusedHostReason is the SKIP detail of a check not run under a refused
// Host name.
func (s *Scanner) refusedHostReason() string {
	return "Kula's DNS-rebinding check refuses the Host name " + s.base.Host +
		" (DISC-003), which this check would read as its own result; rescan by IP address or by a name Kula allows."
}

// runsUnderRefusedHost reports whether check c still judges something when
// Kula refuses the scanner's Host name. Kula refuses it only with auth off,
// and then on every route but /health and /status (host_check.go), after its
// security headers are set and before any other handler runs. So these
// remain meaningful: the header checks, which read headers the refusal
// carries too; the auth check, since a refused /api/auth/status itself shows
// auth is off; TLS, below HTTP; HOST-001, which sends foreign names anyway;
// and the slow-request and idle-connection probes, whose requests never
// complete, with /health as their liveness probe. Every other check reads an
// answer the refusal replaces, and would report it as its own safeguard (a
// 403 from CSRF-001, CSRF-002 or WS-002) or as a missing one.
func runsUnderRefusedHost(c check) bool {
	switch c.entry().Category {
	case "headers", "auth", "tls", "host":
		return true
	}
	return slices.Contains(c.ids, "DOS-SLOWLORIS") || slices.Contains(c.ids, "DOS-CONNFLOOD")
}

// explicitlySkipped reports whether -skip names id or its category.
func explicitlySkipped(sel *selection, id string) bool {
	return sel != nil && (sel.skip[strings.ToLower(id)] || sel.skip[catalogEntryFor(id).Category])
}

// metricsHostRefusal judges /metrics answered with Kula's refusal of the
// scanner's Host name. With auth on, Kula checks the Host of /metrics only
// when it is served without a token, so the refusal shows exactly that. With
// auth off it checks every route, a /metrics that is not enabled included,
// so the refusal shows nothing about /metrics.
func (s *Scanner) metricsHostRefusal(r httpResult) []Finding {
	if !s.authEnabled {
		return []Finding{
			finding("PROM-001", SevInfo, StatusSkip, s.refusedHostReason()),
			finding("PROM-003", SevInfo, StatusSkip, s.refusedHostReason()),
		}
	}
	return []Finding{
		finding("PROM-001", SevMedium, StatusWarn,
			"/metrics refused this Host name with Kula's DNS-rebinding check, which Kula applies with auth on only to a /metrics served without a bearer token: anyone who reaches it by IP address or a local name reads the hostname and detailed system metrics without one.").
			withEvidence("GET /metrics: HTTP %d %s", r.status, truncate(strings.TrimSpace(r.body), 120)).
			withRemediation("Set web.prometheus_metrics.token, or restrict /metrics at the network/proxy layer.").
			expecting("metrics-token"),
		finding("PROM-003", SevInfo, StatusSkip,
			"/metrics is served without a token (Kula checks the Host name of a token-less /metrics only), so no token is required."),
	}
}

// foreignHost is a Host value a public DNS server could answer for, and the
// form it takes.
type foreignHost struct {
	form, host string
}

// foreignHosts returns the Host values HOST-001 sends: a random name under
// example.com, and the forms of it a check comparing strings too literally
// lets through.
func (s *Scanner) foreignHosts() []foreignHost {
	name := "kula-scan-" + randToken(10) + ".example.com"
	port := s.base.Port()
	if port == "" {
		port = "80"
		if s.base.Scheme == "https" {
			port = "443"
		}
	}
	return []foreignHost{
		{"a foreign name", name},
		{"with a port", net.JoinHostPort(name, port)},
		{"with a trailing dot", name + "."},
		{"in mixed case", mixedCase(name)},
		{"behind a local label", "localhost." + name},
	}
}

// mixedCase upper-cases every other letter of s.
func mixedCase(s string) string {
	out := []rune(s)
	for i, r := range out {
		if i%2 == 0 {
			out[i] = unicode.ToUpper(r)
		}
	}
	return string(out)
}

// hostAnswer is how the target answered a request under one Host name.
type hostAnswer int

const (
	hostServed      hostAnswer = iota // Kula's own answer: the data the route serves
	hostKulaRefused                   // Kula's host check: 403 "host not allowed"
	hostOther                         // anything else: a proxy's 404 or 421, a closed connection
)

// classifyHost reads r, the answer to route.
func classifyHost(route string, r httpResult) hostAnswer {
	switch {
	case hostCheckRefusal(r):
		return hostKulaRefused
	case routeServed(route, r):
		return hostServed
	default:
		return hostOther
	}
}

// routeServed reports whether r is Kula's answer to route with its data:
// the configuration /api/config serves, or the metrics /metrics serves.
func routeServed(route string, r httpResult) bool {
	if r.status != http.StatusOK {
		return false
	}
	if route == "/metrics" {
		return prometheusExposition(r)
	}
	var config map[string]json.RawMessage
	if json.Unmarshal([]byte(r.body), &config) != nil {
		return false
	}
	_, ok := config["auth_enabled"]
	return ok
}

// describeHostAnswer says how a request was answered, for a detail line.
func describeHostAnswer(r httpResult) string {
	switch r.status {
	case 0:
		return "the connection closed without an answer"
	case http.StatusOK:
		return "HTTP 200, but not Kula's answer"
	default:
		return fmt.Sprintf("HTTP %d", r.status)
	}
}

// hostRequest sends GET path (below the base path) over a raw connection
// with host as its Host header, followed by the -H headers. As with rawTCP,
// an error means the request was not delivered, and a status of 0 without
// one that the target closed the connection without an answer.
func (s *Scanner) hostRequest(path, host string) (httpResult, error) {
	conn, err := s.dialRaw()
	if err != nil {
		s.logHostRequest(path, host, err.Error())
		return httpResult{}, err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(s.timeout))

	request := fmt.Sprintf("GET %s%s HTTP/1.1\r\nHost: %s\r\n%sConnection: close\r\n\r\n", s.base.Path, path, host, s.rawHeaderLines(nil))
	if _, err := io.WriteString(conn, request); err != nil {
		s.logHostRequest(path, host, err.Error())
		return httpResult{}, err
	}
	resp, err := readResponse(conn)
	if err != nil {
		s.logHostRequest(path, host, "no answer: "+err.Error())
		return httpResult{}, nil
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodyBytes))
	s.logHostRequest(path, host, fmt.Sprintf("%d (%d bytes)", resp.StatusCode, len(body)))
	return httpResult{status: resp.StatusCode, header: resp.Header, body: string(body)}, nil
}

// logHostRequest logs a hostRequest and how it ended, with -v.
func (s *Scanner) logHostRequest(path, host, outcome string) {
	if s.verbose {
		_, _ = fmt.Fprintf(s.log, "    GET %s (Host: %s) -> %s\n", printable(path), printable(host), printable(outcome))
	}
}

// runHostCheck judges HOST-001: the route Kula guards in the target's
// posture answers no Host name a public DNS server could answer for. Without
// auth that is every route but the probes, of which /api/config, open to
// anyone without auth, is asked; with auth only a /metrics served without a
// token is guarded. The same request under the scanner's own Host name is
// the control: a route that serves nothing under it has nothing to protect.
func runHostCheck(s *Scanner) []Finding {
	if s.network == "unix" {
		return []Finding{finding("HOST-001", SevInfo, StatusSkip,
			"the target is reached over a Unix socket (-unix): no browser connects to one, and Kula checks no Host name there.")}
	}
	route, what := "/api/config", "the API"
	if s.authEnabled {
		route, what = "/metrics", "/metrics"
	}

	own, err := s.hostRequest(route, s.base.Host)
	if err != nil {
		return []Finding{finding("HOST-001", SevInfo, StatusError,
			fmt.Sprintf("could not deliver GET %s under the scanner's own Host name over a raw socket: %v", route, err))}
	}
	ownAnswer := classifyHost(route, own)
	if s.authEnabled && ownAnswer == hostOther {
		// Kula guards no other route with auth on: a rebinding page has no
		// session to send, nor the bearer token of a /metrics that asks one.
		why := fmt.Sprintf("/metrics answered HTTP %d, not metrics", own.status)
		switch own.status {
		case http.StatusUnauthorized:
			why = "/metrics asks for a bearer token (HTTP 401), which such a page cannot send either"
		case http.StatusNotFound:
			why = "/metrics is not enabled (HTTP 404)"
		}
		return []Finding{finding("HOST-001", SevInfo, StatusSkip,
			"auth is enabled, so the API needs a session a rebinding page does not have, and "+why+": Kula checks the Host name of a token-less /metrics only.")}
	}

	hosts := s.foreignHosts()
	var served, refused, other, lostHosts []string
	var lost undelivered
	for _, h := range hosts {
		label := fmt.Sprintf("%s (%s)", h.host, h.form)
		r, err := s.hostRequest(route, h.host)
		if err != nil {
			lost.add(err)
			lostHosts = append(lostHosts, label)
			continue
		}
		switch classifyHost(route, r) {
		case hostServed:
			served = append(served, label)
		case hostKulaRefused:
			refused = append(refused, label)
		default:
			other = append(other, label+": "+describeHostAnswer(r))
		}
	}

	unreached := ""
	if lost.n > 0 {
		unreached = "; " + lost.detail(len(hosts), "requests") + ": " + strings.Join(lostHosts, ", ")
	}
	switch {
	case len(served) > 0:
		f := finding("HOST-001", SevHigh, StatusWarn,
			fmt.Sprintf("%s answered Host names a public DNS server could answer for: %s. A page on another site can point its own name at this address (DNS rebinding) and read %s through a visitor's browser%s.",
				what, strings.Join(served, ", "), what, unreached)).
			expecting("host-check")
		if s.authEnabled {
			return []Finding{f.withRemediation("Set web.prometheus_metrics.token, or take \"*\" out of web.security.allowed_hosts: Kula 0.22.0 and later answer a /metrics without a token only under local Host names and the names listed there.")}
		}
		return []Finding{f.withRemediation("Take \"*\" out of web.security.allowed_hosts and list the names Kula is reached by, upgrade a Kula older than 0.22.0, or enable web.auth: while auth is off, Kula answers only local Host names and the names listed there.")}
	case lost.n == len(hosts):
		return []Finding{finding("HOST-001", SevInfo, StatusError,
			lost.detail(len(hosts), "requests under foreign Host names")+".")}
	case lost.n > 0:
		return []Finding{finding("HOST-001", SevInfo, StatusError,
			lost.detail(len(hosts), "requests under foreign Host names")+": "+strings.Join(lostHosts, ", ")+
				fmt.Sprintf("; no answer under the others served %s, which says nothing of them.", what))}
	case ownAnswer == hostOther && len(refused) == 0:
		return []Finding{finding("HOST-001", SevInfo, StatusSkip,
			fmt.Sprintf("%s answered %s under the scanner's own Host name, not its data, so there is nothing a foreign name could read (is the UI off?).",
				route, describeHostAnswer(own)))}
	}

	var by []string
	if len(refused) > 0 {
		by = append(by, fmt.Sprintf("Kula's host check refused %d with 403 host not allowed", len(refused)))
	}
	if len(other) > 0 {
		by = append(by, "something other than Kula's host check refused "+strings.Join(other, ", ")+" (a proxy in front?)")
	}
	ownNote := ""
	if ownAnswer == hostKulaRefused {
		ownNote = fmt.Sprintf("; it refuses the scanner's own name %s too", s.base.Host)
	}
	return []Finding{finding("HOST-001", SevHigh, StatusPass,
		fmt.Sprintf("%s answered none of the Host names a public DNS server could answer for (a random name, with a port, a trailing dot, in mixed case, behind a local label): %s%s.",
			what, strings.Join(by, "; "), ownNote)).
		withEvidence("%s", strings.Join(append(refused, other...), " | "))}
}
