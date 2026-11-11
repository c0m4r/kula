package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	llsyscall "github.com/landlock-lsm/go-landlock/landlock/syscall"
)

// runServeMain runs main() with the arguments in KULA_TEST_SERVE_ARGS, for
// the tests that start `kula serve` in a child process.
func runServeMain() {
	flag.CommandLine = flag.NewFlagSet("kula", flag.ExitOnError)
	os.Args = append([]string{"kula"}, strings.Split(os.Getenv("KULA_TEST_SERVE_ARGS"), "\x1f")...)
	main()
	os.Exit(0)
}

// startKula starts `kula <args>` in a child process (main in the test
// binary, entered through the named test) and returns it with the file its
// log goes to.
func startKula(t *testing.T, testName, dir string, args ...string) (*exec.Cmd, string) {
	t.Helper()
	logPath := filepath.Join(dir, "kula.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logFile.Close() }()
	cmd := exec.Command(os.Args[0], "-test.run=^"+testName+"$")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "KULA_TEST_SERVE_ARGS="+strings.Join(args, "\x1f"))
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	return cmd, logPath
}

func readLog(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// fakeTargets records which fake servers Kula reached.
type fakeTargets struct {
	mu   sync.Mutex
	seen map[string]string
}

func (f *fakeTargets) hit(name, detail string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.seen[name]; !ok {
		f.seen[name] = detail
	}
}

func (f *fakeTargets) missing(names []string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var missing []string
	for _, name := range names {
		if _, ok := f.seen[name]; !ok {
			missing = append(missing, name)
		}
	}
	return missing
}

// serveHTTP answers HTTP on ln, recording each request under name.
func (f *fakeTargets) serveHTTP(t *testing.T, name string, ln net.Listener, body string) {
	t.Helper()
	srv := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			f.hit(name, r.URL.Path)
			_, _ = io.WriteString(w, body)
		}),
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
}

// acceptAndClose records each connection to ln under name and closes it: a
// database server that refuses the handshake.
func (f *fakeTargets) acceptAndClose(t *testing.T, name string, ln net.Listener) {
	t.Helper()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			f.hit(name, conn.LocalAddr().String())
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
}

