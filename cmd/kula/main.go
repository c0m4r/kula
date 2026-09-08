package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
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

func printUsage() {
	fmt.Fprintf(os.Stderr, `Kula v%s — Lightweight Linux Server Monitor

Usage:
  kula [flags] [command]

Commands:
  serve          Start the monitoring daemon with web UI (default)
  tui            Launch the terminal UI dashboard
  hash-password  Generate an Argon2 password hash for config
  inspect        Display storage tier resolution, coverage, and file information
  disks          List available disks and partitions with persistent IDs

Flags:
  -config string  Path to configuration file (default "config.yaml")
  -h, --help      Show this help message

Inspect flags:
  --verbose       Include the latest recorded metrics from each tier

`, version)
}

func main() {
	var showVersion bool
	var showVersionShort bool

	flag.Usage = printUsage
	flag.BoolVar(&showVersion, "version", false, "Print version and exit")
	flag.BoolVar(&showVersionShort, "v", false, "Print version and exit")
	configPath := flag.String("config", "config.yaml", "path to configuration file")
	flag.Parse()

	if showVersion || showVersionShort {
		fmt.Printf("Kula v%s — Lightweight Linux Server Monitor\n", version)
		return
	}

	cmd := "serve"
	if flag.NArg() > 0 {
		cmd = flag.Arg(0)
	}

	// Disk discovery must work before a config exists and must not seed a
	// config file, create storage directories, or initialize applications.
	if cmd == "disks" {
		if err := runDisks(os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to list disks: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// Track whether -config was explicitly provided. When it is, a missing or
	// unreadable file must abort startup rather than silently using defaults.
	configExplicit := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "config" {
			configExplicit = true
		}
	})
	loadConfig := config.Load
	if configExplicit {
		// When the user explicitly points -config at a path that doesn't exist,
		// seed it from the packaged example rather than refusing to start. Fall
		// back to built-in defaults if the file can't be written.
		if _, statErr := os.Stat(*configPath); os.IsNotExist(statErr) {
			if werr := os.WriteFile(*configPath, kula.ExampleConfig, 0o600); werr != nil {
				log.Printf("Warning: config %q did not exist and could not be seeded from the packaged example: %v; using built-in defaults", *configPath, werr)
			} else {
				log.Printf("Warning: config %q did not exist; seeded it from the packaged example", *configPath)
				loadConfig = config.LoadRequired
			}
		} else {
			loadConfig = config.LoadRequired
		}
	}

	osName := getOSName()
	kernelVersion := getKernelVersion()
	cpuArch := runtime.GOARCH

	if cmd == "serve" || cmd == "tui" {
		log.Printf("Kula v%s starting...", version)
	}

	if cmd == "hash-password" {
		// Just to read the password, we don't return yet as we need the config
		password := readPasswordWithAsterisks()

		// Load config
		cfg, err := loadConfig(*configPath)
		if err != nil {
			log.Fatalf("Failed to load config: %v", err)
		}

		web.PrintHashedPassword(password, cfg.Web.Auth.Argon2)
		return
	}

	// Load config for other commands
	cfg, err := loadConfig(*configPath)
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
		runServe(cfg, *configPath, osName, kernelVersion, cpuArch)
	case "tui":
		runTUI(cfg, osName, kernelVersion, cpuArch)
	case "inspect":
		if err := runInspectTier(cfg, flag.Args()[1:], os.Stdout, os.Stderr); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return
			}
			fmt.Fprintf(os.Stderr, "kula inspect: %v\n", err)
			os.Exit(2)
		}
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\nUsage: kula [serve|tui|hash-password|inspect|disks]\n", cmd)
		os.Exit(1)
	}
}

func runServe(cfg *config.Config, configPath string, osName, kernelVersion, cpuArch string) {
	cfg.Web.Version = version
	cfg.Web.OS = osName
	cfg.Web.Kernel = kernelVersion
	cfg.Web.Arch = cpuArch
	cfg.Collection.DebugLog = cfg.Web.Logging.Enabled && cfg.Web.Logging.Level == "debug"
	coll := collector.New(cfg.Global, cfg.Collection, cfg.Applications, cfg.Storage.Directory)

	store, err := storage.NewStore(cfg.Storage)
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

	// Enforce Landlock sandbox: restrict filesystem and network access
	// to only what Kula needs. Non-fatal on unsupported kernels.
	if err := sandbox.Enforce(configPath, cfg.Storage.Directory, cfg.Web, cfg.Applications, cfg.Ollama); err != nil {
		log.Printf("Warning: Landlock sandbox not enforced: %v", err)
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
	go func() {
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
	coll := collector.New(cfg.Global, cfg.Collection, cfg.Applications, cfg.Storage.Directory)
	defer coll.Stop()
	coll.StartApplications()
	if err := tui.RunHeadless(coll, cfg.TUI.RefreshRate, osName, kernelVersion, cpuArch, version, cfg.Global.ShowSystemInfo); err != nil {
		log.Fatalf("TUI error: %v", err)
	}
}

func readPasswordWithAsterisks() string {
	fmt.Print("Enter password: ")
	fd := uintptr(syscall.Stdin)
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		// Fallback to basic bufio if not running in a proper terminal
		reader := bufio.NewReader(os.Stdin)
		password, _ := reader.ReadString('\n')
		return strings.TrimSpace(password)
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

func runInspectTier(cfg *config.Config, args []string, stdout, stderr io.Writer) error {
	inspectFlags := flag.NewFlagSet("kula inspect", flag.ContinueOnError)
	inspectFlags.SetOutput(stderr)
	verbose := inspectFlags.Bool("verbose", false, "include the latest recorded metrics from each tier")
	inspectFlags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: kula [global flags] inspect [--verbose]")
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "Flags:")
		inspectFlags.PrintDefaults()
	}
	if err := inspectFlags.Parse(args); err != nil {
		return err
	}
	if inspectFlags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", inspectFlags.Arg(0))
	}

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

		if *verbose {
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
