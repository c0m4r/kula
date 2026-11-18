package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand" // nosemgrep: go.lang.security.audit.crypto.math_random.math-random-used -- fuzz-input PRNG; -seed reproducibility needs a seedable source; not security-sensitive (tokens use crypto/rand)
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/gorilla/websocket"
)

// Severity ranks how serious a failed safeguard is. The zero value is SevInfo.
type Severity int

const (
	SevInfo Severity = iota
	SevLow
	SevMedium
	SevHigh
	SevCritical
)

func (s Severity) String() string {
	switch s {
	case SevCritical:
		return "CRITICAL"
	case SevHigh:
		return "HIGH"
	case SevMedium:
		return "MEDIUM"
	case SevLow:
		return "LOW"
	default:
		return "INFO"
	}
}

// parseSeverity maps a CLI string to a Severity. Reports ok=false on an
// unknown name so the caller can surface a usage error.
func parseSeverity(s string) (Severity, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "info":
		return SevInfo, true
	case "low":
		return SevLow, true
	case "medium", "med":
		return SevMedium, true
	case "high":
		return SevHigh, true
	case "critical", "crit":
		return SevCritical, true
	default:
		return SevInfo, false
	}
}

// Status is the outcome of a single check.
//
//   - StatusPass — the safeguard is present and behaves as expected.
//   - StatusFail — the safeguard is missing or bypassable (a finding).
//   - StatusWarn — not strictly a vulnerability, but a weak / risky posture.
//   - StatusSkip — the check did not apply (e.g. needs creds, feature disabled).
//   - StatusError — the probe itself could not complete (network/parse error).
type Status int

const (
	StatusPass Status = iota
	StatusFail
	StatusWarn
	StatusSkip
	StatusError
)

func (s Status) String() string {
	switch s {
	case StatusPass:
		return "PASS"
	case StatusFail:
		return "FAIL"
	case StatusWarn:
		return "WARN"
	case StatusSkip:
		return "SKIP"
	default:
		return "ERROR"
	}
}

// Finding is the result of one safeguard probe.
type Finding struct {
	ID          string   `json:"id"`
	Category    string   `json:"category"`
	Title       string   `json:"title"`
	Severity    Severity `json:"-"`
	SeverityStr string   `json:"severity"`
	Status      Status   `json:"-"`
	StatusStr   string   `json:"status"`
	Detail      string   `json:"detail,omitempty"`
	Evidence    string   `json:"evidence,omitempty"`
	Remediation string   `json:"remediation,omitempty"`
	// Expectation names the -expect key this finding was judged against
	// (see expecting). A FAIL carrying one always sets exit status 1.
	Expectation string `json:"expectation,omitempty"`
	// Ignored marks a finding named by -ignore: it keeps its status but is
	// counted apart and never affects the exit status.
	Ignored bool `json:"ignored,omitempty"`
}

// finding builds the finding id with its category and title from the catalog,
// and keeps the string mirrors of the enum fields (used for JSON output) in
// sync with the typed values. What varies with the outcome goes in detail.
func finding(id string, sev Severity, st Status, detail string) Finding {
	entry := catalogEntryFor(id)
	return Finding{
		ID:          id,
		Category:    entry.Category,
		Title:       entry.Title,
		Severity:    sev,
		SeverityStr: sev.String(),
		Status:      st,
		StatusStr:   st.String(),
		Detail:      detail,
	}
}

// withEvidence and withRemediation are fluent helpers for building findings.
func (f Finding) withEvidence(format string, a ...any) Finding {
	f.Evidence = fmt.Sprintf(format, a...)
	return f
}

func (f Finding) withRemediation(s string) Finding {
	f.Remediation = s
	return f
}

// check is one probe in the registry. ids lists every finding ID it can emit;
// their catalog entries give the check its category and its Mode (all of them
// agree, which TestRegistryDeclaresEveryCatalogID checks).
type check struct {
	ids  []string
	run  func(s *Scanner) []Finding
	hint func(s *Scanner) string // optional: why the check may take a while, shown in its progress line
}

// entry returns the catalog entry of the check's first finding ID, which
// carries the category and mode of all of them.
func (c check) entry() catalogEntry {
	return catalogEntryFor(c.ids[0])
}

// Scanner holds connection state and shared HTTP/WS/raw helpers used by every
// check. It is created once per target and reused across all probes.
type Scanner struct {
	base       *url.URL // scheme + host + base path; no trailing slash beyond base
	rawBase    string   // original -target string, for messages
	client     *http.Client
	insecure   bool
	timeout    time.Duration
	dosWait    time.Duration // how long DoS probes wait for the server to reap a slow/idle connection
	aggressive bool
	fuzz       bool
	fuzzIter   int
	seed       int64
	rng        *rand.Rand
	rngMu      sync.Mutex
	verbose    bool            // log every request to log
	progress   bool            // log one line per check as it starts
	log        io.Writer       // request and progress lines; never the report
	expect     map[string]bool // the -expect keys declared

	// How the target is reached: every connection goes to address over
	// network ("tcp", or "unix" with -unix; see dial), and every request
	// carries the -H headers (see addExtraHeaders).
	network string
	address string
	extra   []extraHeader

	// ctx is the run's context (Run sets it): every request and dial derives
	// from it, so cancelling it aborts what is in flight.
	ctx         context.Context
	interrupted bool // the run was cancelled before every check ran
	notRun      int  // how many checks it did not run, or cut short

	username string
	password string

	metricsToken string // -metrics-token, for PROM-003; never printed

	// Discovered during the discovery phase.
	reachable bool // the target answered HTTP
	kula      bool // and identified as Kula
	https     bool
	// authEnabled is whether web.auth is on, as /api/auth/status says. It
	// is false, too, while api says the auth posture is unknown.
	authEnabled bool
	// api says whether discovery could read the auth posture, or why not;
	// apiAnswer says what GET /api/auth/status got when it could not. While
	// it is unknown, only the checks runsWithoutAPI names run.
	api       apiState
	apiAnswer string
	// ollama is whether the AI assistant is enabled, as /api/config says;
	// checks ask assistantEnabled. ollamaKnown: /api/config was read, by
	// discovery or, behind auth, with the session; ollamaReadErr says why
	// that read with the session failed, once it did.
	ollama        bool
	ollamaKnown   bool
	ollamaReadErr string
	version       string // Kula's version, when /api/config discloses it
	// hostRefused: Kula's DNS-rebinding check refuses the Host name the
	// scanner sends (DISC-003), so the checks that would read that refusal
	// as their own result are skipped (runsUnderRefusedHost).
	hostRefused bool

	// Populated by ensureSession() when credentials are supplied and valid.
	session     string       // kula_session cookie value
	csrf        string       // csrf_token returned by login
	loginCookie *http.Cookie // the Set-Cookie issued at login, for attribute inspection
	loggedIn    bool
	loginTried  bool
	login       loginOutcome // how the login attempt ended
	loginDetail string       // why it failed, for AUTH-005
	sessionLost string       // why the session ended before the run did (markSessionLost)
}