func listenTCP(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

func listenUnix(t *testing.T, path string) net.Listener {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

func portOf(ln net.Listener) int { return ln.Addr().(*net.TCPAddr).Port }

// TestServeUnderTheSandbox runs `kula serve` with every collector and listener
// pointed at fakes outside its sandbox, and checks each is still reached
// under the rules the kernel enforces: the container runtime socket (in a
// directory of its own, and as a file in a shared directory), PostgreSQL and
// MySQL over TCP and over a Unix socket, nginx and Apache2 status pages by IP
// and by a hosts-file name, the custom-metrics socket, the web server on a
// Unix socket and on TCP, /metrics, the Ollama proxy at localhost, tier
// writes and the session file written at shutdown.
func TestServeUnderTheSandbox(t *testing.T) {
	if os.Getenv("KULA_TEST_SERVE_ARGS") != "" {
		runServeMain()
	}
	if testing.Short() {
		t.Skip("starts kula serve twice")
	}

	for _, variant := range []string{"web on a Unix socket", "web on TCP"} {
		t.Run(variant, func(t *testing.T) {
			unixWeb := variant == "web on a Unix socket"
			dir := t.TempDir()
			storageDir := filepath.Join(dir, "storage")
			fakes := &fakeTargets{seen: map[string]string{}}

			// Container runtime: a socket in a directory of its own, or
			// one directly in the shared temporary directory.
			dockerSock := filepath.Join(dir, "docker", "docker.sock")
			if !unixWeb {
				dockerSock = filepath.Join(os.TempDir(), fmt.Sprintf("kula-e2e-%d-%s-docker.sock", os.Getpid(), filepath.Base(dir)))
			}
			fakes.serveHTTP(t, "containers", listenUnix(t, dockerSock), "[]")

			// Databases: PostgreSQL over TCP and MySQL over a socket, or
			// the other way round.
			var pgHost, mysqlHost string
			var pgPort, mysqlPort int
			if unixWeb {
				ln := listenTCP(t)
				pgHost, pgPort = "127.0.0.1", portOf(ln)
				fakes.acceptAndClose(t, "postgres", ln)
				mysqlHost = filepath.Join(dir, "mysqld", "mysqld.sock")
				fakes.acceptAndClose(t, "mysql", listenUnix(t, mysqlHost))
			} else {
				pgHost = filepath.Join(dir, "postgresql")
				fakes.acceptAndClose(t, "postgres", listenUnix(t, filepath.Join(pgHost, ".s.PGSQL.5432")))
				ln := listenTCP(t)
				mysqlHost, mysqlPort = "localhost", portOf(ln)
				fakes.acceptAndClose(t, "mysql", ln)
			}

			// Status pages, one by IP and one by a hosts-file name.
			nginxLn, apacheLn := listenTCP(t), listenTCP(t)
			fakes.serveHTTP(t, "nginx", nginxLn, "Active connections: 1 \nserver accepts handled requests\n 1 1 1 \nReading: 0 Writing: 1 Waiting: 0 \n")
			fakes.serveHTTP(t, "apache2", apacheLn, "Total Accesses: 10\nTotal kBytes: 1\nBusyWorkers: 1\nIdleWorkers: 2\n")
			nginxURL := fmt.Sprintf("http://127.0.0.1:%d/status", portOf(nginxLn))
			apacheURL := fmt.Sprintf("http://localhost:%d/server-status?auto", portOf(apacheLn))
			if !unixWeb {
				nginxURL = fmt.Sprintf("http://localhost:%d/status", portOf(nginxLn))
				apacheURL = fmt.Sprintf("http://127.0.0.1:%d/server-status?auto", portOf(apacheLn))
			}

			ollamaLn := listenTCP(t)
			fakes.serveHTTP(t, "ollama", ollamaLn, `{"models":[{"name":"e2e-model:latest"}]}`)

			webSock := filepath.Join(dir, "run", "kula.sock")
			if err := os.Mkdir(filepath.Dir(webSock), 0o700); err != nil {
				t.Fatal(err)
			}
			webListen := fmt.Sprintf("  unix_socket: %q\n", webSock)
			client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", webSock)
				},
			}}
			base := "http://kula"
			if !unixWeb {
				ln := listenTCP(t)
				port := portOf(ln)
				_ = ln.Close()
				webListen = fmt.Sprintf("  listen: 127.0.0.1\n  port: %d\n", port)
				client = &http.Client{Timeout: 5 * time.Second}
				base = fmt.Sprintf("http://127.0.0.1:%d", port)
			}

			configPath := filepath.Join(dir, "config.yaml")
			cfg := fmt.Sprintf(`storage:
  directory: %q
  tiers:
    - resolution: 1s
      max_size: 1MB
    - resolution: 1m
      max_size: 1MB
    - resolution: 5m
      max_size: 1MB
web:
%s  prometheus_metrics:
    enabled: true
applications:
  containers:
    enabled: true
    socket_path: %q
  postgres:
    enabled: true
    host: %q
    port: %d
    user: kula
    dbname: postgres
    sslmode: disable
  mysql:
    enabled: true
    host: %q
    port: %d
    user: kula
  nginx:
    enabled: true
    status_url: %q
  apache2:
    enabled: true
    status_url: %q
  custom:
    e2e:
      - name: value
        unit: x
        max: 100
ollama:
  enabled: true
  url: "http://localhost:%d"
`, storageDir, webListen, dockerSock, pgHost, pgPort, mysqlHost, mysqlPort, nginxURL, apacheURL, portOf(ollamaLn))
			if err := os.WriteFile(configPath, []byte(cfg), 0o600); err != nil {
				t.Fatal(err)
			}

			cmd, logPath := startKula(t, "TestServeUnderTheSandbox", dir, "-config", configPath, "serve")
			get := func(path string) (int, string, error) {
				resp, err := client.Get(base + path)
				if err != nil {
					return 0, "", err
				}
				defer func() { _ = resp.Body.Close() }()
				body, err := io.ReadAll(resp.Body)
				return resp.StatusCode, string(body), err
			}
			waitFor := func(what string, ok func() bool) {
				t.Helper()
				deadline := time.Now().Add(20 * time.Second)
				for !ok() {
					if time.Now().After(deadline) {
						t.Fatalf("timed out waiting for %s\nlog:\n%s", what, readLog(t, logPath))
					}
					if cmd.ProcessState != nil {
						t.Fatalf("kula exited waiting for %s\nlog:\n%s", what, readLog(t, logPath))
					}
					time.Sleep(100 * time.Millisecond)
				}
			}

			waitFor("the web server", func() bool {
				code, _, err := get("/health")
				return err == nil && code == http.StatusOK
			})
			targets := []string{"containers", "postgres", "mysql", "nginx", "apache2"}
			waitFor("every collector to reach its target", func() bool { return len(fakes.missing(targets)) == 0 })

			// The custom-metrics socket, which Kula created in its storage
			// directory, takes a reading from outside.
			waitFor("the custom-metrics socket", func() bool {
				conn, err := net.Dial("unix", filepath.Join(storageDir, "kula.sock"))
				if err != nil {
					return false
				}
				defer func() { _ = conn.Close() }()
				_, err = io.WriteString(conn, `{"custom":{"e2e":[{"value":42}]}}`+"\n")
				return err == nil
			})
			waitFor("the custom metric in /metrics", func() bool {
				code, body, err := get("/metrics")
				return err == nil && code == http.StatusOK && strings.Contains(body, `source="e2e",name="value"} 42`)
			})

			// The Ollama proxy reaches its backend at localhost.
			code, body, err := get("/api/ollama/models")
			if err != nil || code != http.StatusOK || !strings.Contains(body, "e2e-model:latest") {
				t.Errorf("GET /api/ollama/models = %d %q, %v; want the fake backend's model\nlog:\n%s", code, body, err, readLog(t, logPath))
			}

			if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			switch code := waitExit(cmd, 20*time.Second); code {
			case 0:
			case -1:
				t.Fatalf("kula serve did not stop\nlog:\n%s", readLog(t, logPath))
			default:
				t.Errorf("kula serve exited with status %d", code)
			}

			logText := readLog(t, logPath)
			if abi, err := llsyscall.LandlockGetABIVersion(); err == nil && abi >= 1 && !strings.Contains(logText, "Landlock sandbox enforced") {
				t.Errorf("the sandbox was not enforced:\n%s", logText)
			}
			scanner := bufio.NewScanner(strings.NewReader(logText))
			for scanner.Scan() {
				if line := scanner.Text(); strings.Contains(line, "permission denied") || strings.Contains(line, "operation not permitted") {
					t.Errorf("denied under the sandbox: %s", line)
				}
			}
			for _, name := range []string{"tier_0.dat", "sessions.json"} {
				if info, err := os.Stat(filepath.Join(storageDir, name)); err != nil || info.Size() == 0 {
					t.Errorf("%s not written under the sandbox: %v", name, err)
				}
			}
			if _, err := os.Stat(webSock); unixWeb && !errors.Is(err, os.ErrNotExist) {
				t.Errorf("web socket not removed at shutdown: %v", err)
			}
		})
	}
}

