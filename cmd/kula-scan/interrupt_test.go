package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestInterruptLogsOutAndExits130: cancelling the run's context mid-scan
// (what SIGINT does) stops the scan, logs the session out and prints the
// partial report.
func TestInterruptLogsOutAndExits130(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ledger := &loginLedger{attempts: map[string][]string{}}
	mock := ledger.wrap(secureMock())
	// The metrics check is the sixth; the interrupt arrives while it waits
	// for its first answer.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			cancel()
		}
		mock.ServeHTTP(w, r)
	}))
	defer ts.Close()
	t.Setenv(passwordEnv, mockPass)

	code, stdout, stderr := runCapturedContext(t, ctx, "-json", "-username", mockUser, ts.URL)
	if code != exitInterrupted {
		t.Fatalf("exit %d, want 130; stderr:\n%s", code, stderr)
	}
	report := decodeReport(t, stdout)
	if !report.Interrupted || report.NotRun == 0 {
		t.Errorf("interrupted=%v not_run=%d, want true and the checks left", report.Interrupted, report.NotRun)
	}
	// Checks that finished are reported; the one cut short is not.
	wantStatus(t, report.Findings, "CSRF-003", StatusPass)
	for _, id := range []string{"PROM-001", "PROM-002", "WS-000", "INPUT-001"} {
		if f, ok := byID(report.Findings, id); ok {
			t.Errorf("%s reported after the interrupt: %+v", id, f)
		}
	}
	if !strings.Contains(stderr, "kula-scan: exit 130: interrupted;") {
		t.Errorf("no closing line explains the exit:\n%s", stderr)
	}

	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if len(ledger.logouts) == 0 {
		t.Fatal("the interrupted run left its session logged in")
	}
	last := ledger.logouts[len(ledger.logouts)-1]
	if last.Get("Cookie") != "kula_session="+mockToken || last.Get("X-CSRF-Token") != "csrftok" {
		t.Errorf("the closing logout carries Cookie %q, X-CSRF-Token %q", last.Get("Cookie"), last.Get("X-CSRF-Token"))
	}
}

// TestInterruptAbortsAWaitingProbe: a probe waiting out -dos-wait returns as
// soon as the run is interrupted.
func TestInterruptAbortsAWaitingProbe(t *testing.T) {
	ts := httptest.NewServer(secureMock()) // no ReadTimeout: slow requests hang
	defer ts.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	timer := time.AfterFunc(time.Second, cancel)
	defer timer.Stop()

	start := time.Now()
	code, stdout, _ := runCapturedContext(t, ctx, "-aggressive", "-only", "DOS-SLOWLORIS", "-dos-wait", "60s", "-no-color", ts.URL)
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Errorf("the interrupted run took %s; the probe kept waiting", elapsed)
	}
	if code != exitInterrupted {
		t.Errorf("exit %d, want 130", code)
	}
	if !strings.Contains(stdout, "Interrupted: 1 check did not run.") || strings.Contains(stdout, "DOS-SLOWLORIS") {
		t.Errorf("the text report does not show the interrupt:\n%s", stdout)
	}
}

func TestInterruptDuringDiscoveryReportsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ts := httptest.NewServer(secureMock())
	defer ts.Close()
	code, stdout, _ := runCapturedContext(t, ctx, "-json", ts.URL)
	report := decodeReport(t, stdout)
	if code != exitInterrupted || len(report.Findings) != 0 || report.NotRun == 0 {
		t.Errorf("exit %d, findings %v, not_run %d", code, reportedIDs(report), report.NotRun)
	}
}