// apiState is what discovery learnt of the UI and API at the scanned address:
// whether /api/auth/status told the auth posture, and if not, why.
type apiState int

const (
	// apiServed: /api/auth/status answered Kula's JSON, or Kula's Host check
	// refused it, which Kula does only with auth off. authEnabled holds.
	apiServed apiState = iota
	// apiAbsent: Kula serves no UI or API at this address. /api/auth/status
	// answered a 404 that is not Kula's JSON, as Kula with web.ui: false
	// does for every route but /health, /status and /metrics, or a proxy
	// that forwards only those.
	apiAbsent
	// apiUnreadable: /api/auth/status could not be read. The request failed
	// or timed out, or the answer was a 5xx or anything else but Kula's
	// JSON. The run is inconclusive (AUTH-000 is an ERROR).
	apiUnreadable
)

// loginOutcome is how ensureSession's login attempt ended.
type loginOutcome int

const (
	loginNotTried  loginOutcome = iota
	loginOK                     // a session is open
	loginRefused                // 401: wrong username or password
	loginThrottled              // 429: the login rate limiter refused the attempt
	loginFailed                 // the request failed, or another answer
)

// Options configures a Scanner. Only Target is required; everything else has a
// sensible zero/default.
type Options struct {
	Target     string
	BasePath   string
	Username   string
	Password   string
	Timeout    time.Duration
	DoSWait    time.Duration
	Insecure   bool
	Aggressive bool
	Fuzz       bool
	FuzzIter   int
	Seed       int64     // 0 → a random seed is chosen and reported
	Verbose    bool      // log each request and its response status
	Progress   bool      // log one line per check as it starts
	Log        io.Writer // where Verbose and Progress write; nil means standard error
	Expect     []string  // -expect keys (see expectKeys): the posture the target declares
	// Connect (host:port) or Unix (a socket path) sends every connection
	// there instead of to the target URL's address; the URL still gives the
	// Host header, the TLS server name, Origin values and the base path.
	Connect string
	Unix    string
	// Headers are -H "Name: value" headers sent on every request, for a
	// gateway in front of Kula. Their values are never printed.
	Headers []string
	// MetricsToken is the Prometheus bearer token /metrics should accept
	// (PROM-003). It is never printed.
	MetricsToken string
}

// NewScanner builds a Scanner for opt.Target. opt.BasePath, when non-empty, is
// appended to every request path (kula's web.base_path feature).
func NewScanner(opt Options) (*Scanner, error) {
	u, err := url.Parse(strings.TrimSpace(opt.Target))
	if err != nil {
		return nil, fmt.Errorf("invalid target URL %q: %w", opt.Target, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("target must use http or https scheme, got %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("target URL %q has no host", opt.Target)
	}

	// Fold a path component in the URL into basePath unless one was given
	// explicitly. This lets `kula-scan http://host:1234/kula` work.
	bp, source := opt.BasePath, "-base-path"
	if bp == "" {
		bp, source = u.Path, "base path in the target URL"
	}
	if bp, err = normalizeBasePath(bp); err != nil {
		return nil, fmt.Errorf("invalid %s: %w", source, err)
	}
	u.Path = bp
	u.RawPath = ""
	u.RawQuery = ""
	u.Fragment = ""

	network, address, err := dialTarget(u, opt.Connect, opt.Unix)
	if err != nil {
		return nil, err
	}
	extra, err := parseExtraHeaders(opt.Headers)
	if err != nil {
		return nil, err
	}

	if opt.Timeout <= 0 {
		opt.Timeout = 10 * time.Second
	}
	if opt.DoSWait <= 0 {
		opt.DoSWait = 35 * time.Second
	}
	if opt.FuzzIter <= 0 {
		opt.FuzzIter = 200
	}
	if opt.Seed == 0 {
		opt.Seed = time.Now().UnixNano()
	}
	if opt.Log == nil {
		opt.Log = os.Stderr
	}

	expect := map[string]bool{}
	for _, key := range opt.Expect {
		expect[key] = true
	}

	s := &Scanner{
		base:       u,
		rawBase:    opt.Target,
		network:    network,
		address:    address,
		extra:      extra,
		insecure:   opt.Insecure,
		timeout:    opt.Timeout,
		dosWait:    opt.DoSWait,
		aggressive: opt.Aggressive,
		fuzz:       opt.Fuzz,
		fuzzIter:   opt.FuzzIter,
		seed:       opt.Seed,
		rng:        rand.New(rand.NewSource(opt.Seed)), //nolint:gosec // nosemgrep: go.lang.security.audit.crypto.math_random.math-random-used -- fuzz-input randomness, reproducible via -seed; not security-sensitive
		verbose:    opt.Verbose,
		progress:   opt.Progress,
		log:        opt.Log,
		expect:     expect,
		ctx:        context.Background(),
		username:   opt.Username,
		password:   opt.Password,
		https:      u.Scheme == "https",
	}
	s.metricsToken = opt.MetricsToken
	s.client = &http.Client{
		Timeout: opt.Timeout,
		Transport: &http.Transport{
			// Every request goes where dial connects, whatever its URL names.
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return s.dial(ctx)
			},
			TLSClientConfig:        s.tlsConfig(),
			ForceAttemptHTTP2:      true,
			MaxResponseHeaderBytes: maxResponseHeaderBytes,
		},
		// Observe redirects (301/302) directly rather than following them, so
		// base-path redirects and auth bounces are visible to the checks.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return s, nil
}

// normalizeBasePath returns the base path p names with one leading slash and
// no trailing one, or "" for none. Every request path is appended to it, raw
// requests' too, so it refuses what is not a plain path: a query (?), a
// fragment (#), whitespace or a control character, or a URL with a scheme.
func normalizeBasePath(p string) (string, error) {
	if strings.ContainsFunc(p, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return "", fmt.Errorf("%q contains whitespace or a control character", p)
	}
	if strings.ContainsAny(p, "?#") {
		return "", fmt.Errorf("%q contains a query or fragment (? or #); give the path alone, e.g. /kula", p)
	}
	if hasScheme(p) {
		return "", fmt.Errorf("%q is a URL; give the path alone, e.g. /kula, and the address in the target URL", p)
	}
	return strings.TrimRight("/"+strings.TrimLeft(p, "/"), "/"), nil
}

// hasScheme reports whether p starts with a URL scheme ("http:", "https:"):
// letters, then letters, digits, "+", "-" or ".", then a colon, all before
// any slash.
func hasScheme(p string) bool {
	scheme, _, found := strings.Cut(p, ":")
	if !found || scheme == "" || strings.Contains(scheme, "/") {
		return false
	}
	for i, r := range scheme {
		letter := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
		other := r >= '0' && r <= '9' || r == '+' || r == '-' || r == '.'
		if !letter && (i == 0 || !other) {
			return false
		}
	}
	return true
}

// dialTarget returns where the scanner's connections go: the -unix socket,
// the -connect address, or the host and port the target URL names.
func dialTarget(u *url.URL, connect, unix string) (network, address string, err error) {
	switch {
	case connect != "" && unix != "":
		return "", "", errors.New("-connect and -unix both say where to connect; pass one of them")
	case unix != "":
		return "unix", unix, nil
	case connect != "":
		if !validHostPort(connect) {
			return "", "", fmt.Errorf("invalid -connect %q: want host:port, e.g. 127.0.0.1:27960 or [::1]:27960", connect)
		}
		return "tcp", connect, nil
	}
	return "tcp", urlAddress(u), nil
}

// validHostPort reports whether s is a host and a port number.
func validHostPort(s string) bool {
	host, port, err := net.SplitHostPort(s)
	if err != nil || host == "" {
		return false
	}
	n, err := strconv.ParseUint(port, 10, 16)
	return err == nil && n > 0
}

// urlAddress returns the host and port u names, with the scheme's default
// port when it names none. An IPv6 literal keeps its brackets: [::1]:80.
func urlAddress(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	return net.JoinHostPort(u.Hostname(), port)
}

// dial opens a connection to the target. Every connection the scanner makes
// starts here: the HTTP client's, the raw-socket probes' (dialRaw), the
// WebSocket handshakes' (wsDial) and the TLS check's (dialTLS). ctx and
// -timeout bound the dial; the caller adds TLS and, for a connection it holds,
// closeOnCancel.
func (s *Scanner) dial(ctx context.Context) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: s.timeout}
	return dialer.DialContext(ctx, s.network, s.address)
}

