// Command kula-scan is an active, black-box safeguard scanner for a running
// kula instance. It probes a live target over HTTP/WebSocket the way an
// attacker or browser would and reports, per check, whether each security
// safeguard actually holds: authentication gating, CSRF/origin validation,
// CORS, security headers, path-traversal defenses, the Prometheus token,
// WebSocket origin/auth/connection gates, and input/DoS caps.
//
// It imports nothing from kula's internal packages — every assertion is made
// over the wire against a deployed configuration, so it complements (rather
// than duplicates) the in-process runtime tests.
//
// Usage:
//
//	kula-scan [flags] <target-url>
//
// Exit status is 1 when any safeguard FAILs at or above -fail-on severity
// (default high) or contradicts -expect, 2 on a usage error, a target that is
// unreachable or not Kula, or a report that could not be written, 3 when
// nothing fails but a probe could not complete, and 130 when interrupted, so
// it can gate CI / releases.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"kula"
)

func main() {
	// The first SIGINT or SIGTERM interrupts the scan: it logs out and
	// prints the partial report. The handler is then removed, so a second
	// signal kills the process at once.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	done, watched := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(watched)
		select {
		case <-ctx.Done():
			stop()
			_, _ = fmt.Fprintln(os.Stderr, "kula-scan: interrupted; logging out and printing the partial report (interrupt again to quit at once)")
		case <-done:
		}
	}()
	code := runCLI(ctx, os.Args[1:], os.Stdout, os.Stderr)
	close(done)
	<-watched
	stop()
	os.Exit(code)
}

// run is the command line on the process's standard streams, without
// signal handling.
func run(args []string) int {
	return runCLI(context.Background(), args, os.Stdout, os.Stderr)
}

// cliFlags holds the values of the command line's flags.
type cliFlags struct {
	target, username, password, basePath, connect, unix string
	timeout, dosWait                                    time.Duration
	insecure, aggressive, fuzz                          bool
	fuzzIter                                            int
	seed                                                int64
	only, skip, ignore, expect, failOn                  string
	asJSON                                              bool
	format, output                                      string
	noColor, verbose, list, version                     bool
	headers                                             headerFlags
	metrics                                             string
}

