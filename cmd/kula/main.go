package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"kula"
	"kula/internal/backup"
	"kula/internal/collector"
	"kula/internal/config"
	"kula/internal/sandbox"
	"kula/internal/storage"
	"kula/internal/tui"
	"kula/internal/web"

	"github.com/charmbracelet/x/term"
)

var version = kula.Version

// defaultConfigPath is the config read when -config is not given.
const defaultConfigPath = "config.yaml"

// commands lists kula's commands, in the order the help shows them, with
// their line in `kula --help` and the text of `kula <command> --help`.
var commands = []struct {
	name, summary, help string
}{
	{"serve", "Start the monitoring daemon with web UI (default)",
		"Starts the monitoring daemon: collection, storage and the web UI. It is the\ncommand kula runs when none is given."},
	{"tui", "Launch the terminal UI dashboard",
		"Launches the terminal UI dashboard."},
	{"hash-password", "Generate an Argon2 password hash for config",
		"Reads a password, typed or as one line of standard input, and prints its\nArgon2id hash and salt for web.auth, with the config's argon2 settings."},
	{"inspect", "Display storage tier resolution, coverage, and file information",
		"Displays storage tier resolution, coverage, and file information. Exits 1\nwhen a tier file exists but cannot be inspected."},
	{"disks", "List available disks and partitions with persistent IDs",
		"Lists available disks and partitions with their persistent IDs. It reads no\nconfig and creates none: -config is accepted and ignored."},
}

const globalFlagsHelp = `  -config string  Path to configuration file (default "config.yaml")
  -h, --help      Show this help message
`

const verboseFlagHelp = `  --verbose       Include the latest recorded metrics from each tier
`

func printUsage(w io.Writer) {
	fmt.Fprintf(w, "Kula v%s — Lightweight Linux Server Monitor\n\n", version)
	fmt.Fprint(w, "Usage:\n  kula [flags] [command] [flags]\n\nCommands:\n")
	for _, c := range commands {
		fmt.Fprintf(w, "  %-13s  %s\n", c.name, c.summary)
	}
	fmt.Fprint(w, "\nFlags, before or after the command:\n"+globalFlagsHelp)
	fmt.Fprint(w, "\nFlags before the command:\n  -v, -version    Print the version and exit\n")
	fmt.Fprint(w, "\nInspect flags:\n"+verboseFlagHelp)
	fmt.Fprint(w, "\nRun 'kula <command> --help' for the help of one command.\n\n")
}

// commandFlags are the flags that may follow a command: the global -config,
// -h and --help, and the command's own.
type commandFlags struct {
	set     *flag.FlagSet
	config  string
	verbose bool // inspect
}

// newCommandFlags returns the flags of a command in commands. -h and --help
// print its usage to stderr.
func newCommandFlags(command string, stderr io.Writer) *commandFlags {
	f := &commandFlags{set: flag.NewFlagSet("kula "+command, flag.ContinueOnError)}
	f.set.SetOutput(stderr)
	f.set.StringVar(&f.config, "config", defaultConfigPath, "path to configuration file")
	if command == "inspect" {
		f.set.BoolVar(&f.verbose, "verbose", false, "include the latest recorded metrics from each tier")
	}
	f.set.Usage = func() {
		for _, c := range commands {
			if c.name != command {
				continue
			}
			fmt.Fprintf(stderr, "Usage: kula [global flags] %s [flags]\n\n%s\n\nFlags:\n%s", c.name, c.help, globalFlagsHelp)
			if command == "inspect" {
				fmt.Fprint(stderr, verboseFlagHelp)
			}
		}
	}
	return f
}

// invocation is a parsed command line: kula [flags] [command] [flags].
type invocation struct {
	command        string
	configPath     string
	configExplicit bool // -config was given, before or after the command
	showVersion    bool
	args           []string // what follows the command
}

// errUnknownCommand is returned for a command kula does not have.
var errUnknownCommand = errors.New("unknown command")