// waitExit waits up to timeout for cmd to exit and returns its exit status,
// or -1 after killing it when it is still running.
func waitExit(cmd *exec.Cmd, timeout time.Duration) int {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		if err != nil {
			return -2
		}
		return 0
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		<-done
		return -1
	}
}

// TestServeRefusesToStartWithoutTheSandbox: on a kernel with Landlock, a grant
// the config asks for that cannot be applied — a PostgreSQL socket directory
// that is a regular file — stops Kula with a message naming the setting,
// where it used to log a warning and serve without any sandbox.
func TestServeRefusesToStartWithoutTheSandbox(t *testing.T) {
	if os.Getenv("KULA_TEST_SERVE_ARGS") != "" {
		runServeMain()
	}
	if abi, err := llsyscall.LandlockGetABIVersion(); err != nil || abi < 1 {
		t.Skip("no Landlock in this kernel: Kula starts unconfined by design")
	}

	dir := t.TempDir()
	notADir := filepath.Join(dir, "postgresql")
	if err := os.WriteFile(notADir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "run"), 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.yaml")
	cfg := fmt.Sprintf(`storage:
  directory: %q
web:
  unix_socket: %q
applications:
  postgres:
    enabled: true
    host: %q
    port: 0
`, filepath.Join(dir, "storage"), filepath.Join(dir, "run", "kula.sock"), notADir)
	if err := os.WriteFile(configPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd, logPath := startKula(t, "TestServeRefusesToStartWithoutTheSandbox", dir, "-config", configPath, "serve")
	code := waitExit(cmd, 15*time.Second)
	logText := readLog(t, logPath)
	if code == -1 {
		t.Fatalf("kula serve kept running although its sandbox could not be applied:\n%s", logText)
	}
	if code == 0 || !strings.Contains(logText, "Refusing to start without the Landlock sandbox") ||
		!strings.Contains(logText, "applications.postgres.host") {
		t.Errorf("exit %d; want a refusal naming applications.postgres.host:\n%s", code, logText)
	}
}