// newFlagSet returns the command line's flag set, which prints printUsage
// to stderr, and the values it parses into. printUsage must name every flag
// it registers (TestUsageNamesEveryFlag).
func newFlagSet(stderr io.Writer) (*flag.FlagSet, *cliFlags) {
	fs := flag.NewFlagSet("kula-scan", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { printUsage(fs.Output()) }

	f := &cliFlags{}
	fs.StringVar(&f.target, "target", "", "target base URL, e.g. http://localhost:27960 (or pass as positional arg)")
	fs.StringVar(&f.username, "username", "", "username for authenticated checks (optional)")
	fs.StringVar(&f.password, "password", "", "password for authenticated checks (optional; prefer the "+passwordEnv+" environment variable)")
	fs.StringVar(&f.basePath, "base-path", "", "base path if kula is mounted under one, e.g. /kula")
	fs.StringVar(&f.connect, "connect", "", "connect to this host:port instead of the target URL's address; the URL still gives the Host header, TLS server name and origin")
	fs.StringVar(&f.unix, "unix", "", "connect over this Unix socket (Kula's web.unix_socket); the URL still gives the Host header and base path")
	fs.DurationVar(&f.timeout, "timeout", 10*time.Second, "per-request timeout")
	fs.DurationVar(&f.dosWait, "dos-wait", 35*time.Second, "how long DoS probes wait for the server to reap a slow/idle connection (raise if the target uses long read timeouts)")
	fs.BoolVar(&f.insecure, "insecure", false, "skip TLS certificate verification on the scan's connections (TLS-004 still judges the certificate)")
	fs.BoolVar(&f.aggressive, "aggressive", false, "enable disruptive checks (login lockout, AI rate and body limits, slow/flood DoS probes, a history burst, XFF bypass)")
	fs.BoolVar(&f.fuzz, "fuzz", false, "enable blind fault-injection fuzzing (malformed/extreme input across the surface)")
	fs.IntVar(&f.fuzzIter, "fuzz-iter", 200, "iterations per randomized fuzz probe")
	fs.Int64Var(&f.seed, "seed", 0, "PRNG seed for fuzzing (0 = random; the chosen seed is reported so runs are reproducible)")
	fs.StringVar(&f.only, "only", "", "comma-separated categories and/or finding IDs to run (see -list)")
	fs.StringVar(&f.skip, "skip", "", "comma-separated categories and/or finding IDs not to run or report")
	fs.StringVar(&f.ignore, "ignore", "", "comma-separated finding IDs that stay in the report but never affect the exit status")
	fs.StringVar(&f.expect, "expect", "", "comma-separated posture the target must have, failing the run otherwise: "+strings.Join(expectKeyNames(), ","))
	fs.StringVar(&f.failOn, "fail-on", "high", "min FAIL severity for non-zero exit: info|low|medium|high|critical")
	fs.BoolVar(&f.asJSON, "json", false, "emit findings as JSON (-format json)")
	fs.StringVar(&f.format, "format", "", "report format: text|json|junit|sarif (default text)")
	fs.StringVar(&f.output, "o", "", "write the report in -format to `FILE`; stdout then gets the text report")
	fs.BoolVar(&f.noColor, "no-color", false, "disable ANSI colors")
	fs.BoolVar(&f.verbose, "v", false, "verbose: log each request and its response status to stderr")
	fs.BoolVar(&f.list, "list", false, "print every finding ID with its category, mode and title, then exit")
	fs.BoolVar(&f.version, "version", false, "print the version and exit")
	fs.StringVar(&f.metrics, "metrics-token", "", "Prometheus bearer token /metrics should accept (optional; prefer the "+metricsTokenEnv+" environment variable)")
	fs.Var(&f.headers, "H", "'Name: value' header sent on every request, e.g. a gateway's access token (repeatable; values are never printed)")
	return fs, f
}

// runCLI runs the command line and returns the exit status. The report goes
// to stdout and nothing else does; request logs, progress, banners and errors
// go to stderr. Cancelling ctx interrupts the scan (exit status 130).
func runCLI(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs, f := newFlagSet(stderr)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	if f.version {
		_, _ = fmt.Fprintf(stdout, "kula-scan %s\n", kula.Version)
		return 0
	}
	if f.list {
		if err := writeCatalog(stdout); err != nil {
			_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
			return 2
		}
		return 0
	}

	tgt := f.target
	if tgt == "" && fs.NArg() > 0 {
		tgt = fs.Arg(0)
	}
	if tgt == "" {
		_, _ = fmt.Fprintln(stderr, "error: no target URL given")
		printUsage(stderr)
		return 2
	}

	failSev, ok := parseSeverity(f.failOn)
	if !ok {
		_, _ = fmt.Fprintf(stderr, "error: invalid -fail-on %q (want info|low|medium|high|critical)\n", f.failOn)
		return 2
	}

	sel, err := newSelection(f.only, f.skip)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}
	ignored, ignoredIDs, err := parseIgnore(f.ignore)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}
	expected, err := parseExpect(f.expect)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}
	reportFormat, err := resolveFormat(f.format, f.asJSON)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}

	scanner, err := NewScanner(Options{
		Target:     tgt,
		BasePath:   f.basePath,
		Username:   f.username,
		Password:   resolvePassword(f.password),
		Timeout:    f.timeout,
		DoSWait:    f.dosWait,
		Insecure:   f.insecure,
		Aggressive: f.aggressive,
		Fuzz:       f.fuzz,
		FuzzIter:   f.fuzzIter,
		Seed:       f.seed,
		Verbose:    f.verbose,
		// Progress lines are for a person watching; a log keeps only the
		// report and what -v asks for.
		Progress: f.verbose || isTerminal(stderr),
		Log:      stderr,
		Expect:   expected,
		Connect:  f.connect,
		Unix:     f.unix,
		Headers:  f.headers,
		// The token, like the password, can come from the environment.
		MetricsToken: resolveMetricsToken(f.metrics),
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}

	// With -o the chosen format goes to the file and stdout gets the text
	// report, so a CI job keeps a readable log and an artifact from one run.
	stdoutFormat := reportFormat
	var outFile *os.File
	if f.output != "" {
		if outFile, err = createReportFile(f.output); err != nil {
			_, _ = fmt.Fprintf(stderr, "error: -o: %v\n", err)
			return 2
		}
		stdoutFormat = formatText
	}
	textOut := stdoutFormat == formatText

	useColor := !f.noColor && textOut && colorEnabled(stdout)
	bannerColor := !f.noColor && colorEnabled(stderr)

	if f.aggressive && textOut {
		_, _ = fmt.Fprintln(stderr, banner(bannerColor,
			"AGGRESSIVE MODE: locks out login from your IP for ~5 min, floods connections, history and AI chat requests, and holds slow requests open "+
				"(up to -dos-wait="+f.dosWait.String()+" each). Run against staging, or accept the disruption."))
	}
	if f.fuzz && textOut {
		_, _ = fmt.Fprintln(stderr, banner(bannerColor, fuzzBanner(scanner.seed)))
	}

	started := time.Now()
	findings := scanner.Run(ctx, sel)
	markIgnored(findings, ignored)
	report := newReport(scanner.urlFor(""), findings)
	report.Scanner = ScannerInfo{Name: "kula-scan", Version: kula.Version}
	report.Started = started.UTC().Truncate(time.Second)
	report.DurationMS = time.Since(started).Milliseconds()
	report.Options = ReportOptions{
		Aggressive: f.aggressive,
		Fuzz:       f.fuzz,
		Only:       nonNil(sel.onlyList),
		Skip:       nonNil(sel.skipList),
		Ignore:     nonNil(ignoredIDs),
		Expect:     nonNil(expected),
		FailOn:     strings.ToLower(failSev.String()),
		Connect:    f.connect,
		Unix:       f.unix,
		Headers:    scanner.extraHeaderNames(),
	}
	if f.fuzz {
		report.Options.Seed = scanner.seed
	}
	report.TargetInfo = scanner.targetInfo()
	report.Interrupted, report.NotRun = scanner.interrupted, scanner.notRun
	report.ExitCode = report.exitCode(failSev)

	// The file first: it is the artifact a CI job keeps.
	writeFailed := false
	if outFile != nil {
		if err := writeReportFile(outFile, report, reportFormat, failSev); err != nil {
			_, _ = fmt.Fprintf(stderr, "error: -o: %v\n", err)
			writeFailed = true
		}
	}
	if err := report.writeFormat(stdout, stdoutFormat, failSev, !useColor); err != nil {
		_, _ = fmt.Fprintf(stderr, "error writing the %s report: %v\n", stdoutFormat, err)
		writeFailed = true
	}
	if writeFailed {
		return 2
	}

	if reason := report.exitReason(failSev); reason != "" {
		_, _ = fmt.Fprintln(stderr, reason)
	}
	return report.ExitCode
}