// errUsage is returned for a command line that was refused after its reason
// and the usage were printed.
var errUsage = errors.New("usage error")

// parseCommandLine parses kula's arguments, the program name left out. The
// global flags -config, -h and --help may come before the command or after
// it; -v and -version only before it, since -v after inspect would read as
// its -verbose. A -config after the command is as explicit as one before it,
// and wins over it. It returns flag.ErrHelp once help was asked for and
// printed, errUsage once a refused command line was explained, and
// errUnknownCommand, with the command in inv.command.
func parseCommandLine(args []string, stderr io.Writer) (invocation, error) {
	inv := invocation{command: "serve"}
	global := flag.NewFlagSet("kula", flag.ContinueOnError)
	global.SetOutput(stderr)
	global.Usage = func() { printUsage(stderr) }
	global.StringVar(&inv.configPath, "config", defaultConfigPath, "path to configuration file")
	global.BoolVar(&inv.showVersion, "version", false, "print version and exit")
	global.BoolVar(&inv.showVersion, "v", false, "print version and exit")
	if err := global.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return inv, err
		}
		return inv, errUsage
	}
	global.Visit(func(f *flag.Flag) {
		if f.Name == "config" {
			inv.configExplicit = true
		}
	})
	if inv.showVersion {
		return inv, nil
	}
	if global.NArg() > 0 {
		inv.command = global.Arg(0)
		inv.args = global.Args()[1:]
	}
	known := false
	for _, c := range commands {
		known = known || c.name == inv.command
	}
	if !known {
		return inv, errUnknownCommand
	}

	cmdFlags := newCommandFlags(inv.command, stderr)
	if err := cmdFlags.set.Parse(inv.args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return inv, err
		}
		return inv, errUsage
	}
	if cmdFlags.set.NArg() > 0 {
		fmt.Fprintf(stderr, "kula %s: unexpected argument %q\n", inv.command, cmdFlags.set.Arg(0))
		cmdFlags.set.Usage()
		return inv, errUsage
	}
	cmdFlags.set.Visit(func(f *flag.Flag) {
		if f.Name == "config" {
			inv.configPath, inv.configExplicit = cmdFlags.config, true
		}
	})
	return inv, nil
}

