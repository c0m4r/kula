package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"kula-szpiegula/internal/collector"
	"kula-szpiegula/internal/config"
	"kula-szpiegula/internal/sandbox"
	"kula-szpiegula/internal/storage"
	"kula-szpiegula/internal/tui"
	"kula-szpiegula/internal/web"

	"github.com/charmbracelet/x/term"
)

var version = "0.1.0"

func init() {
	if data, err := os.ReadFile("VERSION"); err == nil {
		if v := strings.TrimSpace(string(data)); v != "" {
			version = v
		}
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `Kula-Szpiegula v%s — Lightweight Linux Server Monitor

Usage:
  kula [flags] [command]

Commands:
  serve          Start the monitoring daemon with web UI (default)
  tui            Launch the terminal UI dashboard
  hash-password  Generate an Argon2 password hash for config

Flags:
  -config string  Path to configuration file (default "config.yaml")
  -h, --help      Show this help message

Examples:
  kula                              Start with default config
  kula -config /etc/kula/config.yaml serve
  kula tui
  kula hash-password

`, version)
}

func main() {
	flag.Usage = printUsage
	configPath := flag.String("config", "config.yaml", "path to configuration file")
	flag.Parse()

	cmd := "serve"
	if flag.NArg() > 0 {
		cmd = flag.Arg(0)
	}

	// Handle hash-password command first (doesn't need config)
	if cmd == "hash-password" {
		password := readPasswordWithAsterisks()
		web.PrintHashedPassword(password)
		return
	}

	// Load config
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	switch cmd {
	case "serve":
		runServe(cfg, *configPath)
	case "tui":
		runTUI(cfg)
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\nUsage: kula [serve|tui|hash-password]\n", cmd)
		os.Exit(1)
	}
}

func runServe(cfg *config.Config, configPath string) {
	cfg.Web.Version = version
	coll := collector.New()

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

	server := web.NewServer(cfg.Web, coll, store)

	// Signal handling
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

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

		for range ticker.C {
			sample := coll.Collect()
			if err := store.WriteSample(sample); err != nil {
				log.Printf("Storage write error: %v", err)
			}
			server.BroadcastSample(sample)
		}
	}()

	// Start web server
	go func() {
		if err := server.Start(); err != nil {
			log.Fatalf("Web server error: %v", err)
		}
	}()

	log.Printf("Kula-Szpiegula started (collecting every %s)", cfg.Collection.Interval)
	<-sigCh
	log.Println("Shutting down...")
}

func runTUI(cfg *config.Config) {
	coll := collector.New()
	if err := tui.RunHeadless(coll, cfg.TUI.RefreshRate); err != nil {
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
	defer term.Restore(fd, oldState)

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
			term.Restore(fd, oldState)
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