// nonNil returns list, or an empty list for nil, so JSON shows [] not null.
func nonNil(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}

// passwordEnv names the environment variable that can carry the password
// instead of -password, whose value any local user can read in the process
// list and which the shell keeps in its history.
const passwordEnv = "KULA_SCAN_PASSWORD" //nolint:gosec // G101: the variable's name, not a credential

// resolvePassword returns the -password value, or the passwordEnv variable
// when the flag is empty.
func resolvePassword(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	return os.Getenv(passwordEnv)
}

// metricsTokenEnv names the environment variable that can carry the
// Prometheus token instead of -metrics-token, for the reasons passwordEnv
// carries the password.
const metricsTokenEnv = "KULA_SCAN_METRICS_TOKEN" //nolint:gosec // G101: the variable's name, not a credential

// resolveMetricsToken returns the -metrics-token value, or the
// metricsTokenEnv variable when the flag is empty.
func resolveMetricsToken(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	return os.Getenv(metricsTokenEnv)
}

// headerFlags collects the repeatable -H flag. String shows no value: the
// flag package prints it, and header values are secrets.
type headerFlags []string

func (h *headerFlags) String() string { return "" }

func (h *headerFlags) Set(value string) error {
	*h = append(*h, value)
	return nil
}

// colorEnabled reports whether ANSI colors should be used on w: honors
// NO_COLOR and only colorizes a terminal.
func colorEnabled(w io.Writer) bool {
	return os.Getenv("NO_COLOR") == "" && isTerminal(w)
}

// isTerminal reports whether w is a character device (a terminal).
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// fuzzBanner is the -fuzz warning: what fuzzing costs the target. The login
// attempts are FUZZ-BODY's, and with origin validation off one of
// FUZZ-SMUGGLE's requests (see allChecks).
func fuzzBanner(seed int64) string {
	return fmt.Sprintf("FUZZ MODE: sends malformed/extreme input across the surface (seed %d). "+
		"With auth on it spends up to %d of this address's %d login attempts in five minutes (%d with origin validation off), "+
		"and leaves the refused requests in the target's log; reproduce findings with -seed.",
		seed, fuzzLoginBodies, kulaLoginAddressLimit, fuzzLoginBodies+1)
}