func main() {
	inv, err := parseCommandLine(os.Args[1:], os.Stderr)
	switch {
	case errors.Is(err, flag.ErrHelp):
		return
	case errors.Is(err, errUnknownCommand):
		fmt.Fprintf(os.Stderr, "Unknown command: %s\nUsage: kula [flags] [serve|tui|hash-password|inspect|disks] [flags]\n", inv.command)
		os.Exit(1)
	case err != nil:
		os.Exit(2)
	}

	if inv.showVersion {
		fmt.Printf("Kula v%s — Lightweight Linux Server Monitor\n", version)
		return
	}
	cmd, configPath := inv.command, inv.configPath

	// Disk discovery must work before a config exists and must not seed a
	// config file, create storage directories, or initialize applications.
	if cmd == "disks" {
		if err := runDisks(os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to list disks: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// An explicit -config, before or after the command, must be read: one
	// that is missing or unreadable aborts startup rather than silently
	// using defaults.
	loadConfig := config.Load
	if inv.configExplicit {
		if err := seedExplicitConfig(configPath); err != nil {
			log.Fatalf("Failed to load config: %v", err)
		}
		loadConfig = config.LoadRequired
	}

	osName := getOSName()
	kernelVersion := getKernelVersion()
	cpuArch := runtime.GOARCH

	if cmd == "serve" || cmd == "tui" {
		log.Printf("Kula v%s starting...", version)
	}

	if cmd == "hash-password" {
		password := readPasswordWithAsterisks()
		if password == "" {
			// A config holding the hash of nothing would accept an empty
			// password, as after a scripted run with an unset variable.
			fmt.Fprintln(os.Stderr, "kula hash-password: the password is empty")
			os.Exit(1)
		}

		// Load config
		cfg, err := loadConfig(configPath)
		if err != nil {
			log.Fatalf("Failed to load config: %v", err)
		}

		web.PrintHashedPassword(password, cfg.Web.Auth.Argon2)
		return
	}

	// Load config for other commands
	cfg, err := loadConfig(configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	if !cfg.Global.ShowSystemInfo {
		osName = "Hidden"
		kernelVersion = "Hidden"
		cpuArch = "Hidden"
	}

	switch cmd {
	case "serve":
		runServe(cfg, configPath, osName, kernelVersion, cpuArch)
	case "tui":
		runTUI(cfg, osName, kernelVersion, cpuArch)
	case "inspect":
		if err := runInspectTier(cfg, inv.args, os.Stdout, os.Stderr); err != nil {
			fmt.Fprintf(os.Stderr, "kula inspect: %v\n", err)
			if errors.Is(err, errTierNotInspected) {
				os.Exit(1)
			}
			os.Exit(2)
		}
	}
}

// seedExplicitConfig prepares a config path given with -config, which Kula
// then must read: one that does not exist is created from the packaged
// example, as on a first run. When that fails, it is an error — never a
// start on built-in defaults, which would serve with auth off. The file is
// created exclusively, so a link left at the path is not followed.
func seedExplicitConfig(path string) error {
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		return nil // exists, or cannot be looked at: loading reports it
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("config %q does not exist and cannot be created from the packaged example: %w", path, err)
	}
	if _, err = f.Write(kula.ExampleConfig); err == nil {
		err = f.Close()
	} else {
		_ = f.Close()
	}
	if err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("config %q does not exist and cannot be created from the packaged example: %w", path, err)
	}
	log.Printf("Warning: config %q did not exist; seeded it from the packaged example", path)
	return nil
}

// collectionStopTimeout bounds how long a shutdown waits for the collection
// loop to finish the sample it is on.
const collectionStopTimeout = 5 * time.Second

// The sandbox and the store `kula serve` starts with; variables so a test
// can check that the store opens under the sandbox.
var (
	enforceSandbox = sandbox.Enforce
	openStore      = storage.NewStore
)

func runServe(cfg *config.Config, configPath string, osName, kernelVersion, cpuArch string) {
	cfg.Web.Version = version
	cfg.Web.OS = osName
	cfg.Web.Kernel = kernelVersion
	cfg.Web.Arch = cpuArch
	cfg.Collection.DebugLog = cfg.Web.Logging.Enabled && cfg.Web.Logging.Level == "debug"

	// Enforce Landlock sandbox: restrict filesystem and network access
	// to only what Kula needs, before opening storage files or anything
	// else. It is a hardening layer, not a requirement: a kernel without
	// Landlock, or one that refuses the ruleset, leaves Kula unconfined
	// with a warning.
	if err := enforceSandbox(configPath, cfg.Storage.Directory, cfg.Web, cfg.Applications, cfg.Ollama); err != nil {
		log.Printf("Warning: Landlock sandbox not enforced, Kula runs unconfined: %v", err)
	}

	coll := collector.New(cfg.Global, cfg.Collection, cfg.Applications, cfg.Storage.Directory)

	store, err := openStore(cfg.Storage)
	if err != nil {
		log.Fatalf("Failed to initialize storage: %v", err)
	}
	defer func() { _ = store.Close() }()

	// Build the backup scheduler up front so an invalid cron expression fails
	// fast, before the server starts. Started below once the signal context exists.
	var backupScheduler *backup.Scheduler
	if cfg.Backup.Enabled {
		backupScheduler, err = backup.New(store, cfg.Storage.Directory, cfg.Backup)
		if err != nil {
			log.Fatalf("Failed to initialize backup scheduler: %v", err)
		}
	}

	var server *web.Server
	if cfg.Web.Enabled {

		server = web.NewServer(cfg.Web, cfg.Global, coll, store, cfg.Storage.Directory, cfg.Ollama)

		// Start web server
		go func() {
			if err := server.Start(); err != nil {
				log.Fatalf("Web server error: %v", err)
			}
		}()
	} else {
		log.Printf("Web server disabled by configuration")
	}

	// Signal handling with Context
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Backup scheduler
	if backupScheduler != nil {
		go backupScheduler.Run(ctx)
		log.Printf("Backup enabled (schedule %q, maxtier %d, retention %s, compress %v)",
			cfg.Backup.Cron, cfg.Backup.MaxTier, cfg.Backup.Retention, cfg.Backup.Compress)
	}

	log.Printf("Kula v%s started (collecting every %s)", version, cfg.Collection.Interval)
	log.Printf("OS: %s, Kernel: %s, Arch: %s", osName, kernelVersion, cpuArch)

	// Initialize optional application collectors after the startup banner so their
	// (potentially noisy) discovery output appears below it rather than above.
	coll.StartApplications()

	// Collection loop
	collectionDone := make(chan struct{})
	go func() {
		defer close(collectionDone)
		ticker := time.NewTicker(cfg.Collection.Interval)
		defer ticker.Stop()

		// Initial collection
		sample := coll.Collect()
		if err := store.WriteSample(sample); err != nil {
			log.Printf("Storage write error: %v", err)
		}
		if server != nil {
			server.BroadcastSample(sample)
		}

		for {
			select {
			case <-ticker.C:
				sample := coll.Collect()
				if err := store.WriteSample(sample); err != nil {
					log.Printf("Storage write error: %v", err)
				}
				if server != nil {
					server.BroadcastSample(sample)
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	<-ctx.Done()

	log.Println("Shutting down...")
	// The loop ends with the context, but may be in the middle of a sample.
	// Wait for it, so the deferred store.Close() does not close the tiers
	// under its write. Not forever: a collection stuck on the host (a hung
	// mount) must not keep the process from stopping.
	select {
	case <-collectionDone:
	case <-time.After(collectionStopTimeout):
		log.Printf("Collection still running after %s, closing storage without it", collectionStopTimeout)
	}
	coll.Stop()

	if server != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("Web server shutdown error: %v", err)
		}
	}
}

func runTUI(cfg *config.Config, osName, kernelVersion, cpuArch string) {
	// The TUI shows no custom metrics, and their listener lives at
	// <storage>/kula.sock: with the service's config it would contend for the
	// socket a running `kula serve` owns.
	apps := cfg.Applications
	apps.Custom = nil

	// The TUI, often run as root, runs the same collectors as `kula serve`:
	// confine it as well, before it collects or opens storage, to their
	// reads and connections and the storage directory read-only.
	if err := sandbox.EnforceTUI(cfg.Storage.Directory, apps); err != nil {
		log.Printf("Warning: Landlock sandbox not enforced, the TUI runs unconfined: %v", err)
	}

	coll := collector.New(cfg.Global, cfg.Collection, apps, cfg.Storage.Directory)
	defer coll.Stop()
	coll.StartApplications()

	opts := tui.Options{
		RefreshRate:    cfg.TUI.RefreshRate,
		OSName:         osName,
		KernelVersion:  kernelVersion,
		CPUArch:        cpuArch,
		Version:        version,
		ShowSystemInfo: cfg.Global.ShowSystemInfo,
		HistoryDir:     cfg.Storage.Directory,
		// The History view reads the tier files `kula serve` writes. Read-only:
		// the TUI never writes, migrates or resizes storage it does not own.
		OpenHistory: func() (tui.HistorySource, error) {
			history, err := storage.OpenReadOnly(cfg.Storage)
			if err != nil {
				return nil, err // not a nil *Store inside a non-nil interface
			}
			return history, nil
		},
	}

	if err := tui.RunHeadless(coll, opts); err != nil {
		log.Fatalf("TUI error: %v", err)
	}
}

func readPasswordWithAsterisks() string {
	fmt.Print("Enter password: ")
	fd := uintptr(syscall.Stdin)
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		// Not a terminal: read one line. Only its line ending is dropped, so
		// the hash is of exactly the password a login will send.
		reader := bufio.NewReader(os.Stdin)
		password, _ := reader.ReadString('\n')
		return strings.TrimRight(password, "\r\n")
	}
	defer func() { _ = term.Restore(fd, oldState) }()

	var password []byte
	b := make([]byte, 1)
	for {
		n, err := os.Stdin.Read(b)
		if err != nil || n == 0 {
			break
		}

		if b[0] == '\n' || b[0] == '\r' {
			fmt.Print("\n\r")
			break
		}

		if b[0] == 3 { // Ctrl+C
			_ = term.Restore(fd, oldState)
			os.Exit(1)
		}

		if b[0] == 127 || b[0] == '\b' { // Backspace
			if len(password) > 0 {
				password = password[:len(password)-1]
				fmt.Print("\b \b")
			}
			continue
		}

		password = append(password, b[0])
		fmt.Print("*")
	}
	return string(password)
}

// errTierNotInspected is returned by runInspectTier when a tier file exists
// but could not be inspected; `kula inspect` then exits 1.
var errTierNotInspected = errors.New("not inspected")

// runInspectTier prints every tier of cfg. args are the inspect flags (a
// -config among them was applied by the caller). A tier file that does not
// exist is reported and is not an error, as on a fresh install; one that
// exists but cannot be inspected is reported on stderr, the others are
// printed, and the result is an errTierNotInspected error.
func runInspectTier(cfg *config.Config, args []string, stdout, stderr io.Writer) error {
	inspectFlags := newCommandFlags("inspect", stderr)
	if err := inspectFlags.set.Parse(args); err != nil {
		return err
	}
	if inspectFlags.set.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", inspectFlags.set.Arg(0))
	}
	verbose := inspectFlags.verbose

	failed := 0
	for i := range cfg.Storage.Tiers {
		tierCfg := cfg.Storage.Tiers[i]
		path := filepath.Join(cfg.Storage.Directory, fmt.Sprintf("tier_%d.dat", i))
		info, err := storage.InspectTierFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				fmt.Fprintf(stdout, "File: %s (not found)\n", path)
				fmt.Fprintf(stdout, "Resolution: %s\n", tierCfg.Resolution)
				fmt.Fprintln(stdout, "Coverage ETA: unavailable (tier file not found)")
				fmt.Fprintln(stdout)
				continue
			}
			fmt.Fprintf(stderr, "Error inspecting tier file %s: %v\n\n", path, err)
			failed++
			continue
		}

		fmt.Fprintf(stdout, "File: %s\n", path)
		fmt.Fprintf(stdout, "Resolution: %s\n", tierCfg.Resolution)
		fmt.Fprintf(stdout, "Version: %d\n", info.Version)

		currentData := info.WriteOff
		if info.Wrapped {
			currentData = info.MaxData
		}
		pct := 0.0
		if info.MaxData > 0 {
			pct = float64(currentData) / float64(info.MaxData) * 100
		}
		fmt.Fprintf(stdout, "Data Size: %d / %d bytes (%.2f%%)\n", currentData, info.MaxData, pct)

		fmt.Fprintf(stdout, "Write Offset: %d\n", info.WriteOff)
		fmt.Fprintf(stdout, "Total Records: %d\n", info.Count)

		if !info.OldestTS.IsZero() {
			fmt.Fprintf(stdout, "Oldest Timestamp: %s\n", info.OldestTS.Format(time.RFC3339))
		} else {
			fmt.Fprintln(stdout, "Oldest Timestamp: (none)")
		}

		if !info.NewestTS.IsZero() {
			fmt.Fprintf(stdout, "Newest Timestamp: %s\n", info.NewestTS.Format(time.RFC3339))
		} else {
			fmt.Fprintln(stdout, "Newest Timestamp: (none)")
		}

		fmt.Fprintf(stdout, "Wrapped: %v\n", info.Wrapped)

		if !info.OldestTS.IsZero() && !info.NewestTS.IsZero() {
			fmt.Fprintf(stdout, "Time Range Covered: %s\n", info.NewestTS.Sub(info.OldestTS))
		}
		writeCoverageEstimate(stdout, info, tierCfg.Resolution)

		if verbose {
			latest, latestErr := storage.InspectLatestTierSample(path)
			if latestErr != nil {
				fmt.Fprintf(stderr, "Error reading latest record from %s: %v\n", path, latestErr)
				fmt.Fprintln(stdout, "Latest Recorded Metrics: unavailable")
			} else if latest == nil {
				fmt.Fprintln(stdout, "Latest Recorded Metrics: (none)")
			} else if err := writeLatestMetrics(stdout, latest); err != nil {
				fmt.Fprintf(stderr, "Error formatting latest record from %s: %v\n", path, err)
				fmt.Fprintln(stdout, "Latest Recorded Metrics: unavailable")
			}
		}
		fmt.Fprintln(stdout)
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d tier files %w", failed, len(cfg.Storage.Tiers), errTierNotInspected)
	}
	return nil
}

