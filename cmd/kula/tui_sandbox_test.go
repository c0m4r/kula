package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"kula/internal/collector"
	"kula/internal/config"
	"kula/internal/storage"

	llsyscall "github.com/landlock-lsm/go-landlock/landlock/syscall"
	"golang.org/x/sys/unix"
)

// openPTY opens a pseudo-terminal pair sized cols x rows.
func openPTY(t *testing.T, cols, rows uint16) (master, slave *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pseudo-terminal: %v", err)
	}
	fd := int(master.Fd())
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	n, err := unix.IoctlGetUint32(fd, unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	slave, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.IoctlSetWinsize(int(slave.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: rows, Col: cols}); err != nil {
		t.Fatal(err)
	}
	return master, slave
}

// screen collects what a program writes to its terminal.
type screen struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *screen) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *screen) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// TestTUIUnderTheSandbox runs `kula tui` on a pseudo-terminal, under the
// sandbox it applies to itself: it draws, its collectors reach their targets,
// its History view charts storage another process wrote, and it quits on q —
// also with standard input redirected, when it reads keys from /dev/tty.
func TestTUIUnderTheSandbox(t *testing.T) {
	if os.Getenv("KULA_TEST_SERVE_ARGS") != "" {
		runServeMain()
	}
	if testing.Short() {
		t.Skip("starts kula tui")
	}
	t.Run("stdin is the terminal", func(t *testing.T) { runTUIOnPTY(t, true) })
	t.Run("stdin is not a terminal", func(t *testing.T) { runTUIOnPTY(t, false) })
}

func runTUIOnPTY(t *testing.T, stdinIsTerminal bool) {
	dir := t.TempDir()
	storageDir := filepath.Join(dir, "storage")
	fakes := &fakeTargets{seen: map[string]string{}}
	dockerSock := filepath.Join(dir, "docker", "docker.sock")
	fakes.serveHTTP(t, "containers", listenUnix(t, dockerSock), "[]")
	nginxLn := listenTCP(t)
	fakes.serveHTTP(t, "nginx", nginxLn, "Active connections: 1 \nserver accepts handled requests\n 1 1 1 \nReading: 0 Writing: 1 Waiting: 0 \n")
	pgLn := listenTCP(t)
	fakes.acceptAndClose(t, "postgres", pgLn)

	configPath := filepath.Join(dir, "config.yaml")
	cfgText := fmt.Sprintf(`storage:
  directory: %q
  tiers:
    - resolution: 1s
      max_size: 1MB
    - resolution: 1m
      max_size: 1MB
    - resolution: 5m
      max_size: 1MB
applications:
  containers:
    enabled: true
    socket_path: %q
  nginx:
    enabled: true
    status_url: "http://127.0.0.1:%d/status"
  postgres:
    enabled: true
    host: 127.0.0.1
    port: %d
    user: kula
    sslmode: disable
`, storageDir, dockerSock, portOf(nginxLn), portOf(pgLn))
	if err := os.WriteFile(configPath, []byte(cfgText), 0o600); err != nil {
		t.Fatal(err)
	}

	// History recorded by another process, as `kula serve` would.
	cfg, err := config.LoadRequired(configPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.NewStore(cfg.Storage)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for i := 30; i > 0; i-- {
		sample := &collector.Sample{Timestamp: now.Add(-time.Duration(i) * time.Second)}
		sample.Memory.UsedPercent = 42
		if err := store.WriteSample(sample); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	master, slave := openPTY(t, 140, 50)
	defer func() { _ = master.Close() }()
	out := &screen{}
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				_, _ = out.Write(buf[:n])
				// Answer the terminal queries the styling library sends at
				// start (background colour, cursor position), as a terminal
				// would, rather than leave it to time out.
				if bytes.Contains(buf[:n], []byte("\x1b]11;?")) {
					_, _ = master.Write([]byte("\x1b]11;rgb:0000/0000/0000\x1b\\"))
				}
				if bytes.Contains(buf[:n], []byte("\x1b[6n")) {
					_, _ = master.Write([]byte("\x1b[1;1R"))
				}
			}
			if err != nil {
				return
			}
		}
	}()

	cmd := exec.Command(os.Args[0], "-test.run=^TestTUIUnderTheSandbox$")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "TERM=xterm-256color",
		"KULA_TEST_SERVE_ARGS="+strings.Join([]string{"-config", configPath, "tui"}, "\x1f"))
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if !stdinIsTerminal {
		devNull, err := os.Open(os.DevNull)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = devNull.Close() }()
		cmd.Stdin = devNull
		cmd.SysProcAttr.Ctty = 1 // the terminal is standard output
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = slave.Close()
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})

	waitFor := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s; terminal output:\n%q", what, out.String())
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	if abi, err := llsyscall.LandlockGetABIVersion(); err == nil && abi >= 1 {
		waitFor("the TUI sandbox", func() bool { return strings.Contains(out.String(), "Landlock sandbox enforced for the TUI") })
	}
	waitFor("live samples", func() bool { return strings.Contains(out.String(), "LIVE") })
	waitFor("every collector to reach its target", func() bool { return len(fakes.missing([]string{"containers", "nginx", "postgres"})) == 0 })

	if _, err := master.Write([]byte("8")); err != nil {
		t.Fatal(err)
	}
	waitFor("the History view to chart the stored samples", func() bool { return strings.Contains(out.String(), "CPU USAGE") })

	if _, err := master.Write([]byte("q")); err != nil {
		t.Fatal(err)
	}
	switch code := waitExit(cmd, 10*time.Second); code {
	case 0:
	case -1:
		t.Fatalf("kula tui did not quit; terminal output:\n%q", out.String())
	default:
		t.Errorf("kula tui exited with status %d; terminal output:\n%q", code, out.String())
	}
	if text := out.String(); strings.Contains(text, "permission denied") || strings.Contains(text, "operation not permitted") {
		t.Errorf("denied under the TUI sandbox; terminal output:\n%q", text)
	}
}
