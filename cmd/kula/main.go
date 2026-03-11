package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"kula-szpiegula"
	"kula-szpiegula/internal/collector"
	"kula-szpiegula/internal/config"
	"kula-szpiegula/internal/sandbox"
	"kula-szpiegula/internal/storage"
	"kula-szpiegula/internal/tui"
	"kula-szpiegula/internal/web"

	"github.com/charmbracelet/x/term"
)

var version = kula.Version

func printUsage() {
	fmt.Fprintf(os.Stderr, `Kula-Szpiegula v%s — Lightweight Linux Server Monitor

Usage:
  kula [flags] [command]

Commands:
  serve          Start the monitoring daemon with web UI (default)
  tui            Launch the terminal UI dashboard
  hash-password  Generate an Argon2 password hash for config
  inspect        Display information about storage tier files

Flags:
  -config string  Path to configuration file (default "config.yaml")
  -h, --help      Show this help message

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
		fmt.Printf("Kula-Szpiegula v%s — Lightweight Linux Server Monitor\n", version)
		os.Exit(0)
	}

	osName := getOSName()
	kernelVersion := getKernelVersion()
	cpuArch := runtime.GOARCH

	cmd := "serve"
	if flag.NArg() > 0 {
		cmd = flag.Arg(0)
	}

	if cmd == "hash-password" {
		// Just to read the password, we don't return yet as we need the config
		password := readPasswordWithAsterisks()

		// Load config
		cfg, err := config.Load(*configPath)
		if err != nil {
			log.Fatalf("Failed to load config: %v", err)
		}

		web.PrintHashedPassword(password, cfg.Web.Auth.Argon2)
		return
	}

	// Load config for other commands
	cfg, err := config.Load(*configPath)
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
		runInspectTier(cfg)
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\nUsage: kula [serve|tui|hash-password|inspect]\n", cmd)
		os.Exit(1)
	}
}

func runServe(cfg *config.Config, configPath string, osName, kernelVersion, cpuArch string) {
	cfg.Web.Version = version
	cfg.Web.OS = osName
	cfg.Web.Kernel = kernelVersion
	cfg.Web.Arch = cpuArch
	cfg.Collection.DebugLog = cfg.Web.Logging.Enabled && cfg.Web.Logging.Level == "debug"
	coll := collector.New(cfg.Global, cfg.Collection)

	store, err := storage.NewStore(cfg.Storage)
	if err != nil {
		log.Fatalf("Failed to initialize storage: %v", err)
	}
	defer func() { _ = store.Close() }()

	// Enforce Landlock sandbox: restrict filesystem and network access
	// to only what Kula needs. Non-fatal on unsupported kernels.
	if err := sandbox.Enforce(configPath, cfg.Storage.Directory, cfg.Web.Port); err != nil {
		log.Printf("Warning: Landlock sandbox not enforced: %v", err)
	}

	server := web.NewServer(cfg.Web, cfg.Global, coll, store, cfg.Storage.Directory)

	// Signal handling with Context
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Collection loop
	go func() {
		ticker := time.NewTicker(cfg.Collection.Interval)
		defer ticker.Stop()

		// Initial collection
		sample := coll.Collect()
		if err := store.WriteSample(sample); err != nil {
			log.Printf("Storage write error: %v", err)
		}
		server.BroadcastSample(sample)

		for {
			select {
			case <-ticker.C:
				sample := coll.Collect()
				if err := store.WriteSample(sample); err != nil {
					log.Printf("Storage write error: %v", err)
				}
				server.BroadcastSample(sample)
			case <-ctx.Done():
				return
			}
		}
	}()

	// Start web server
	go func() {
		if err := server.Start(); err != nil {
			log.Fatalf("Web server error: %v", err)
		}
	}()

	log.Printf("Kula-Szpiegula v%s started (collecting every %s)", version, cfg.Collection.Interval)
	log.Printf("OS: %s, Kernel: %s, Arch: %s", osName, kernelVersion, cpuArch)
	<-ctx.Done()

	log.Println("Shutting down...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("Web server shutdown error: %v", err)
	}
}

func runTUI(cfg *config.Config, osName, kernelVersion, cpuArch string) {
	coll := collector.New(cfg.Global, cfg.Collection)
	if err := tui.RunHeadless(coll, cfg.TUI.RefreshRate, osName, kernelVersion, cpuArch, cfg.Global.ShowSystemInfo); err != nil {
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

func runInspectTier(cfg *config.Config) {
	for i := range cfg.Storage.Tiers {
		path := filepath.Join(cfg.Storage.Directory, fmt.Sprintf("tier_%d.dat", i))
		info, err := storage.InspectTierFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				fmt.Printf("File: %s (not found)\n\n", path)
				continue
			}
			fmt.Fprintf(os.Stderr, "Error inspecting tier file %s: %v\n\n", path, err)
			continue
		}

		fmt.Printf("File: %s\n", path)
		fmt.Printf("Version: %d\n", info.Version)

		currentData := info.WriteOff
		if info.Wrapped {
			currentData = info.MaxData
		}
		pct := 0.0
		if info.MaxData > 0 {
			pct = float64(currentData) / float64(info.MaxData) * 100
		}
		fmt.Printf("Data Size: %d / %d bytes (%.2f%%)\n", currentData, info.MaxData, pct)

		fmt.Printf("Write Offset: %d\n", info.WriteOff)
		fmt.Printf("Total Records: %d\n", info.Count)

		if !info.OldestTS.IsZero() {
			fmt.Printf("Oldest Timestamp: %s\n", info.OldestTS.Format(time.RFC3339))
		} else {
			fmt.Printf("Oldest Timestamp: (none)\n")
		}

		if !info.NewestTS.IsZero() {
			fmt.Printf("Newest Timestamp: %s\n", info.NewestTS.Format(time.RFC3339))
		} else {
			fmt.Printf("Newest Timestamp: (none)\n")
		}

		fmt.Printf("Wrapped: %v\n", info.Wrapped)

		if !info.OldestTS.IsZero() && !info.NewestTS.IsZero() {
			fmt.Printf("Time Range Covered: %s\n", info.NewestTS.Sub(info.OldestTS))
		}
		fmt.Println()
	}
}