// tlsConfig returns the TLS client configuration of every TLS connection the
// scanner makes. The server name, sent as SNI and checked against the
// certificate, is always the target URL's host name, wherever dial connects.
func (s *Scanner) tlsConfig() *tls.Config {
	// kula-scan probes arbitrary targets; the Go client default min (TLS 1.2) is correct, and InsecureSkipVerify is the opt-in below.
	// nosemgrep: missing-ssl-minversion
	return &tls.Config{ServerName: s.base.Hostname(), InsecureSkipVerify: s.insecure} //nolint:gosec // -insecure is an explicit opt-in for self-signed test instances
}

// dialTLS opens a TLS connection to the target through dial and completes
// the handshake. ctx ends the dial and the handshake, which -timeout bounds
// together; the established connection outlives it.
func (s *Scanner) dialTLS(ctx context.Context) (*tls.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	conn, err := s.dial(ctx)
	if err != nil {
		return nil, err
	}
	tlsConn := tls.Client(conn, s.tlsConfig())
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return tlsConn, nil
}

// randIntn returns a non-negative pseudo-random int in [0,n) from the seeded
// fuzz RNG. It is safe for the concurrent goroutines some fuzz probes spawn.
func (s *Scanner) randIntn(n int) int {
	if n <= 0 {
		return 0
	}
	s.rngMu.Lock()
	defer s.rngMu.Unlock()
	return s.rng.Intn(n)
}

// urlFor joins the configured base (scheme+host+base path) with a root-relative
// request path like "/api/login".
func (s *Scanner) urlFor(path string) string {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return s.base.Scheme + "://" + s.base.Host + s.base.Path + path
}

// httpResult bundles the parts of a response the checks care about.
type httpResult struct {
	status int
	header http.Header
	body   string
	err    error

	// The request as the probe named it: its method and its path relative
	// to the base path. unsent marks an err that came before the request
	// was written: the client could not build it or found no connection to
	// write it on, so the target saw none of it.
	method, path string
	unsent       bool
}

// do issues an HTTP request to path with optional headers and body, returning a
// compact result. Cookies are never persisted between calls (the client has no
// jar); callers set Cookie/Authorization headers explicitly. The -H headers
// are added (see addExtraHeaders). The request ends with the run: an
// interrupt aborts it.
func (s *Scanner) do(method, path string, headers map[string]string, body string) httpResult {
	return s.doContext(s.ctx, method, path, headers, body)
}

// doContext is do under parent instead of the run's context.
func (s *Scanner) doContext(parent context.Context, method, path string, headers map[string]string, body string) httpResult {
	ctx, cancel := context.WithTimeout(parent, s.timeout)
	defer cancel()
	result := httpResult{method: method, path: path}

	// wrote turns true once the whole request is written to a connection.
	var wrote atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				wrote.Store(true)
			}
		},
	})

	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.urlFor(path), rdr)
	if err != nil {
		if s.verbose {
			_, _ = fmt.Fprintf(s.log, "    %s %s -> not sent: %s\n", printable(method), printable(path), printable(err.Error()))
		}
		result.err, result.unsent = err, true
		return result
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	s.addExtraHeaders(req.Header)
	resp, err := s.client.Do(req)
	if err != nil {
		if s.verbose {
			_, _ = fmt.Fprintf(s.log, "    %s %s -> %s\n", printable(method), printable(path), printable(err.Error()))
		}
		result.err, result.unsent = err, !wrote.Load()
		return result
	}
	defer func() { _ = resp.Body.Close() }()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if s.verbose {
		_, _ = fmt.Fprintf(s.log, "    %s %s -> %d (%d bytes)\n", printable(method), printable(path), resp.StatusCode, len(raw))
	}
	result.status, result.header, result.body = resp.StatusCode, resp.Header, string(raw)
	return result
}

// authedHeaders returns request headers carrying the active session cookie (and
// matching CSRF token when csrf is true), or nil when no session is available.
func (s *Scanner) authedHeaders(csrf bool) map[string]string {
	if s.session == "" {
		return nil
	}
	h := map[string]string{"Cookie": "kula_session=" + s.session}
	if csrf && s.csrf != "" {
		h["X-CSRF-Token"] = s.csrf
	}
	return h
}

// dialRaw opens a raw connection to the target (TLS when https). The caller
// owns the returned conn and must close it. DoS probes use this to drive the
// socket directly (partial requests, oversized headers); rawTCP layers a
// single request/response on top. The connection is closed when the run is
// interrupted.
func (s *Scanner) dialRaw() (net.Conn, error) {
	var conn net.Conn
	var err error
	if s.https {
		conn, err = s.dialTLS(s.ctx)
	} else {
		conn, err = s.dial(s.ctx)
	}
	if err != nil {
		return nil, err
	}
	return s.closeOnCancel(conn), nil
}

// closeOnCancel ties conn to the run: an interrupt closes it, so a probe
// blocked on it (a DoS probe waiting out -dos-wait) returns at once.
func (s *Scanner) closeOnCancel(conn net.Conn) net.Conn {
	stop := context.AfterFunc(s.ctx, func() { _ = conn.Close() })
	return &runConn{Conn: conn, stop: stop}
}

