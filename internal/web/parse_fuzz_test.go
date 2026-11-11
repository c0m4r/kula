package web

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"kula/internal/config"
)

// Fuzz coverage for request-header parsers on the security path: the CSRF
// origin check and the client-IP extractor (which feeds rate-limiter keys and
// logs). Both consume fully attacker-controlled header values.

// FuzzValidateOrigin asserts the CSRF origin gate never accepts a cross-origin
// request. With no allow-list configured, the only safe "true" is an
// Origin/Referer whose host equals the request Host; the fuzzer hunts for any
// crafted header that slips a foreign host past that check.
func FuzzValidateOrigin(f *testing.F) {
	const host = "kula.example:27960"

	for _, s := range [][2]string{
		{"http://kula.example:27960", ""},
		{"https://evil.com", ""},
		{"", "http://kula.example:27960/dashboard"},
		{"null", ""},
		{"http://kula.example:27960@evil.com", ""},
		{"http://kula.example:27960.evil.com", ""},
		{"HTTP://KULA.EXAMPLE:27960", ""},
		{"http://kula.example", ""},
		{"http:\\\\kula.example:27960", ""},
		{"http://kula.example:27960\t", "http://evil"},
	} {
		f.Add(s[0], s[1])
	}

	am := NewAuthManager(config.AuthConfig{}, "", false, config.SecurityConfig{OriginValidation: true})

	f.Fuzz(func(t *testing.T, origin, referer string) {
		r := httptest.NewRequest(http.MethodPost, "http://"+host+"/api/x", nil)
		r.Host = host
		r.Header.Set("Origin", origin)
		r.Header.Set("Referer", referer)

		if !am.ValidateOrigin(r) {
			return // rejected — safe
		}

		// Accepted. Re-derive the effective source the same way ValidateOrigin
		// does (Origin, falling back to Referer) and confirm its host really is
		// our own Host — otherwise the CSRF guard let a foreign origin through.
		eff := origin
		if eff == "" {
			eff = referer
		}
		u, err := url.Parse(eff)
		if err != nil {
			t.Fatalf("ValidateOrigin accepted origin=%q referer=%q but url.Parse(%q) errors: %v", origin, referer, eff, err)
		}
		if !strings.EqualFold(u.Host, host) {
			t.Fatalf("ValidateOrigin accepted cross-origin: origin=%q referer=%q parsed host=%q want %q", origin, referer, u.Host, host)
		}
	})
}

// FuzzGetClientIP ensures the client-IP extractor never panics on hostile
// RemoteAddr / X-Forwarded-For values and honors its documented contract: when
// the proxy is trusted and the rightmost (trusted) XFF segment is an address,
// that address is returned; otherwise the connection's.
func FuzzGetClientIP(f *testing.F) {
	f.Add("203.0.113.5:1234", "10.0.0.1, 192.168.1.1", true)
	f.Add("[2001:db8::1]:443", "", false)
	f.Add("garbage-no-port", "x", true)
	f.Add("", "  1.2.3.4 , 5.6.7.8  ", true)
	f.Add("1.2.3.4:5", ",,,", true)

	f.Fuzz(func(t *testing.T, remoteAddr, xff string, trustProxy bool) {
		r := httptest.NewRequest(http.MethodGet, "http://example/", nil)
		r.RemoteAddr = remoteAddr
		r.Header.Set("X-Forwarded-For", xff)

		ip := getClientIP(r, trustProxy) // must not panic

		want, ok := "", false
		if trustProxy && xff != "" {
			parts := strings.Split(xff, ",")
			want, ok = forwardedAddr(strings.TrimSpace(parts[len(parts)-1]))
		}
		if !ok {
			want = remoteAddr
			if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
				want = host
			}
		}
		if ip != want {
			t.Fatalf("getClientIP = %q, want %q (remote %q, xff %q, trusted %v)", ip, want, remoteAddr, xff, trustProxy)
		}
	})
}
