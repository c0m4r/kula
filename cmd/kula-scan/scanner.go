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
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

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

	username string
	password string

	// Discovered during the discovery phase.
	reachable   bool // the target answered HTTP
	kula        bool // and identified as Kula
	https       bool
	authEnabled bool
	ollama      bool
	version     string // Kula's version, when /api/config discloses it

	// Populated by ensureSession() when credentials are supplied and valid.
	session     string       // kula_session cookie value
	csrf        string       // csrf_token returned by login
	loginCookie *http.Cookie // the Set-Cookie issued at login, for attribute inspection
	loggedIn    bool
	loginTried  bool
	login       loginOutcome // how the login attempt ended
	loginDetail string       // why it failed, for AUTH-005
}

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
	bp := strings.TrimRight(opt.BasePath, "/")
	if bp == "" && u.Path != "" && u.Path != "/" {
		bp = strings.TrimRight(u.Path, "/")
	}
	u.Path = bp
	u.RawQuery = ""
	u.Fragment = ""

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

	transport := &http.Transport{
		// kula-scan probes arbitrary targets; the Go client default min (TLS 1.2) is correct, and InsecureSkipVerify is the opt-in below.
		// nosemgrep: missing-ssl-minversion
		TLSClientConfig:        &tls.Config{InsecureSkipVerify: opt.Insecure}, //nolint:gosec // -insecure is an explicit opt-in for self-signed test instances
		ForceAttemptHTTP2:      true,
		MaxResponseHeaderBytes: maxResponseHeaderBytes,
	}
	client := &http.Client{
		Timeout:   opt.Timeout,
		Transport: transport,
		// Observe redirects (301/302) directly rather than following them, so
		// base-path redirects and auth bounces are visible to the checks.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	expect := map[string]bool{}
	for _, key := range opt.Expect {
		expect[key] = true
	}

	return &Scanner{
		base:       u,
		rawBase:    opt.Target,
		client:     client,
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
		username:   opt.Username,
		password:   opt.Password,
		https:      u.Scheme == "https",
	}, nil
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
}

// do issues an HTTP request to path with optional headers and body, returning a
// compact result. Cookies are never persisted between calls (the client has no
// jar); callers set Cookie/Authorization headers explicitly.
func (s *Scanner) do(method, path string, headers map[string]string, body string) httpResult {
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()

	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.urlFor(path), rdr)
	if err != nil {
		return httpResult{err: err}
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		if s.verbose {
			_, _ = fmt.Fprintf(s.log, "    %s %s -> %s\n", printable(method), printable(path), printable(err.Error()))
		}
		return httpResult{err: err}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if s.verbose {
		_, _ = fmt.Fprintf(s.log, "    %s %s -> %d (%d bytes)\n", printable(method), printable(path), resp.StatusCode, len(raw))
	}
	return httpResult{status: resp.StatusCode, header: resp.Header, body: string(raw)}
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

// dialRaw opens a raw connection to the target (TLS when https), with default
// ports filled in. The caller owns the returned conn and must close it. DoS
// probes use this to drive the socket directly (partial requests, oversized
// headers); rawTCP layers a single request/response on top.
func (s *Scanner) dialRaw() (net.Conn, error) {
	host := s.base.Host
	if !strings.Contains(host, ":") {
		if s.https {
			host += ":443"
		} else {
			host += ":80"
		}
	}
	dialer := &net.Dialer{Timeout: s.timeout}
	if s.https {
		// Probe tool: connect to whatever the target offers; Go's client default min is TLS 1.2. InsecureSkipVerify is an explicit opt-in.
		// nosemgrep: missing-ssl-minversion
		return tls.DialWithDialer(dialer, "tcp", host, &tls.Config{InsecureSkipVerify: s.insecure}) //nolint:gosec // explicit opt-in
	}
	return dialer.Dial("tcp", host)
}

// rawTCP opens a raw connection (TLS when the target is https) and writes a
// literal HTTP/1.1 request line, so the path is sent verbatim without any
// client-side URL normalisation. This is how traversal payloads are delivered.
// It returns the status code and body; a status of 0 means the server rejected
// the request at the protocol layer (an acceptable, safe outcome).
func (s *Scanner) rawTCP(requestLine string) (int, string) {
	conn, err := s.dialRaw()
	if err != nil {
		return 0, ""
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(s.timeout))

	if _, err := fmt.Fprintf(conn, "%s\r\nHost: %s\r\nConnection: close\r\n\r\n", requestLine, s.base.Host); err != nil {
		return 0, ""
	}
	resp, err := readResponse(conn)
	if err != nil {
		return 0, ""
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodyBytes))
	return resp.StatusCode, string(body)
}

