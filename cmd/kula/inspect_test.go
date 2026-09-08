package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kula/internal/collector"
	"kula/internal/config"
	"kula/internal/storage"
)

func TestRunInspectTierReportsResolutionCoverageAndLatestMetrics(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tier_0.dat")
	tier, err := storage.OpenTier(path, 64*1024)
	if err != nil {
		t.Fatalf("OpenTier() error: %v", err)
	}
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	if err := tier.Write(&storage.AggregatedSample{
		Timestamp: now,
		Duration:  time.Second,
		Data: &collector.Sample{
			Timestamp: now,
			CPU: collector.CPUStats{
				Total: collector.CPUCoreStats{Usage: 42},
			},
			System: collector.SystemStats{Hostname: "inspect-test"},
		},
	}); err != nil {
		t.Fatalf("Write() error: %v", err)
	}
	if err := tier.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}

	cfg := &config.Config{Storage: config.StorageConfig{
		Directory: dir,
		Tiers: []config.TierConfig{
			{Resolution: time.Second, MaxSize: "64KB", MaxBytes: 64 * 1024},
		},
	}}
	var stdout, stderr bytes.Buffer
	if err := runInspectTier(cfg, []string{"--verbose"}, &stdout, &stderr); err != nil {
		t.Fatalf("runInspectTier() error: %v", err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("runInspectTier() stderr: %s", stderr.String())
	}

	for _, want := range []string{
		"Resolution: 1s",
		"Estimated Full Coverage: ~",
		"Coverage ETA: ~",
		"Latest Recorded Metrics:",
		`"usage": 42`,
		`"hostname": "inspect-test"`,
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("inspect output missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestRunInspectTierMissingFileStillReportsResolution(t *testing.T) {
	cfg := &config.Config{Storage: config.StorageConfig{
		Directory: t.TempDir(),
		Tiers: []config.TierConfig{
			{Resolution: time.Minute},
		},
	}}
	var stdout, stderr bytes.Buffer
	if err := runInspectTier(cfg, nil, &stdout, &stderr); err != nil {
		t.Fatalf("runInspectTier() error: %v", err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("runInspectTier() stderr: %s", stderr.String())
	}
	for _, want := range []string{
		"(not found)",
		"Resolution: 1m0s",
		"Coverage ETA: unavailable",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("inspect output missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestFormatInspectSeconds(t *testing.T) {
	tests := []struct {
		seconds float64
		want    string
	}{
		{seconds: 5, want: "5s"},
		{seconds: 65, want: "1m 5s"},
		{seconds: 3660, want: "1h 1m"},
		{seconds: 2*24*60*60 + 3*60*60, want: "2d 3h"},
	}
	for _, test := range tests {
		if got := formatInspectSeconds(test.seconds); got != test.want {
			t.Errorf("formatInspectSeconds(%v) = %q, want %q", test.seconds, got, test.want)
		}
	}
}
