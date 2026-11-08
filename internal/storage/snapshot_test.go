package storage

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"kula/internal/config"
)

// snapshotToFile snapshots tier into a new file and returns its path, or an
// error (it may run off the test goroutine).
func snapshotToFile(dir string, tier *Tier) (string, error) {
	path := filepath.Join(dir, "tier_copy.dat")
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	n, err := tier.SnapshotTo(f)
	if err != nil {
		return "", err
	}
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if info.Size() != n {
		return "", fmt.Errorf("snapshot reported %d bytes but wrote %d", n, info.Size())
	}
	return path, nil
}

func snapshotTimestamps(t *testing.T, tier *Tier, from, to time.Time) []time.Time {
	t.Helper()
	samples, err := tier.ReadRange(from, to)
	if err != nil {
		t.Fatalf("ReadRange: %v", err)
	}
	stamps := make([]time.Time, len(samples))
	for i, s := range samples {
		stamps[i] = s.Timestamp
	}
	return stamps
}

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

	dst := filepath.Join(t.TempDir(), "tier_copy.dat")
	f, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	written, err := store.SnapshotTier(0, f)
	if err != nil {
		t.Fatalf("SnapshotTier: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(dst); err != nil || written != info.Size() || written < headerSize {
		t.Fatalf("snapshot reported %d bytes, file %v (%v)", written, info, err)
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
	f, err := os.Create(filepath.Join(t.TempDir(), "tier_copy.dat"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := store.SnapshotTier(5, f); err == nil {
		t.Error("expected error for out-of-range tier index")
	}
	if _, err := store.SnapshotTier(-1, f); err == nil {
		t.Error("expected error for negative tier index")
	}
}

// TestSnapshotCopiesWritesMadeDuringTheCopy writes records between
// SnapshotTo's unlocked copy and its patch, in every way the writer can move
// meanwhile: appending, wrapping (from a fresh ring, whose file also grows,
// and from a wrapped one), and lapping the ring. The snapshot must equal the
// tier file byte for byte when SnapshotTo returns.
func TestSnapshotCopiesWritesMadeDuringTheCopy(t *testing.T) {
	const ringSize = 16 * 1024
	base := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		wrapped bool  // the ring has wrapped before the snapshot starts
		passes  int64 // ring passes the writer completes during the copy
	}{
		{"appends to a fresh ring", false, 0},
		{"appends to a wrapped ring", true, 0},
		{"wraps a fresh ring", false, 1},
		{"wraps a wrapped ring", true, 1},
		{"laps the ring", true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "tier_0.dat")
			tier, err := OpenTier(path, ringSize)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tier.Close() }()
			next := base
			write := func() {
				t.Helper()
				if err := tier.Write(varSample(next, 2)); err != nil {
					t.Fatal(err)
				}
				next = next.Add(time.Second)
			}
			for tier.count < 10 || (tc.wrapped && tier.writeCycle == 0) {
				write()
			}
			if tc.wrapped {
				for i := 0; i < 5; i++ {
					write()
				}
			}

			passes := int64(-1)
			tier.snapshotCopied = func() {
				// Writing on this goroutine would deadlock on a held lock.
				if !tier.mu.TryLock() {
					t.Error("SnapshotTo holds the tier lock while it copies")
					return
				}
				tier.mu.Unlock()
				start := tier.writeCycle
				for tier.writeCycle-start < tc.passes {
					write()
				}
				for i := 0; i < 3; i++ {
					write()
				}
				passes = tier.writeCycle - start
			}
			copyPath, err := snapshotToFile(t.TempDir(), tier)
			if err != nil {
				t.Fatalf("SnapshotTo: %v", err)
			}
			if passes != tc.passes {
				t.Fatalf("the writer completed %d ring passes during the copy, want %d", passes, tc.passes)
			}

			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(copyPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				at := 0
				for at < min(len(got), len(want)) && got[at] == want[at] {
					at++
				}
				t.Fatalf("snapshot (%d bytes) differs from the tier file (%d bytes) from byte %d", len(got), len(want), at)
			}

			copyTier, err := OpenTier(copyPath, ringSize)
			if err != nil {
				t.Fatalf("OpenTier on snapshot: %v", err)
			}
			defer func() { _ = copyTier.Close() }()
			if got, want := snapshotTimestamps(t, copyTier, base, next), snapshotTimestamps(t, tier, base, next); fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("snapshot reads %d records, the tier %d", len(got), len(want))
			}
		})
	}
}

// TestSnapshotLetsWritesThrough is S4 of the 0.21.1 review: a backup copied
// each tier under its read lock, so Write — and the collection loop, the
// store lock and every query behind it — waited for the whole copy.
func TestSnapshotLetsWritesThrough(t *testing.T) {
	tier, err := OpenTier(filepath.Join(t.TempDir(), "tier_0.dat"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	const n = 10
	for i := 0; i < n; i++ {
		if err := tier.Write(varSample(base.Add(time.Duration(i)*time.Second), 2)); err != nil {
			t.Fatal(err)
		}
	}

	blocked := errors.New("Write blocked while the snapshot copied the file")
	tier.snapshotCopied = func() {
		wrote := make(chan error, 1)
		go func() { wrote <- tier.Write(varSample(base.Add(n*time.Second), 2)) }()
		select {
		case err := <-wrote:
			if err != nil {
				t.Errorf("Write during the snapshot: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error(blocked)
		}
	}
	type result struct {
		path string
		err  error
	}
	done := make(chan result, 1)
	dir := t.TempDir()
	go func() {
		path, err := snapshotToFile(dir, tier)
		done <- result{path, err}
	}()
	var res result
	select {
	case res = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("SnapshotTo did not return")
	}
	// Close only once nothing waits on the lock any more.
	defer func() { _ = tier.Close() }()
	if res.err != nil {
		t.Fatalf("SnapshotTo: %v", res.err)
	}
	copyTier, err := OpenTier(res.path, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = copyTier.Close() }()
	if got := copyTier.Count(); got != n+1 {
		t.Fatalf("snapshot holds %d records, want %d including the one written during the copy", got, n+1)
	}
}

// TestSnapshotWhileTheStoreWrites takes snapshots while a store writes as fast
// as it can, lapping a small ring many times over (run with -race). Each
// snapshot must open as a tier holding one unbroken run of records.
func TestSnapshotWhileTheStoreWrites(t *testing.T) {
	const ringSize = 256 * 1024
	store, err := NewStore(config.StorageConfig{
		Directory: t.TempDir(),
		Tiers:     []config.TierConfig{{Resolution: time.Second, MaxSize: "256KB", MaxBytes: ringSize}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	base := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if err := store.WriteSample(makeSample(base.Add(time.Duration(i) * time.Second))); err != nil {
				t.Errorf("WriteSample(%d): %v", i, err)
				return
			}
		}
	}()
	defer func() {
		close(stop)
		wg.Wait()
	}()

	dir := t.TempDir()
	for k := 0; k < 30; k++ {
		path := filepath.Join(dir, fmt.Sprintf("snapshot_%d.dat", k))
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		_, err = store.SnapshotTier(0, f)
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			t.Fatalf("snapshot %d: %v", k, err)
		}
		copyTier, err := OpenTier(path, ringSize)
		if err != nil {
			t.Fatalf("snapshot %d does not open: %v", k, err)
		}
		if copyTier.Count() > 0 {
			fullReadChecked(t, copyTier, base, copyTier.NewestTimestamp())
		}
		_ = copyTier.Close()
	}
}