// rawExchange writes a fully caller-supplied raw HTTP request (request line,
// headers, and body verbatim) and returns the response status and body. It is
// used by the request-smuggling probes that need conflicting/duplicate framing
// headers the net/http client refuses to send. A status of 0 means the server
// closed or rejected the connection at the protocol layer.
func (s *Scanner) rawExchange(raw string) (int, string) {
	conn, err := s.dialRaw()
	if err != nil {
		return 0, ""
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(s.timeout))

	if _, err := conn.Write([]byte(raw)); err != nil {
		return 0, ""
	}
	resp, err := readResponse(conn)
	if err != nil {
		return 0, ""
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodyBytes))
	return resp.StatusCode, string(body)
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
// returning the HTTP response (for status inspection) and the connection (nil
// on failure). The caller is responsible for closing a non-nil connection.
func (s *Scanner) wsDial(path string, headers http.Header) (*websocket.Conn, *http.Response, error) {
	scheme := "ws"
	if s.https {
		scheme = "wss"
	}
	wsURL := scheme + "://" + s.base.Host + s.base.Path + path

	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = s.timeout
	// Probe tool: connect to whatever the target offers; Go's client default min is TLS 1.2. InsecureSkipVerify is an explicit opt-in.
	// nosemgrep: missing-ssl-minversion
	dialer.TLSClientConfig = &tls.Config{InsecureSkipVerify: s.insecure} //nolint:gosec // explicit opt-in
	// Dial calls this in the calling goroutine and is done with the budget
	// when it returns.
	var budgeted []*budgetConn
	dialer.NetDialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		budget := &budgetConn{Conn: conn, remaining: maxResponseHeaderBytes}
		budgeted = append(budgeted, budget)
		return budget, nil
	}
	conn, resp, err := dialer.Dial(wsURL, headers)
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
// ERROR and leaves s.reachable or s.kula false: the scan stops there.
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

	// /api/config (open when auth is off; gated when on) reveals ollama state.
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

	// A host check refusal leaves authEnabled false, which it is: Kula
	// guards the API with that check only while auth is off.
	if hasAuthRequired {
		_ = json.Unmarshal(authStatus["auth_required"], &s.authEnabled)
	}
	if cfg.status == http.StatusOK {
		var c struct {
			OllamaEnabled bool   `json:"ollama_enabled"`
			Version       string `json:"version"`
		}
		_ = json.Unmarshal([]byte(cfg.body), &c)
		s.ollama = c.OllamaEnabled
		s.version = c.Version
	}

	authState := "disabled"
	if s.authEnabled {
		authState = "enabled"
	}
	detail := fmt.Sprintf("reachable at %s (%s); authentication %s; ollama %v",
		s.urlFor(""), s.base.Scheme, authState, s.ollama)
	if s.version != "" {
		detail += "; version " + s.version
	}
	if hostRefused {
		detail += "; the API refuses this Host name (Kula's DNS-rebinding check), so API checks see that refusal: scan by address or by a name in web.security.allowed_hosts"
	}
	out = append(out, finding("DISC-001", SevInfo, StatusPass, detail))

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

	r := s.loginRaw(s.username, s.password)
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

// allChecks returns the full registry in execution order. Categories run
// grouped; aggressive checks are tagged and filtered by the runner.
func allChecks() []check {
	var checks []check
	checks = append(checks, headerChecks()...)
	checks = append(checks, authChecks()...)
	checks = append(checks, csrfChecks()...)
	checks = append(checks, corsChecks()...)
	checks = append(checks, traversalChecks()...)
	checks = append(checks, metricsChecks()...)
	checks = append(checks, wsChecks()...)
	checks = append(checks, inputChecks()...)
	checks = append(checks, redirectChecks()...)
	checks = append(checks, tlsChecks()...)
	checks = append(checks, aggressiveChecks()...)
	checks = append(checks, dosChecks()...)
	checks = append(checks, bypassChecks()...)
	checks = append(checks, fuzzChecks()...) // runs last; ends with the liveness probe
	return checks
}

// Run executes discovery and every check sel selects, returning the selected
// findings. Discovery always runs, and DISC-001, which says whether the target
// was scanned at all, is always reported. Findings are judged against the
// -expect posture as each check returns them.
func (s *Scanner) Run(sel *selection) []Finding {
	var findings []Finding
	discovered := s.discover()
	s.applyExpectations(discovered)
	for _, f := range discovered {
		if f.ID == "DISC-001" || sel.selected(f.ID) {
			findings = append(findings, f)
		}
	}

	// A target that does not answer, or is not Kula, is not probed further.
	if !s.reachable || !s.kula {
		return findings
	}

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

	started := 0
	for _, c := range checks {
		switch mode := c.entry().Mode; {
		case mode == ModeFuzz && !s.fuzz:
			findings = append(findings, skippedFindings(c,
				"blind fault-injection check not run; pass -fuzz to enable (it sends malformed/extreme input to the live target).")...)
			continue
		case mode == ModeAggressive && !s.aggressive:
			findings = append(findings, skippedFindings(c,
				"disruptive check not run; pass -aggressive to enable (it has side effects on the live target).")...)
			continue
		}
		started++
		s.logProgress(started, runnable, c)
		results := c.run(s)
		s.applyExpectations(results)
		for _, f := range results {
			if sel.selected(f.ID) {
				findings = append(findings, f)
			}
		}
	}
	s.logout()
	return findings
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

// logout ends the session ensureSession opened, so a run leaves no session
// behind on the target. It is best effort: a check that probed the logout
// endpoint may have ended the session already.
func (s *Scanner) logout() {
	if !s.loggedIn {
		return
	}
	headers := s.authedHeaders(true)
	headers["Origin"] = s.base.Scheme + "://" + s.base.Host
	s.do(http.MethodPost, "/api/logout", headers, "")
	s.loggedIn = false
	s.session = ""
	s.csrf = ""
}