// runConn is a connection closeOnCancel watches; closing it stops the watch.
type runConn struct {
	net.Conn
	stop func() bool
}

func (c *runConn) Close() error {
	c.stop()
	return c.Conn.Close()
}

// rawTCP opens a raw connection (TLS when the target is https) and writes a
// literal HTTP/1.1 request line, so the path is sent verbatim without any
// client-side URL normalisation, followed by the Host line, the -H headers
// and Connection: close. This is how traversal payloads are delivered.
//
// It returns the status code and body. An error means the target said
// nothing to the request: either it was not delivered (the connection or the
// write failed), or the target took it and, within -timeout, neither
// answered nor closed the connection (errNoAnswer). A status of 0 without an
// error means the target took the request and closed or reset the
// connection without a parsable response: it rejected the request at the
// protocol layer (an acceptable, safe outcome).
func (s *Scanner) rawTCP(requestLine string) (int, string, error) {
	return s.rawExchange(fmt.Sprintf("%s\r\nHost: %s\r\n%sConnection: close\r\n\r\n", requestLine, s.base.Host, s.rawHeaderLines(nil)))
}

// rawExchange writes a fully caller-supplied raw HTTP request (request line,
// headers, and body verbatim) and returns the response status and body. It is
// used by the request-smuggling probes that need conflicting/duplicate framing
// headers the net/http client refuses to send; a caller adds the -H headers
// with rawHeaderLines. As with rawTCP, an error means the request was not
// delivered or not answered within -timeout (errNoAnswer), and a status of 0
// without one that the target closed or rejected the connection at the
// protocol layer.
func (s *Scanner) rawExchange(raw string) (int, string, error) {
	conn, err := s.dialRaw()
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(s.timeout))

	if _, err := io.WriteString(conn, raw); err != nil {
		return 0, "", err
	}
	status, body, answered := readRawResponse(conn)
	if !answered {
		return 0, "", fmt.Errorf("%w within -timeout (%s): the target neither responded nor closed the connection", errNoAnswer, s.timeout)
	}
	return status, body, nil
}

// errNoAnswer marks a raw request the target took and left unanswered: the
// read reached the connection's deadline before a response or a close.
// Silence proves nothing about the request, so a probe that meets it reports
// an ERROR (see undelivered), never a refusal at the protocol layer.
var errNoAnswer = errors.New("no answer")

// readRawResponse reads the response to a request written on conn: its
// status and body, or status 0 when the target closed or reset the
// connection without a parsable response. answered is false when the read
// reached conn's deadline first: the target neither responded nor closed.
func readRawResponse(conn net.Conn) (status int, body string, answered bool) {
	resp, err := readResponse(conn)
	switch {
	case err != nil && isReadTimeout(err):
		return 0, "", false
	case err != nil:
		return 0, "", true
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodyBytes))
	return resp.StatusCode, string(raw), true
}

// undelivered counts the raw requests of a probe that drew nothing from the
// target: rawTCP or rawExchange returned an error because the request was
// not delivered, or because the target took it and did not answer within
// -timeout (errNoAnswer). Such a request proves nothing either way: a probe
// that lost any reports an ERROR rather than a PASS.
type undelivered struct {
	n      int   // requests that drew nothing
	first  error // the first one not delivered
	silent int   // of n, delivered and not answered
	quiet  error // the first one not answered
}

func (u *undelivered) add(err error) {
	u.n++
	if errors.Is(err, errNoAnswer) {
		if u.silent == 0 {
			u.quiet = err
		}
		u.silent++
		return
	}
	if u.first == nil {
		u.first = err
	}
}

// detail says how many of total requests, named by what, drew nothing, and
// why: not delivered, or not answered.
func (u *undelivered) detail(total int, what string) string {
	switch lost := u.n - u.silent; {
	case u.silent == 0:
		return fmt.Sprintf("%d of %d %s could not be delivered over a raw socket (%v)", u.n, total, what, u.first)
	case lost == 0:
		return fmt.Sprintf("%d of %d %s got %v", u.n, total, what, u.quiet)
	default:
		return fmt.Sprintf("%d of %d %s drew nothing: %d could not be delivered over a raw socket (%v), and %d got %v",
			u.n, total, what, lost, u.first, u.silent, u.quiet)
	}
}

// Bounds on what a target can make the scanner hold. Kula's responses are far
// smaller; a hostile target could otherwise stream one endless header line
// into memory until the request deadline.
const (
	maxResponseHeaderBytes = 2 << 20 // status line and headers
	maxResponseBodyBytes   = 1 << 20 // body, read by the caller
	maxWebSocketMessage    = 1 << 20 // one message the target sends
)

var errResponseTooLarge = errors.New("response headers larger than 2 MiB")

// readResponse reads a response from a raw connection, failing once the status
// line and headers pass maxResponseHeaderBytes. The caller still limits the
// body to maxResponseBodyBytes.
func readResponse(conn net.Conn) (*http.Response, error) {
	limited := &io.LimitedReader{R: conn, N: maxResponseHeaderBytes}
	resp, err := http.ReadResponse(bufio.NewReader(limited), nil)
	if err != nil {
		if limited.N <= 0 {
			return nil, errResponseTooLarge
		}
		return nil, err
	}
	limited.N = maxResponseBodyBytes
	return resp, nil
}

// budgetConn fails reads past a byte budget, so the WebSocket handshake, which
// gorilla reads with http.ReadResponse, cannot buffer an endless header. The
// budget is lifted once the handshake is done; from then on the scanner limits
// each message it reads instead.
type budgetConn struct {
	net.Conn
	remaining int64 // < 0: no budget
}

func (c *budgetConn) Read(p []byte) (int, error) {
	if c.remaining < 0 {
		return c.Conn.Read(p)
	}
	if c.remaining == 0 {
		return 0, errResponseTooLarge
	}
	if int64(len(p)) > c.remaining {
		p = p[:c.remaining]
	}
	n, err := c.Conn.Read(p)
	c.remaining -= int64(n)
	return n, err
}

