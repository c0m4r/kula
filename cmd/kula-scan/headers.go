package main

import (
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
)

// Extra request headers (-H): what a gateway in front of Kula asks every
// request to carry, such as an access proxy's service token or basic auth at
// a reverse proxy. They travel on every request the scanner sends, through
// the HTTP client, raw sockets and WebSocket handshakes alike. Their values
// are secrets as likely as not, so nothing prints them: not the -v request
// log, not a finding, not the report, which lists their names only.

// extraHeader is one -H header.
type extraHeader struct {
	name  string // canonical form, e.g. X-Access-Token
	value string // never printed
}

// parseExtraHeaders parses -H values, each "Name: value". An error names the
// header by its position or its name, never by its value.
func parseExtraHeaders(specs []string) ([]extraHeader, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	out := make([]extraHeader, 0, len(specs))
	for i, spec := range specs {
		name, value, found := strings.Cut(spec, ":")
		if !found || !validHeaderName(name) {
			return nil, fmt.Errorf("-H #%d is not \"Name: value\" with a valid header name before the colon", i+1)
		}
		name = http.CanonicalHeaderKey(name)
		value = strings.Trim(value, " \t")
		switch {
		case strings.ContainsAny(value, "\r\n"):
			return nil, fmt.Errorf("-H %s: the value contains a line break (CR or LF)", name)
		case strings.ContainsFunc(value, func(r rune) bool { return r < ' ' && r != '\t' || r == 0x7f }):
			return nil, fmt.Errorf("-H %s: the value contains a control character", name)
		case value == "":
			return nil, fmt.Errorf("-H %s: the header has no value", name)
		}
		if why := reservedHeader(name); why != "" {
			return nil, fmt.Errorf("-H %s: %s", name, why)
		}
		out = append(out, extraHeader{name: name, value: value})
	}
	return out, nil
}

// validHeaderName reports whether name is an HTTP field name, a token.
func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		alnum := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
		if !alnum && !strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c)) {
			return false
		}
	}
	return true
}

// reservedHeader says why -H may not set name, or returns "" when it may.
// These are the headers the scanner writes itself to frame a request and
// manage its connection; a second copy would break the probes.
func reservedHeader(name string) string {
	switch {
	case name == "Host":
		return "the target URL gives the Host header; to connect to another address under that name, use -connect host:port"
	case name == "Content-Length", name == "Transfer-Encoding":
		return "each request frames its own body"
	case name == "Connection", name == "Upgrade", strings.HasPrefix(name, "Sec-Websocket-"):
		return "the scanner sets the connection and WebSocket handshake headers itself"
	}
	return ""
}

// extraHeaderNames lists the -H header names, each once, for the report.
func (s *Scanner) extraHeaderNames() []string {
	var names []string
	for _, x := range s.extra {
		if !slices.Contains(names, x.name) {
			names = append(names, x.name)
		}
	}
	return names
}

// addExtraHeaders adds the -H headers to h, a request's own headers. A
// header the request sets itself wins over an extra one of the same name,
// except Cookie, which carries both, the -H cookies first: a gateway's
// cookie travels with the scanner's session cookie.
func (s *Scanner) addExtraHeaders(h http.Header) {
	if len(s.extra) == 0 {
		return
	}
	own := make(map[string]bool, len(h))
	for name := range h {
		own[http.CanonicalHeaderKey(name)] = true
	}
	var cookies []string
	for _, x := range s.extra {
		switch {
		case x.name == "Cookie":
			cookies = append(cookies, x.value)
		case !own[x.name]:
			h.Add(x.name, x.value)
		}
	}
	if len(cookies) > 0 {
		h.Set("Cookie", strings.Join(append(cookies, h.Values("Cookie")...), "; "))
	}
}

// rawHeaderLines renders header lines for a request written by hand: own,
// the probe's headers, with the -H headers added as addExtraHeaders adds
// them, one "Name: value\r\n" line per value, sorted by name. A probe that
// writes its Host and framing lines itself passes nil and writes the lines
// returned, the -H headers alone, after its Host line.
func (s *Scanner) rawHeaderLines(own http.Header) string {
	h := cloneHeader(own)
	s.addExtraHeaders(h)
	var b strings.Builder
	for _, name := range slices.Sorted(maps.Keys(h)) {
		for _, value := range h[name] {
			b.WriteString(name + ": " + value + "\r\n")
		}
	}
	return b.String()
}