func banner(color bool, msg string) string {
	if color {
		return cYellow + cBold + "⚠ " + msg + cReset
	}
	return "WARNING: " + msg
}

func printUsage(w io.Writer) {
	_, _ = fmt.Fprintf(w, `kula-scan — active safeguard scanner for a running kula instance

Usage:
  kula-scan [flags] <target-url>

Examples:
  kula-scan http://localhost:27960
  read -rs KULA_SCAN_PASSWORD && export KULA_SCAN_PASSWORD
  kula-scan -username admin https://mon.example.com
  kula-scan -only headers,traversal,cors http://10.0.0.5:27960
  kula-scan -connect 10.0.0.5:443 https://mon.example.com
  kula-scan -unix /run/kula/kula.sock http://localhost/
  kula-scan -skip HDR-009 -ignore PROM-001 http://10.0.0.5:27960
  kula-scan -expect auth,tls,hsts -username admin https://mon.example.com
  kula-scan -aggressive -username admin http://localhost:27960
  kula-scan -fuzz -fuzz-iter 500 -username admin http://localhost:27960
  kula-scan -fuzz -seed 12345 -only fuzz http://localhost:27960
  kula-scan -json http://localhost:27960 > report.json
  kula-scan -format sarif -o kula-scan.sarif http://localhost:27960

Flags:
  -target string      target base URL (or pass as positional arg)
  -username string    username for authenticated checks
  -password string    password for authenticated checks; KULA_SCAN_PASSWORD in the
                      environment keeps it out of the process list and shell history
  -metrics-token string
                      Prometheus bearer token /metrics should accept (PROM-003);
                      KULA_SCAN_METRICS_TOKEN in the environment keeps it out of the process list
  -base-path string   base path if kula is mounted under one (e.g. /kula)
  -connect host:port  connect here instead of the target URL's address; the URL still gives
                      the Host header, TLS server name and origin (scan by IP under the real
                      name, or a backend behind its proxy)
  -unix path          connect over this Unix socket (Kula's web.unix_socket); the URL still
                      gives the Host header and base path
  -H 'Name: value'    header sent on every request, for a gateway in front of Kula (an access
                      proxy's token, basic auth at the proxy); repeatable. A probe's own header
                      of the same name wins, except Cookie, which carries both. Values are
                      never printed; the JSON report lists the names
  -timeout duration   per-request timeout (default 10s)
  -dos-wait duration  how long DoS probes wait for a slow/idle connection to be reaped (default 35s)
  -insecure           skip TLS certificate verification on the scan's connections, for a
                      self-signed instance; TLS-004 still judges the certificate
  -aggressive         enable disruptive checks (login lockout, AI rate and body limits,
                      slow/flood DoS probes, a history burst, XFF bypass)
  -fuzz               enable blind fault-injection fuzzing (malformed/extreme input)
  -fuzz-iter int      iterations per randomized fuzz probe (default 200)
  -seed int           PRNG seed for fuzzing (0 = random; reported for reproducibility)
  -only list          categories and/or finding IDs to run, comma-separated (case-insensitive);
                      IDs such as CSRF-003 are listed by -list; the categories are
                      %s
  -skip list          categories and/or finding IDs not to run or report
  -ignore list        finding IDs that stay in the report, marked ignored, but never affect
                      the exit status
  -expect list        posture the target must have; a finding that contradicts it FAILs and
                      exits 1 whatever -fail-on says; the keys are
                      %s
  -fail-on string     min FAIL severity for non-zero exit: info|low|medium|high|critical (default high)
  -format string      report format: text|json|junit|sarif (default text)
  -o FILE             write the -format report to FILE (created with mode 0600); stdout
                      then gets the text report
  -json               emit findings as JSON (the same as -format json)
  -no-color           disable ANSI colors
  -v                  log each request and its response status to stderr
  -list               print every finding ID with its category, mode and title, then exit
  -version            print the version and exit

Exit status:
  0    no failing safeguard at/above -fail-on, and every probe completed
  1    one or more safeguards FAIL at/above -fail-on, or contradict -expect
       (ignored findings excepted)
  2    usage error, the target is unreachable or not a Kula instance, or the
       report could not be written
  3    inconclusive: nothing fails, but a probe could not complete (ERROR),
       e.g. the supplied credentials were refused
  130  interrupted (SIGINT/SIGTERM): the scan logs out and prints a partial report;
       a second signal quits at once
`, strings.Join(categories(), ","), strings.Join(expectKeyNames(), ","))
}