// wsDial attempts a WebSocket upgrade to path with the given extra headers,
// and the -H headers (see addExtraHeaders), returning the HTTP response (for
// status inspection) and the connection (nil on failure). The caller is
// responsible for closing a non-nil connection.
func (s *Scanner) wsDial(path string, headers http.Header) (*websocket.Conn, *http.Response, error) {
	scheme := "ws"
	if s.https {
		scheme = "wss"
	}
	wsURL := scheme + "://" + s.base.Host + s.base.Path + path
	headers = cloneHeader(headers)
	s.addExtraHeaders(headers)

	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = s.timeout
	// The handshake goes where dial connects, as the HTTP client's requests
	// do, never through a proxy from the environment.
	dialer.Proxy = nil
	dialer.TLSClientConfig = s.tlsConfig()
	// Dial calls this in the calling goroutine and is done with the budget
	// when it returns. The connection is closed when the run is interrupted.
	var budgeted []*budgetConn
	dialer.NetDialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		conn, err := s.dial(ctx)
		if err != nil {
			return nil, err
		}
		budget := &budgetConn{Conn: s.closeOnCancel(conn), remaining: maxResponseHeaderBytes}
		budgeted = append(budgeted, budget)
		return budget, nil
	}
	conn, resp, err := dialer.DialContext(s.ctx, wsURL, headers)
	for _, budget := range budgeted {
		budget.remaining = -1
	}
	if conn != nil {
		conn.SetReadLimit(maxWebSocketMessage)
	}
	return conn, resp, err
}

// ---- discovery ---------------------------------------------------------------

// discover probes the target to learn whether it is reachable and is Kula,
// whether auth is enabled, and which optional features (Ollama) are active.
// The findings it returns describe the target rather than judging it. A
// target that does not answer, or answers but is not Kula, gets a DISC-001
// ERROR and leaves s.reachable or s.kula false: the scan stops there. Kula
// refusing the scanner's Host name gets a DISC-003 ERROR and sets
// s.hostRefused.
func (s *Scanner) discover() []Finding {
	var out []Finding

	// The liveness endpoint and auth/status are open whatever the config;
	// auth/status reveals the auth posture.
	health := s.do(http.MethodGet, "/health", nil, "")
	status := s.do(http.MethodGet, "/api/auth/status", nil, "")
	if health.err != nil && status.err != nil {
		return []Finding{finding("DISC-001", SevInfo, StatusError,
			fmt.Sprintf("could not reach %s: %v", s.rawBase, status.err)).
			withRemediation("Check the target URL, port, and that kula's web server is enabled.")}
	}
	s.reachable = true

	// /api/config (open when auth is off; gated when on) reveals ollama
	// state; behind auth, assistantEnabled reads it with the session.
	cfg := s.do(http.MethodGet, "/api/config", nil, "")

	// Kula says so on /health, answers auth/status with auth_required, or
	// refuses the API to a Host name it does not serve (DNS-rebinding check).
	var authStatus map[string]json.RawMessage
	hasAuthRequired := false
	if status.err == nil && json.Unmarshal([]byte(status.body), &authStatus) == nil {
		_, hasAuthRequired = authStatus["auth_required"]
	}
	hostRefused := hostCheckRefusal(status) || hostCheckRefusal(cfg)
	s.kula = (health.err == nil && strings.Contains(health.body, "kula is healthy")) || hasAuthRequired || hostRefused
	if !s.kula {
		return []Finding{finding("DISC-001", SevInfo, StatusError,
			fmt.Sprintf("%s answers HTTP but does not look like Kula: /health does not say \"kula is healthy\" and /api/auth/status carries no auth_required.", s.urlFor(""))).
			withEvidence("/health: %s; /api/auth/status: %s", describeResult(health), describeResult(status)).
			withRemediation("Check the target URL and port; if Kula is mounted under a base path, include it in the URL or pass -base-path.")}
	}

	// The auth posture, or why it is unknown (s.api). A host check refusal
	// leaves authEnabled false, which it is: Kula guards the API with that
	// check only while auth is off.
	switch {
	case hasAuthRequired:
		if json.Unmarshal(authStatus["auth_required"], &s.authEnabled) != nil {
			s.api, s.apiAnswer = apiUnreadable, fmt.Sprintf("answered HTTP %d with an auth_required that is neither true nor false", status.status)
		}
	case hostRefused:
	case status.err != nil:
		s.api, s.apiAnswer = apiUnreadable, "failed: "+status.err.Error()
	case status.status == http.StatusNotFound && !kulaJSONError(status.body):
		s.api, s.apiAnswer = apiAbsent, "answered a plain HTTP 404, not Kula's JSON"
	default:
		s.api, s.apiAnswer = apiUnreadable, fmt.Sprintf("answered HTTP %d without Kula's auth_required", status.status)
	}
	if cfg.status == http.StatusOK {
		s.learnConfig(cfg.body)
	}

	authState := "authentication disabled"
	switch {
	case s.api == apiAbsent:
		authState = "no UI or API served at this address (web.ui: false?), so authentication is unknown"
	case s.api == apiUnreadable:
		authState = "authentication unknown: GET /api/auth/status " + s.apiAnswer
	case s.authEnabled:
		authState = "authentication enabled"
	}
	assistant := fmt.Sprintf("ollama %v", s.ollama)
	switch {
	case s.ollamaKnown:
	case s.authEnabled:
		assistant = "ollama not visible without a session"
	case s.api != apiServed:
		assistant = "ollama unknown"
	}
	detail := fmt.Sprintf("reachable at %s (%s); %s; %s",
		s.urlFor(""), s.base.Scheme, authState, assistant)
	if s.version != "" {
		detail += "; version " + s.version
	}
	if hostRefused {
		detail += "; Kula refuses this Host name (its DNS-rebinding check; see DISC-003)"
	}
	out = append(out, finding("DISC-001", SevInfo, StatusPass, detail))
	if hostRefused {
		s.hostRefused = true
		path, refusal := "/api/auth/status", status
		if !hostCheckRefusal(status) {
			path, refusal = "/api/config", cfg
		}
		out = append(out, s.hostRefusedFinding(path, refusal))
	}

	// Plaintext transport with auth enabled means credentials cross the wire in
	// the clear unless a TLS-terminating proxy sits in front.
	if s.authEnabled && !s.https {
		out = append(out, finding("DISC-002", SevMedium, StatusWarn,
			"authentication is enabled but the target was reached over plain HTTP; login credentials and session cookies are exposed unless a TLS-terminating reverse proxy sits in front.").
			withRemediation("Serve kula behind HTTPS (reverse proxy + trust_proxy, or terminate TLS) so the Secure cookie flag and HSTS apply.").
			expecting("tls"))
	}

	return out
}

// targetInfo returns what discovery learnt, and behind auth what the run's
// session read of /api/config (assistantEnabled), for the report. Its
// AuthEnabled is nil while the auth posture is unknown (s.api).
func (s *Scanner) targetInfo() TargetInfo {
	info := TargetInfo{HTTPS: s.https, Ollama: s.ollama, Version: s.version}
	if s.api == apiServed {
		info.AuthEnabled = &s.authEnabled
	}
	return info
}

// kulaJSONError reports whether body is Kula's JSON error, {"error": …}.
func kulaJSONError(body string) bool {
	_, ok := kulaErrorOf(body)
	return ok
}

