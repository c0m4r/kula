package storage

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestSnapshotTierRoundTrip writes samples, snapshots tier 0 to a new file, and
// verifies the copy reopens as a valid tier with the same records.
func TestSnapshotTierRoundTrip(t *testing.T) {
	store := newTestStore(t)
	defer func() { _ = store.Close() }()

	base := time.Now().Truncate(time.Second)
	const n = 50
	for i := 0; i < n; i++ {
		if err := store.WriteSample(makeSample(base.Add(time.Duration(i) * time.Second))); err != nil {
			t.Fatalf("WriteSample: %v", err)
		}
	}

	if got := store.TierCount(); got != 1 {
		t.Fatalf("TierCount = %d, want 1", got)
	}

	var buf bytes.Buffer
	written, err := store.SnapshotTier(0, &buf)
	if err != nil {
		t.Fatalf("SnapshotTier: %v", err)
	}
	if written != int64(buf.Len()) || written < headerSize {
		t.Fatalf("snapshot wrote %d bytes (buffer %d)", written, buf.Len())
	}

	// Write the snapshot out and reopen it as a tier.
	dst := filepath.Join(t.TempDir(), "tier_copy.dat")
	if err := os.WriteFile(dst, buf.Bytes(), 0600); err != nil {
		t.Fatalf("write copy: %v", err)
	}
	copyTier, err := OpenTier(dst, 10*1024*1024)
	if err != nil {
		t.Fatalf("OpenTier on snapshot: %v", err)
	}
	defer func() { _ = copyTier.Close() }()

	if copyTier.Count() != uint64(n) {
		t.Errorf("snapshot record count = %d, want %d", copyTier.Count(), n)
	}
	samples, err := copyTier.ReadRange(base, base.Add(n*time.Second))
	if err != nil {
		t.Fatalf("ReadRange on snapshot: %v", err)
	}
	if len(samples) != n {
		t.Errorf("snapshot ReadRange returned %d samples, want %d", len(samples), n)
	}
}

func TestSnapshotTierOutOfRange(t *testing.T) {
	store := newTestStore(t)
	defer func() { _ = store.Close() }()
	var buf bytes.Buffer
	if _, err := store.SnapshotTier(5, &buf); err == nil {
		t.Error("expected error for out-of-range tier index")
	}
	if _, err := store.SnapshotTier(-1, &buf); err == nil {
		t.Error("expected error for negative tier index")
	}
}
