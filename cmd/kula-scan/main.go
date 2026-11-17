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
// Exit status is non-zero when any safeguard FAILs at or above -fail-on
// severity (default high), so it can gate CI / releases.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"kula"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

// run is the command line on the process's standard streams.
func run(args []string) int {
	return runCLI(args, os.Stdout, os.Stderr)
}

// runCLI runs the command line and returns the exit status. The report goes
// to stdout and nothing else does; request logs, progress, banners and errors
// go to stderr.
func runCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("kula-scan", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { printUsage(fs.Output()) }

	var (
		target     = fs.String("target", "", "target base URL, e.g. http://localhost:27960 (or pass as positional arg)")
		username   = fs.String("username", "", "username for authenticated checks (optional)")
		password   = fs.String("password", "", "password for authenticated checks (optional; prefer the "+passwordEnv+" environment variable)")
		basePath   = fs.String("base-path", "", "base path if kula is mounted under one, e.g. /kula")
		timeout    = fs.Duration("timeout", 10*time.Second, "per-request timeout")
		dosWait    = fs.Duration("dos-wait", 35*time.Second, "how long DoS probes wait for the server to reap a slow/idle connection (raise if the target uses long read timeouts)")
		insecure   = fs.Bool("insecure", false, "skip TLS certificate verification")
		aggressive = fs.Bool("aggressive", false, "enable disruptive checks (login lockout, slow/flood DoS probes, XFF bypass)")
		fuzz       = fs.Bool("fuzz", false, "enable blind fault-injection fuzzing (malformed/extreme input across the surface)")
		fuzzIter   = fs.Int("fuzz-iter", 200, "iterations per randomized fuzz probe")
		seed       = fs.Int64("seed", 0, "PRNG seed for fuzzing (0 = random; the chosen seed is reported so runs are reproducible)")
		only       = fs.String("only", "", "comma-separated categories to run (auth,csrf,cors,headers,traversal,metrics,ws,input,rate,dos,redirect,tls,bypass,fuzz)")
		failOn     = fs.String("fail-on", "high", "min FAIL severity for non-zero exit: info|low|medium|high|critical")
		asJSON     = fs.Bool("json", false, "emit findings as JSON")
		noColor    = fs.Bool("no-color", false, "disable ANSI colors")
		verbose    = fs.Bool("v", false, "verbose: log each request and its response status to stderr")
		list       = fs.Bool("list", false, "print every finding ID with its category, mode and title, then exit")
		version    = fs.Bool("version", false, "print the version and exit")
	)

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	if *version {
		_, _ = fmt.Fprintf(stdout, "kula-scan %s\n", kula.Version)
		return 0
	}
	if *list {
		if err := writeCatalog(stdout); err != nil {
			_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
			return 2
		}
		return 0
	}

	tgt := *target
	if tgt == "" && fs.NArg() > 0 {
		tgt = fs.Arg(0)
	}
	if tgt == "" {
		_, _ = fmt.Fprintln(stderr, "error: no target URL given")
		printUsage(stderr)
		return 2
	}

	failSev, ok := parseSeverity(*failOn)
	if !ok {
		_, _ = fmt.Fprintf(stderr, "error: invalid -fail-on %q (want info|low|medium|high|critical)\n", *failOn)
		return 2
	}

	onlySet := parseOnly(*only)

	scanner, err := NewScanner(Options{
		Target:     tgt,
		BasePath:   *basePath,
		Username:   *username,
		Password:   resolvePassword(*password),
		Timeout:    *timeout,
		DoSWait:    *dosWait,
		Insecure:   *insecure,
		Aggressive: *aggressive,
		Fuzz:       *fuzz,
		FuzzIter:   *fuzzIter,
		Seed:       *seed,
		Verbose:    *verbose,
		// Progress lines are for a person watching; a log keeps only the
		// report and what -v asks for.
		Progress: *verbose || isTerminal(stderr),
		Log:      stderr,
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}

	useColor := !*noColor && !*asJSON && colorEnabled(stdout)
	bannerColor := !*noColor && colorEnabled(stderr)

	if *aggressive && !*asJSON {
		_, _ = fmt.Fprintln(stderr, banner(bannerColor,
			"AGGRESSIVE MODE: locks out login from your IP for ~5 min, floods connections, and holds slow requests open "+
				"(up to -dos-wait="+dosWait.String()+" each). Run against staging, or accept the disruption."))
	}
	if *fuzz && !*asJSON {
		_, _ = fmt.Fprintln(stderr, banner(bannerColor,
			fmt.Sprintf("FUZZ MODE: sends malformed/extreme input across the surface (seed %d). May create junk sessions/log noise; reproduce findings with -seed.", scanner.seed)))
	}

	findings := scanner.Run(onlySet)
	report := newReport(scanner.urlFor(""), findings)

	if *asJSON {
		if err := report.writeJSON(stdout); err != nil {
			_, _ = fmt.Fprintf(stderr, "error encoding JSON: %v\n", err)
			return 2
		}
	} else {
		report.writeText(stdout, !useColor)
		if ids := report.failingIDs(failSev); len(ids) > 0 {
			_, _ = fmt.Fprintf(stdout, "\nFAILING safeguards at/above %s: %s\n", failSev, strings.Join(ids, ", "))
		}
	}

	return report.exitCode(failSev)
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

// parseOnly turns a comma list into a set; empty input means "all categories".
func parseOnly(s string) map[string]bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	set := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		if p := strings.ToLower(strings.TrimSpace(part)); p != "" {
			set[p] = true
		}
	}
	return set
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

func banner(color bool, msg string) string {
	if color {
		return cYellow + cBold + "⚠ " + msg + cReset
	}
	return "WARNING: " + msg
}

func printUsage(w interface{ Write([]byte) (int, error) }) {
	_, _ = fmt.Fprint(w, `kula-scan — active safeguard scanner for a running kula instance

Usage:
  kula-scan [flags] <target-url>

Examples:
  kula-scan http://localhost:27960
  read -rs KULA_SCAN_PASSWORD && export KULA_SCAN_PASSWORD
  kula-scan -username admin https://mon.example.com
  kula-scan -only headers,traversal,cors http://10.0.0.5:27960
  kula-scan -aggressive -username admin http://localhost:27960
  kula-scan -fuzz -fuzz-iter 500 -username admin http://localhost:27960
  kula-scan -fuzz -seed 12345 -only fuzz http://localhost:27960
  kula-scan -json http://localhost:27960 > report.json

Flags:
  -target string      target base URL (or pass as positional arg)
  -username string    username for authenticated checks
  -password string    password for authenticated checks; KULA_SCAN_PASSWORD in the
                      environment keeps it out of the process list and shell history
  -base-path string   base path if kula is mounted under one (e.g. /kula)
  -timeout duration   per-request timeout (default 10s)
  -dos-wait duration  how long DoS probes wait for a slow/idle connection to be reaped (default 35s)
  -insecure           skip TLS certificate verification
  -aggressive         enable disruptive checks (login lockout, slow/flood DoS probes, XFF bypass)
  -fuzz               enable blind fault-injection fuzzing (malformed/extreme input)
  -fuzz-iter int      iterations per randomized fuzz probe (default 200)
  -seed int           PRNG seed for fuzzing (0 = random; reported for reproducibility)
  -only string        comma-separated categories: auth,csrf,cors,headers,traversal,metrics,ws,input,rate,dos,redirect,tls,bypass,fuzz
  -fail-on string     min FAIL severity for non-zero exit: info|low|medium|high|critical (default high)
  -json               emit findings as JSON
  -no-color           disable ANSI colors
  -v                  log each request and its response status to stderr
  -list               print every finding ID with its category, mode and title, then exit
  -version            print the version and exit

Exit status: 0 = no failing safeguard at/above -fail-on; 1 = one or more failures; 2 = usage error.
`)
}
