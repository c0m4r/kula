package storage

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"kula/internal/config"
)

func readOnlyTestConfig(dir string, maxBytes int64) config.StorageConfig {
	return config.StorageConfig{
		Directory: dir,
		Tiers: []config.TierConfig{
			{Resolution: time.Second, MaxBytes: maxBytes},
		},
	}
}

func writeCPUSamples(t *testing.T, store *Store, start time.Time, count int, usage float64) {
	t.Helper()
	for i := 0; i < count; i++ {
		if err := store.WriteSample(makeSampleWithCPU(start.Add(time.Duration(i)*time.Second), usage)); err != nil {
			t.Fatalf("WriteSample(%d): %v", i, err)
		}
	}
}

func queryAll(t *testing.T, store *Store, from, to time.Time) []*AggregatedSample {
	t.Helper()
	result, err := store.QueryRangeWithMeta(from, to, 7200)
	if err != nil {
		t.Fatalf("QueryRangeWithMeta: %v", err)
	}
	return result.Samples
}

func TestReadOnlyStoreFollowsOwningWriter(t *testing.T) {
	cfg := readOnlyTestConfig(t.TempDir(), 1024*1024)
	owner, err := NewStore(cfg)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer func() { _ = owner.Close() }()

	start := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	writeCPUSamples(t, owner, start, 30, 25)

	reader, err := OpenReadOnly(cfg)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer func() { _ = reader.Close() }()
	if !reader.ReadOnly() || owner.ReadOnly() {
		t.Fatalf("ReadOnly() = reader %v, owner %v; want true, false", reader.ReadOnly(), owner.ReadOnly())
	}

	end := start.Add(2 * time.Minute)
	if got := queryAll(t, reader, start, end); len(got) != 30 {
		t.Fatalf("reader saw %d samples before new writes, want 30", len(got))
	}

	writeCPUSamples(t, owner, start.Add(30*time.Second), 30, 75)
	if err := reader.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	samples := queryAll(t, reader, start, end)
	if len(samples) != 60 {
		t.Fatalf("reader saw %d samples after refresh, want 60", len(samples))
	}
	if got := samples[len(samples)-1].Data.CPU.Total.Usage; got != 75 {
		t.Fatalf("newest CPU usage = %v, want 75", got)
	}
	ranges, _ := reader.RetainedRanges()
	if len(ranges) != 1 || !ranges[0].To.Equal(start.Add(59*time.Second)) {
		t.Fatalf("RetainedRanges() = %+v, want newest %s", ranges, start.Add(59*time.Second))
	}
}

func TestReadOnlyStoreNeverWrites(t *testing.T) {
	cfg := readOnlyTestConfig(t.TempDir(), 1024*1024)
	owner, err := NewStore(cfg)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	start := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	writeCPUSamples(t, owner, start, 10, 40)
	if err := owner.Close(); err != nil {
		t.Fatalf("owner Close: %v", err)
	}

	path := filepath.Join(cfg.Directory, "tier_0.dat")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	reader, err := OpenReadOnly(cfg)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	if got := queryAll(t, reader, start, start.Add(time.Minute)); len(got) != 10 {
		t.Fatalf("reader saw %d samples, want 10", len(got))
	}
	if err := reader.WriteSample(makeSample(start.Add(time.Hour))); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("WriteSample error = %v, want ErrReadOnly", err)
	}
	if err := reader.tiers[0].Write(&AggregatedSample{Timestamp: start, Data: makeSample(start)}); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("Tier.Write error = %v, want ErrReadOnly", err)
	}
	if err := reader.tiers[0].Flush(); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("Tier.Flush error = %v, want ErrReadOnly", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("reader Close: %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("read-only store modified the tier file")
	}
}