// apiMissing says why the auth posture is unknown, for a detail: what
// GET /api/auth/status got, and for apiAbsent that Kula serves no UI or API
// at the scanned address. It ends without a period.
func (s *Scanner) apiMissing() string {
	if s.api == apiAbsent {
		return "Kula serves no UI or API at this address (web.ui: false, or a proxy that forwards only /health, /status and /metrics): GET /api/auth/status " + s.apiAnswer
	}
	return "GET /api/auth/status " + s.apiAnswer + ", so whether web.auth is enabled is unknown"
}

// runsWithoutAPI reports whether check c still judges something while the
// auth posture is unknown: Kula serves no UI or API at the scanned address,
// or /api/auth/status could not be read. These need neither, nor the auth
// posture: TLS, below HTTP; /metrics, which Kula serves whatever web.ui says
// and guards with its own bearer token; REDIR-001, whose crafted paths reach
// the router and the base-path redirect on any route; the slow-request,
// oversized-header and idle-connection probes, which judge the connection
// handling with /health as their liveness probe; FUZZ-PATH, which fuzzes
// whatever routes the server has; and FUZZ-LIVENESS, which asks /health.
// Every other check reads the UI page, the API, the WebSocket or the static
// files, or chooses what to probe by the auth posture (HOST-001): run
// without them, it would judge a 404 or a failed request as Kula's answer.
func runsWithoutAPI(c check) bool {
	switch c.entry().Category {
	case "tls", "metrics":
		return true
	}
	for _, id := range c.ids {
		switch id {
		case "REDIR-001", "DOS-SLOWLORIS", "DOS-HEADERBOMB", "DOS-CONNFLOOD", "FUZZ-PATH", "FUZZ-LIVENESS":
			return true
		}
	}
	return false
}

// apiSkippedFindings reports check c, not run while the auth posture is
// unknown (runsWithoutAPI): one finding per ID it would have emitted. Each is
// a SKIP saying why, but AUTH-000, the auth posture itself (authPosture), and
// a finding that judges a declared -expect key, which the run cannot judge:
// an ERROR, never a pass.
func (s *Scanner) apiSkippedFindings(c check) []Finding {
	out := make([]Finding, 0, len(c.ids))
	for _, id := range c.ids {
		key := expectKeyOf(id)
		switch {
		case id == "AUTH-000":
			out = append(out, s.authPosture())
		case key != "" && s.expects(key):
			out = append(out, finding(id, SevInfo, StatusError,
				fmt.Sprintf("-expect %s cannot be judged: %s.", key, s.apiMissing())).expecting(key))
		default:
			see := ""
			if s.api == apiUnreadable {
				see = " (see AUTH-000)"
			}
			out = append(out, finding(id, SevInfo, StatusSkip,
				s.apiMissing()+"; this check needs the UI or API, so it did not run"+see+"."))
		}
	}
	return out
}

// authPosture is AUTH-000 while the auth posture is unknown. Without a UI or
// API there is no authentication of theirs to judge, so it SKIPs, unless
// -expect auth asks for one; a posture that could not be read leaves the run
// inconclusive. Either of those is an ERROR.
func (s *Scanner) authPosture() Finding {
	switch {
	case s.expects("auth"):
		return finding("AUTH-000", SevInfo, StatusError,
			fmt.Sprintf("-expect auth cannot be judged: %s.", s.apiMissing())).
			expecting("auth")
	case s.api == apiAbsent:
		return finding("AUTH-000", SevInfo, StatusSkip,
			s.apiMissing()+", so there is no authentication of the UI or API to judge.")
	default:
		return finding("AUTH-000", SevInfo, StatusError,
			s.apiMissing()+", and the checks that need the UI or API were skipped. "+
				"Check that the URL reaches Kula's web server, and that a proxy in front forwards /api/.")
	}
}

// learnConfig keeps what a body of /api/config, answered 200, says of the
// target: whether the AI assistant is enabled, and Kula's version when it
// discloses one. It reports false, learning nothing, when the body is not
// that JSON.
func (s *Scanner) learnConfig(body string) bool {
	var c struct {
		OllamaEnabled bool   `json:"ollama_enabled"`
		Version       string `json:"version"`
	}
	if json.Unmarshal([]byte(body), &c) != nil {
		return false
	}
	s.ollama, s.ollamaKnown = c.OllamaEnabled, true
	if c.Version != "" {
		s.version = c.Version
	}
	return true
}

// assistantEnabled reports whether the target's AI assistant is enabled, as
// /api/config's ollama_enabled says. Discovery reads it when /api/config
// answers without a session, as it does with auth off. Behind auth the first
// call made while the run's session is open reads it with that session, once,
// and keeps the answer, which the report's target_info then shows. It never
// logs in itself, so a run that opens no session reads nothing: a check that
// needs the assistant opens the session first, as it needs one to reach it.
//
// st is StatusPass when enabled is the answer. Otherwise the answer is not
// known and why says why: StatusSkip without a session, StatusError when the
// read failed. why ends without a period.
func (s *Scanner) assistantEnabled() (enabled bool, st Status, why string) {
	switch {
	case !s.ollamaKnown && s.api != apiServed:
		return false, StatusSkip, s.apiMissing()
	case s.ollamaKnown || !s.authEnabled:
		// With auth off discovery could read it: what it learnt stands.
		return s.ollama, StatusPass, ""
	case s.ollamaReadErr != "":
		return false, StatusError, s.ollamaReadErr
	case !s.loggedIn:
		return false, StatusSkip, "auth is enabled and no session is available to read whether the AI assistant is enabled: " + s.noSessionReason()
	}
	r := s.do(http.MethodGet, "/api/config", s.authedHeaders(false), "")
	switch {
	case r.err != nil:
		s.ollamaReadErr = "could not read /api/config to learn whether the AI assistant is enabled: " + r.err.Error()
	case r.status != http.StatusOK || !s.learnConfig(r.body):
		s.ollamaReadErr = fmt.Sprintf("/api/config answered HTTP %d to the session, not the configuration, so whether the AI assistant is enabled is unknown", r.status)
	default:
		return s.ollama, StatusPass, ""
	}
	return false, StatusError, s.ollamaReadErr
}

// hostCheckRefusal reports whether r is Kula's refusal of a Host name it does
// not serve: a 403 whose JSON error starts "host not allowed".
func hostCheckRefusal(r httpResult) bool {
	if r.err != nil || r.status != http.StatusForbidden {
		return false
	}
	var body struct {
		Error string `json:"error"`
	}
	return json.Unmarshal([]byte(r.body), &body) == nil && strings.HasPrefix(body.Error, "host not allowed")
}

// describeResult renders how a request ended, for evidence.
func describeResult(r httpResult) string {
	if r.err != nil {
		return r.err.Error()
	}
	return fmt.Sprintf("HTTP %d", r.status)
}