func writeCoverageEstimate(w io.Writer, info *storage.TierInfo, resolution time.Duration) {
	if info.Wrapped {
		fmt.Fprintln(w, "Coverage ETA: reached (tier has wrapped)")
		return
	}
	if info.Count == 0 || info.WriteOff <= 0 || info.MaxData <= 0 || resolution <= 0 {
		fmt.Fprintln(w, "Estimated Full Coverage: unavailable (no records yet)")
		fmt.Fprintln(w, "Coverage ETA: unavailable (no records yet)")
		return
	}

	averageRecordBytes := float64(info.WriteOff) / float64(info.Count)
	maxRecords := float64(info.MaxData) / averageRecordBytes
	remainingRecords := maxRecords - float64(info.Count)
	if remainingRecords < 0 {
		remainingRecords = 0
	}
	fmt.Fprintf(w, "Estimated Full Coverage: ~%s\n",
		formatInspectSeconds(maxRecords*resolution.Seconds()))
	fmt.Fprintf(w, "Coverage ETA: ~%s (assuming continuous collection)\n",
		formatInspectSeconds(remainingRecords*resolution.Seconds()))
}

func formatInspectSeconds(seconds float64) string {
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 {
		return "unavailable"
	}
	seconds = math.Round(seconds)
	if seconds < 60 {
		return fmt.Sprintf("%.0fs", seconds)
	}

	const (
		minute = 60.0
		hour   = 60 * minute
		day    = 24 * hour
	)
	var major, minor float64
	var majorUnit, minorUnit string
	switch {
	case seconds >= day:
		major = math.Floor(seconds / day)
		minor = math.Floor(math.Mod(seconds, day) / hour)
		majorUnit, minorUnit = "d", "h"
	case seconds >= hour:
		major = math.Floor(seconds / hour)
		minor = math.Floor(math.Mod(seconds, hour) / minute)
		majorUnit, minorUnit = "h", "m"
	default:
		major = math.Floor(seconds / minute)
		minor = math.Mod(seconds, minute)
		majorUnit, minorUnit = "m", "s"
	}
	if minor == 0 {
		return fmt.Sprintf("%.0f%s", major, majorUnit)
	}
	return fmt.Sprintf("%.0f%s %.0f%s", major, majorUnit, minor, minorUnit)
}

func writeLatestMetrics(w io.Writer, latest *storage.AggregatedSample) error {
	record := struct {
		Timestamp time.Time         `json:"timestamp"`
		Duration  string            `json:"duration"`
		Data      *collector.Sample `json:"data,omitempty"`
		Min       *collector.Sample `json:"min,omitempty"`
		Max       *collector.Sample `json:"max,omitempty"`
	}{
		Timestamp: latest.Timestamp,
		Duration:  latest.Duration.String(),
		Data:      latest.Data,
		Min:       latest.Min,
		Max:       latest.Max,
	}
	payload, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	fmt.Fprintln(w, "Latest Recorded Metrics:")
	_, err = fmt.Fprintln(w, string(payload))
	return err
}
