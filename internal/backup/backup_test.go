package backup

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"kula/internal/config"
)

// fakeStore is a minimal Snapshotter that emits deterministic bytes per tier.
type fakeStore struct {
	tiers int
	fail  int // tier index to fail on, or -1
}

func (f *fakeStore) TierCount() int { return f.tiers }

func (f *fakeStore) SnapshotTier(i int, w io.Writer) (int64, error) {
	if i == f.fail {
		return 0, fmt.Errorf("induced failure on tier %d", i)
	}
	data := bytes.Repeat([]byte{byte('0' + i)}, 1024)
	n, err := w.Write(data)
	return int64(n), err
}

func newSched(t *testing.T, store Snapshotter, cfg config.BackupConfig) (*Scheduler, string) {
	t.Helper()
	dir := t.TempDir()
	dur, err := parseRetentionForTest(cfg.Retention)
	if err != nil {
		t.Fatalf("retention: %v", err)
	}
	cfg.RetentionDur = dur
	s, err := New(store, dir, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := os.MkdirAll(s.backupDir, 0750); err != nil {
		t.Fatalf("mkdir backupDir: %v", err)
	}
	return s, dir
}

// parseRetentionForTest mirrors the tiny config parser without importing it
// indirectly; it keeps the test self-contained.
func parseRetentionForTest(s string) (time.Duration, error) {
	switch s {
	case "", "0":
		return 0, nil
	case "1d":
		return 24 * time.Hour, nil
	case "1h":
		return time.Hour, nil
	default:
		return 0, fmt.Errorf("unsupported test retention %q", s)
	}
}

func TestRunBackupCompressed(t *testing.T) {
	s, _ := newSched(t, &fakeStore{tiers: 3, fail: -1}, config.BackupConfig{
		Enabled: true, Cron: "0 0 * * *", MaxTier: 2, Retention: "1d", Compress: true,
	})

	now := time.Date(2026, 6, 4, 0, 0, 0, 0, time.Local)
	if err := s.RunBackup(now); err != nil {
		t.Fatalf("RunBackup: %v", err)
	}

	runDir := filepath.Join(s.backupDir, now.Format(runDirLayout))
	// maxtier=2 -> tier 0 and 1, compressed only.
	for i := 0; i < 2; i++ {
		gz := filepath.Join(runDir, fmt.Sprintf("tier_%d.dat.gz", i))
		raw := filepath.Join(runDir, fmt.Sprintf("tier_%d.dat", i))
		if _, err := os.Stat(gz); err != nil {
			t.Errorf("expected %s: %v", gz, err)
		}
		if _, err := os.Stat(raw); !os.IsNotExist(err) {
			t.Errorf("uncompressed %s should have been removed", raw)
		}
		// Verify decompressed content matches the fake snapshot.
		assertGzipContent(t, gz, bytes.Repeat([]byte{byte('0' + i)}, 1024))
	}
	// tier 2 must NOT be backed up (maxtier=2).
	if _, err := os.Stat(filepath.Join(runDir, "tier_2.dat.gz")); !os.IsNotExist(err) {
		t.Errorf("tier_2 should not be backed up with maxtier=2")
	}
	// No leftover temp dir.
	if _, err := os.Stat(runDir + tmpSuffix); !os.IsNotExist(err) {
		t.Errorf("temp dir should have been renamed away")
	}
}

func TestRunBackupUncompressed(t *testing.T) {
	s, _ := newSched(t, &fakeStore{tiers: 1, fail: -1}, config.BackupConfig{
		Enabled: true, Cron: "0 0 * * *", MaxTier: 5, Retention: "1d", Compress: false,
	})

	now := time.Date(2026, 6, 4, 0, 0, 0, 0, time.Local)
	if err := s.RunBackup(now); err != nil {
		t.Fatalf("RunBackup: %v", err)
	}
	runDir := filepath.Join(s.backupDir, now.Format(runDirLayout))
	raw := filepath.Join(runDir, "tier_0.dat")
	got, err := os.ReadFile(raw)
	if err != nil {
		t.Fatalf("read %s: %v", raw, err)
	}
	if !bytes.Equal(got, bytes.Repeat([]byte{'0'}, 1024)) {
		t.Errorf("tier_0.dat content mismatch")
	}
	// maxtier(5) clamped to available tiers(1): only tier_0 exists.
	if _, err := os.Stat(filepath.Join(runDir, "tier_1.dat")); !os.IsNotExist(err) {
		t.Errorf("only one tier should be backed up")
	}
}

func TestRunBackupFailureLeavesNoPartial(t *testing.T) {
	s, _ := newSched(t, &fakeStore{tiers: 3, fail: 1}, config.BackupConfig{
		Enabled: true, Cron: "0 0 * * *", MaxTier: 3, Retention: "1d", Compress: true,
	})
	now := time.Date(2026, 6, 4, 0, 0, 0, 0, time.Local)
	if err := s.RunBackup(now); err == nil {
		t.Fatal("expected RunBackup to fail")
	}
	runDir := filepath.Join(s.backupDir, now.Format(runDirLayout))
	if _, err := os.Stat(runDir); !os.IsNotExist(err) {
		t.Errorf("failed run must not leave a committed dir")
	}
	if _, err := os.Stat(runDir + tmpSuffix); !os.IsNotExist(err) {
		t.Errorf("failed run must clean up its temp dir")
	}
}

func TestCleanupPrunesExpired(t *testing.T) {
	s, _ := newSched(t, &fakeStore{tiers: 1, fail: -1}, config.BackupConfig{
		Enabled: true, Cron: "0 0 * * *", MaxTier: 1, Retention: "1d", Compress: false,
	})
	now := time.Date(2026, 6, 4, 0, 0, 0, 0, time.Local)

	old := now.Add(-48 * time.Hour).Format(runDirLayout)
	recent := now.Add(-1 * time.Hour).Format(runDirLayout)
	foreign := "not-a-backup"
	leftoverTmp := now.Format(runDirLayout) + tmpSuffix

	for _, name := range []string{old, recent, foreign, leftoverTmp} {
		if err := os.MkdirAll(filepath.Join(s.backupDir, name), 0750); err != nil {
			t.Fatal(err)
		}
	}

	s.cleanup(now)

	if _, err := os.Stat(filepath.Join(s.backupDir, old)); !os.IsNotExist(err) {
		t.Errorf("expired backup %s should be pruned", old)
	}
	if _, err := os.Stat(filepath.Join(s.backupDir, recent)); err != nil {
		t.Errorf("recent backup %s should be kept: %v", recent, err)
	}
	if _, err := os.Stat(filepath.Join(s.backupDir, foreign)); err != nil {
		t.Errorf("foreign dir %s must not be touched: %v", foreign, err)
	}
	if _, err := os.Stat(filepath.Join(s.backupDir, leftoverTmp)); !os.IsNotExist(err) {
		t.Errorf("leftover temp dir %s should be removed", leftoverTmp)
	}
}

func TestCleanupNoRetentionKeepsAll(t *testing.T) {
	s, _ := newSched(t, &fakeStore{tiers: 1, fail: -1}, config.BackupConfig{
		Enabled: true, Cron: "0 0 * * *", MaxTier: 1, Retention: "", Compress: false,
	})
	now := time.Date(2026, 6, 4, 0, 0, 0, 0, time.Local)
	old := now.Add(-1000 * time.Hour).Format(runDirLayout)
	if err := os.MkdirAll(filepath.Join(s.backupDir, old), 0750); err != nil {
		t.Fatal(err)
	}
	s.cleanup(now)
	if _, err := os.Stat(filepath.Join(s.backupDir, old)); err != nil {
		t.Errorf("with no retention, %s should be kept: %v", old, err)
	}
}

func assertGzipContent(t *testing.T, path string, want []byte) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	gr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip reader %s: %v", path, err)
	}
	defer func() { _ = gr.Close() }()
	got, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("read gzip %s: %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s decompressed content mismatch", path)
	}
}