// ensureSession logs in once (lazily) using the supplied credentials and caches
// the resulting session cookie + CSRF token. It is a no-op without credentials
// or when auth is disabled. Returns true when an authenticated session exists;
// otherwise s.login and s.loginDetail say why not.
func (s *Scanner) ensureSession() bool {
	if s.loggedIn {
		return true
	}
	if s.loginTried {
		return false
	}
	s.loginTried = true
	if s.username == "" || s.password == "" {
		return false
	}
	if err := s.ctx.Err(); err != nil {
		// Interrupted: a session opened now would only be logged out.
		s.login, s.loginDetail = loginFailed, "the run was interrupted before the login"
		return false
	}

	r := s.sessionLogin()
	switch {
	case r.err != nil:
		s.login, s.loginDetail = loginFailed, "the login request failed: "+r.err.Error()
	case r.status == http.StatusUnauthorized:
		s.login, s.loginDetail = loginRefused, "the target refused them (HTTP 401): wrong username or password"
	case r.status == http.StatusTooManyRequests:
		s.login, s.loginDetail = loginThrottled, "login is rate-limited for this account or address (HTTP 429); retry in five minutes"
	case r.status != http.StatusOK:
		s.login, s.loginDetail = loginFailed, fmt.Sprintf("the login request answered HTTP %d", r.status)
	default:
		for _, c := range (&http.Response{Header: r.header}).Cookies() {
			if c.Name == "kula_session" {
				s.session = c.Value
				s.loginCookie = c
			}
		}
		var parsed struct {
			CSRFToken string `json:"csrf_token"`
		}
		_ = json.Unmarshal([]byte(r.body), &parsed)
		s.csrf = parsed.CSRFToken
		s.loggedIn = s.session != ""
		if s.loggedIn {
			s.login = loginOK
		} else {
			s.login, s.loginDetail = loginFailed, "login answered HTTP 200 without a kula_session cookie"
		}
	}
	return s.loggedIn
}

// sessionLogin sends the run's login with the supplied credentials under a
// context an interrupt does not cancel, bounded by -timeout alone. Kula
// opens the session whether or not the client is still there to read the
// cookie; seeing the answer through captures the cookie, so the closing
// logout can end the session instead of leaving it open on the target until
// it expires.
func (s *Scanner) sessionLogin() httpResult {
	return s.loginContext(context.WithoutCancel(s.ctx), s.username, s.password)
}

// loginContext is loginRaw under parent instead of the run's context: a
// same-origin JSON login with username and password.
func (s *Scanner) loginContext(parent context.Context, username, password string) httpResult {
	b, err := json.Marshal(map[string]string{"username": username, "password": password})
	if err != nil {
		return httpResult{err: err}
	}
	headers := map[string]string{
		"Content-Type": "application/json",
		"Origin":       s.base.Scheme + "://" + s.base.Host,
	}
	return s.doContext(parent, http.MethodPost, "/api/login", headers, string(b))
}

// allChecks returns the full registry in execution order. Categories run
// grouped, but for the end of the run, which the login budget orders (below);
// aggressive and fuzz checks are tagged and filtered by the runner.
func allChecks() []check {
	var checks []check
	checks = append(checks, headerChecks()...)
	checks = append(checks, authChecks()...)
	checks = append(checks, csrfChecks()...)
	checks = append(checks, corsChecks()...)
	checks = append(checks, hostChecks()...)
	checks = append(checks, traversalChecks()...)
	checks = append(checks, metricsChecks()...)
	checks = append(checks, wsChecks()...)
	checks = append(checks, inputChecks()...)
	checks = append(checks, redirectChecks()...)
	checks = append(checks, integrityChecks()...)
	checks = append(checks, tlsChecks()...)
	checks = append(checks, aggressiveChecks()...)
	checks = append(checks, dosChecks()...)
	checks = append(checks, limitChecks()...)

	// The login budget. Kula admits 15 login attempts per client address in
	// five minutes, and 5 per account and address (loginIPAttemptLimit and
	// loginUserAttemptLimit in internal/web/auth.go). Every POST /api/login
	// that passes the CSRF gate counts against the first, whatever its body;
	// one whose body decodes counts against the second too. The probes that
	// spend attempts run first, so that each still reaches the code it tests:
	//
	//	the scan's login, by the first check that needs a session   1
	//	AUTH-006: a wrong password, known and unknown username       2
	//	INPUT-AGG: an 8 KiB body, refused by the 4 KiB cap           1
	//	FUZZ-BODY: malformed bodies, stopping at a 429            <= 6
	//	                                                            10 of 15
	//
	// Then the two that exhaust the limiter on purpose: RATE-LOGIN sends 8
	// for one account, 5 reach the handler and 3 are refused 429 (its PASS);
	// BYPASS-XFF's 12 then all find the address spent (its PASS, with
	// trust_proxy off). FUZZ-LIVENESS, which needs no login, ends the run.
	//
	// With web.security.origin_validation off, two more reach the limiter
	// before FUZZ-BODY: CSRF-001's and CSRF-002's logins without and with a
	// foreign Origin. And FUZZ-SMUGGLE's three ambiguously framed requests to
	// /api/login, which carry no Origin, pass the CSRF gate: net/http refuses
	// two of them before routing (two Content-Lengths, two
	// Transfer-Encodings) and one reaches the limiter, three at most on
	// another server. That is 13 attempts, 15 at most, before RATE-LOGIN:
	// FUZZ-BODY's six still reach the parser (attempts 7 to 12), AUTH-006 and
	// INPUT-AGG are not throttled, RATE-LOGIN still meets a 429 within its 8,
	// and BYPASS-XFF's 12 are all still refused.
	checks = append(checks, fuzzChecks()...)
	// RATE-CHAT exhausts the AI chat limiter, so it runs after FUZZ-BODY,
	// which sends the chat a few bodies of its own.
	checks = append(checks, chatRateChecks()...)
	checks = append(checks, check{ids: []string{"RATE-LOGIN"}, run: runLoginRateCheck})
	checks = append(checks, bypassChecks()...)
	checks = append(checks, fuzzLivenessCheck())
	return checks
}