func TestReadOnlyStoreMissingDirectoryIsEmptyAndCreatesNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "not-yet")
	cfg := readOnlyTestConfig(dir, 1024*1024)

	reader, err := OpenReadOnly(cfg)
	if err != nil {
		t.Fatalf("OpenReadOnly on a missing directory: %v", err)
	}
	defer func() { _ = reader.Close() }()

	start := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	if got := queryAll(t, reader, start, start.Add(time.Minute)); len(got) != 0 {
		t.Fatalf("empty reader returned %d samples", len(got))
	}
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("read-only open created %s (stat error %v)", dir, err)
	}

	// The owner may start after the viewer; a refresh adopts its new files.
	owner, err := NewStore(cfg)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer func() { _ = owner.Close() }()
	writeCPUSamples(t, owner, start, 5, 10)
	if err := reader.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := queryAll(t, reader, start, start.Add(time.Minute)); len(got) != 5 {
		t.Fatalf("reader saw %d samples after the owner started, want 5", len(got))
	}
}

func TestReadOnlyStoreReopensReplacedTier(t *testing.T) {
	cfg := readOnlyTestConfig(t.TempDir(), 1024*1024)
	first, err := NewStore(cfg)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	start := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	writeCPUSamples(t, first, start, 10, 10)

	reader, err := OpenReadOnly(cfg)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer func() { _ = reader.Close() }()

	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	path := filepath.Join(cfg.Directory, "tier_0.dat")
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	second, err := NewStore(cfg)
	if err != nil {
		t.Fatalf("NewStore after replacing the tier: %v", err)
	}
	defer func() { _ = second.Close() }()
	later := start.Add(time.Hour)
	writeCPUSamples(t, second, later, 3, 90)

	if err := reader.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	samples := queryAll(t, reader, start, later.Add(time.Minute))
	if len(samples) != 3 {
		t.Fatalf("reader saw %d samples, want the replacement's 3", len(samples))
	}
	for _, sample := range samples {
		if sample.Data.CPU.Total.Usage != 90 {
			t.Fatalf("reader returned a sample from the replaced file: %+v", sample.Data.CPU.Total)
		}
	}
}

func TestReadOnlyStoreFollowsWrappingRing(t *testing.T) {
	// A small ring wraps many times; every refresh must describe exactly the
	// records the owner retains.
	cfg := readOnlyTestConfig(t.TempDir(), 8*1024)
	owner, err := NewStore(cfg)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer func() { _ = owner.Close() }()
	reader, err := OpenReadOnly(cfg)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer func() { _ = reader.Close() }()

	start := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	from, to := start.Add(-time.Minute), start.Add(24*time.Hour)
	next := start
	for round := 0; round < 12; round++ {
		writeCPUSamples(t, owner, next, 37, float64(round))
		next = next.Add(37 * time.Second)
		if err := reader.Refresh(); err != nil {
			t.Fatalf("round %d Refresh: %v", round, err)
		}

		want := queryAll(t, owner, from, to)
		got := queryAll(t, reader, from, to)
		if len(got) != len(want) {
			t.Fatalf("round %d: reader saw %d samples, owner %d", round, len(got), len(want))
		}
		for i := range want {
			if !got[i].Timestamp.Equal(want[i].Timestamp) ||
				got[i].Data.CPU.Total.Usage != want[i].Data.CPU.Total.Usage {
				t.Fatalf("round %d sample %d: reader %s/%v, owner %s/%v", round, i,
					got[i].Timestamp, got[i].Data.CPU.Total.Usage,
					want[i].Timestamp, want[i].Data.CPU.Total.Usage)
			}
		}
	}
	if !owner.tiers[0].wrapped && owner.tiers[0].writeCycle == 0 {
		t.Fatal("test ring never wrapped; shrink the tier or write more rounds")
	}
}

func TestReadOnlyStoreReportsUnreadableTier(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions")
	}
	cfg := readOnlyTestConfig(t.TempDir(), 1024*1024)
	owner, err := NewStore(cfg)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	writeCPUSamples(t, owner, time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC), 3, 10)
	if err := owner.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	path := filepath.Join(cfg.Directory, "tier_0.dat")
	if err := os.Chmod(path, 0); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	defer func() { _ = os.Chmod(path, 0o600) }()

	if _, err := OpenReadOnly(cfg); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("OpenReadOnly error = %v, want a permission error", err)
	}
}