// Run executes discovery and every check sel selects, returning the selected
// findings. Discovery always runs, and DISC-001, which says whether the target
// was scanned at all, is always reported. Findings are judged against the
// -expect posture as each check returns them.
//
// When Kula refuses the scanner's Host name (DISC-003), only the checks
// runsUnderRefusedHost names run; the others are skipped and say why, and
// DISC-003 is reported when it skipped any of them, unless -skip names it.
//
// When discovery could not read the auth posture (s.api), only the checks
// runsWithoutAPI names run; the others are skipped and say why
// (apiSkippedFindings). A posture that could not be read is an AUTH-000
// ERROR, reported when it skipped any check, unless -skip names it.
//
// Cancelling ctx interrupts the run: requests in flight abort, no further
// check starts, the findings of the check that was cut short are dropped,
// and s.interrupted and s.notRun record it. The session is logged out in
// any case; when the auth check ran, that closing logout is also the AUTH-010
// probe (closeSession).
func (s *Scanner) Run(ctx context.Context, sel *selection) []Finding {
	s.ctx = ctx

	// A check runs when any ID it declares is selected, and reports only
	// those.
	var checks []check
	for _, c := range allChecks() {
		if ids := sel.selectedIDs(c.ids); len(ids) > 0 {
			c.ids = ids
			checks = append(checks, c)
		}
	}
	runnable := 0
	for _, c := range checks {
		if s.modeEnabled(c.entry().Mode) {
			runnable++
		}
	}

	discovered := s.discover()
	if ctx.Err() != nil {
		// What discovery saw while being cancelled says nothing of the target.
		s.interrupted, s.notRun = true, runnable
		return nil
	}
	s.applyExpectations(discovered)

	// Under a refused Host name, the checks that would read the refusal as
	// their own result do not run; with the auth posture unknown, neither
	// do the checks that need the UI or API.
	refusedSkips, apiSkips := 0, 0
	postureReported := false // a check that runs, or is skipped, reports AUTH-000
	for _, c := range checks {
		postureReported = postureReported || slices.Contains(c.ids, "AUTH-000")
		switch {
		case !s.modeEnabled(c.entry().Mode):
		case s.hostRefused && !runsUnderRefusedHost(c):
			refusedSkips++
		case s.api != apiServed && !runsWithoutAPI(c):
			apiSkips++
		}
	}
	runnable -= refusedSkips + apiSkips

	var findings []Finding
	for _, f := range discovered {
		keep := f.ID == "DISC-001" || sel.selected(f.ID)
		if f.ID == "DISC-003" {
			keep = (keep || refusedSkips > 0) && !explicitlySkipped(sel, f.ID)
		}
		if keep {
			findings = append(findings, f)
		}
	}
	// An unreadable auth posture that kept selected checks from running
	// leaves the run inconclusive, which AUTH-000 says even when not
	// selected, unless -skip names it.
	if s.kula && s.api == apiUnreadable && apiSkips > 0 && !postureReported && !explicitlySkipped(sel, "AUTH-000") {
		findings = append(findings, s.authPosture())
	}

	// A target that does not answer, or is not Kula, is not probed further.
	if !s.reachable || !s.kula {
		return findings
	}
	// The closing logout below normally ends the session; this one covers a
	// check that panics.
	defer func() {
		ctx, cancel := s.closingContext()
		defer cancel()
		s.logout(ctx)
	}()

	revocation := false // the check declaring AUTH-010 ran: the closing logout is its probe
	started := 0
	for _, c := range checks {
		if ctx.Err() != nil {
			s.interrupted, s.notRun = true, runnable-started
			break
		}
		switch mode := c.entry().Mode; {
		case mode == ModeFuzz && !s.fuzz:
			findings = append(findings, skippedFindings(c,
				"blind fault-injection check not run; pass -fuzz to enable (it sends malformed/extreme input to the live target).")...)
			continue
		case mode == ModeAggressive && !s.aggressive:
			findings = append(findings, skippedFindings(c,
				"disruptive check not run; pass -aggressive to enable (it has side effects on the live target).")...)
			continue
		case s.hostRefused && !runsUnderRefusedHost(c):
			findings = append(findings, skippedFindings(c, s.refusedHostReason())...)
			continue
		case s.api != apiServed && !runsWithoutAPI(c):
			findings = append(findings, s.apiSkippedFindings(c)...)
			continue
		}
		started++
		s.logProgress(started, runnable, c)
		results := c.run(s)
		if ctx.Err() != nil {
			// Cut short: its requests failed for want of time, not because
			// of the target.
			s.interrupted, s.notRun = true, runnable-started+1
			break
		}
		revocation = revocation || slices.Contains(c.ids, "AUTH-010")
		s.applyExpectations(results)
		for _, f := range results {
			if sel.selected(f.ID) {
				findings = append(findings, f)
			}
		}
	}
	// Behind auth only the session can read whether the assistant is on;
	// while it is open, the report's target_info learns it (once a run).
	if s.loggedIn && !s.interrupted {
		s.assistantEnabled()
	}
	return s.closeSession(findings, revocation)
}

// modeEnabled reports whether checks of mode run on this scan.
func (s *Scanner) modeEnabled(mode Mode) bool {
	switch mode {
	case ModeAggressive:
		return s.aggressive
	case ModeFuzz:
		return s.fuzz
	default:
		return true
	}
}

// logProgress writes the line announcing the n-th of total checks, when
// progress is on.
func (s *Scanner) logProgress(n, total int, c check) {
	if !s.progress {
		return
	}
	ids := c.ids
	more := ""
	if len(ids) > 4 {
		more = fmt.Sprintf(" and %d more", len(ids)-3)
		ids = ids[:3]
	}
	hint := ""
	if c.hint != nil {
		hint = " (" + c.hint(s) + ")"
	}
	_, _ = fmt.Fprintf(s.log, "[%d/%d] %s: %s%s%s\n", n, total, c.entry().Category, strings.Join(ids, ", "), more, hint)
}

// skippedFindings reports a check that was not run: one SKIP finding per ID it
// would have emitted, each with its catalog title.
func skippedFindings(c check, detail string) []Finding {
	out := make([]Finding, 0, len(c.ids))
	for _, id := range c.ids {
		out = append(out, finding(id, SevInfo, StatusSkip, detail))
	}
	return out
}

// logoutTimeout bounds the closing logout, which an interrupted run still
// sends.
const logoutTimeout = 5 * time.Second

// closingContext bounds the closing logout and its probe. It derives from a
// fresh context, since the run's may have been cancelled by an interrupt.
func (s *Scanner) closingContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), min(s.timeout, logoutTimeout))
}

// logout ends the session ensureSession opened, so a run leaves no session
// behind on the target, and forgets it. It returns the session token it ended
// and the answer; ok is false when no session was open (none was opened, or
// a probe ended it: markSessionLost).
func (s *Scanner) logout(ctx context.Context) (token string, r httpResult, ok bool) {
	if !s.loggedIn {
		return "", httpResult{}, false
	}
	headers := s.authedHeaders(true)
	headers["Origin"] = s.base.Scheme + "://" + s.base.Host
	token = s.session
	r = s.doContext(ctx, http.MethodPost, "/api/logout", headers, "")
	s.loggedIn, s.session, s.csrf = false, "", ""
	return token, r, true
}

// markSessionLost records that a probe ended the scan's session: the target
// accepted a logout it should have refused. The session is forgotten without
// another login, so the checks after it that need one skip and say why, and
// the closing logout is not sent.
func (s *Scanner) markSessionLost(reason string) {
	s.loggedIn, s.session, s.csrf = false, "", ""
	s.sessionLost = reason
}
