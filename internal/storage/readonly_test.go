package storage

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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
	for _, readerBytes := range []int64{1024 * 1024, 32 * 1024 * 1024, 256 * 1024} {
		t.Run(fmt.Sprintf("configured %d bytes", readerBytes), func(t *testing.T) {
			testReadOnlyStoreNeverWrites(t, readerBytes)
		})
	}
}

// testReadOnlyStoreNeverWrites reads a tier the owner wrote at 1 MiB through
// a config naming readerBytes: an owner would grow a tier to a larger size on
// open, but a read-only store must leave the file alone either way.
func testReadOnlyStoreNeverWrites(t *testing.T, readerBytes int64) {
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

	reader, err := OpenReadOnly(readOnlyTestConfig(cfg.Directory, readerBytes))
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

// A legacy JSON (v1) tier is read as it is: an owner migrates it to v2 on
// open, a read-only store must not.
func TestReadOnlyStoreReadsLegacyTierWithoutMigrating(t *testing.T) {
	cfg := readOnlyTestConfig(t.TempDir(), 64*1024)
	path := filepath.Join(cfg.Directory, "tier_0.dat")
	start := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	var samples []*AggregatedSample
	for i := 0; i < 5; i++ {
		ts := start.Add(time.Duration(i) * time.Second)
		samples = append(samples, &AggregatedSample{Timestamp: ts, Duration: time.Second, Data: makeSampleWithCPU(ts, float64(10*i))})
	}
	writeLegacyJSONTier(t, path, cfg.Tiers[0].MaxBytes-headerSize, samples...)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	reader, err := OpenReadOnly(cfg)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	got, err := reader.tiers[0].ReadRange(start, start.Add(time.Minute))
	if err != nil {
		t.Fatalf("ReadRange: %v", err)
	}
	if len(got) != 5 || got[4].Data.CPU.Total.Usage != 40 {
		t.Fatalf("read %d legacy samples (want 5, last usage 40)", len(got))
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("read-only store rewrote a legacy tier")
	}
	if matches, _ := filepath.Glob(filepath.Join(cfg.Directory, "*")); len(matches) != 1 {
		t.Fatalf("read-only store left files behind: %v", matches)
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

// The owner keeps writing, from its own Store as another process would,
// while a read-only store reads the whole ring: every sample returned must be
// one the owner wrote, with the value written for its timestamp and in order.
// A scan the owner overtakes is retried or reported, never returned torn.
func TestReadOnlyStoreReadsWhileOwnerWrites(t *testing.T) {
	cfg := readOnlyTestConfig(t.TempDir(), 32*1024)
	owner, err := NewStore(cfg)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer func() { _ = owner.Close() }()
	start := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	usage := func(i int) float64 { return float64(i % 97) }
	write := func(i int) {
		if err := owner.WriteSample(makeSampleWithCPU(start.Add(time.Duration(i)*time.Second), usage(i))); err != nil {
			t.Errorf("WriteSample(%d): %v", i, err)
		}
	}
	next := 0
	for ; next < 1000; next++ {
		write(next) // wraps the ring several times
	}
	if !owner.tiers[0].wrapped {
		t.Fatal("test ring never wrapped; shrink the tier or write more")
	}

	reader, err := OpenReadOnly(cfg)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer func() { _ = reader.Close() }()

	// Bursts overwrite the oldest records under running scans; the pauses
	// let scans complete, even under the race detector.
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			for burst := 0; burst < 20; burst++ {
				write(next)
				next++
			}
			select {
			case <-stop:
				return
			case <-time.After(15 * time.Millisecond):
			}
		}
	}()

	verify := func(samples []*AggregatedSample) {
		t.Helper()
		for i, sample := range samples {
			n := int(sample.Timestamp.Sub(start) / time.Second)
			if sample.Data == nil || sample.Data.CPU.Total.Usage != usage(n) {
				t.Fatalf("sample at %s carries a value the owner never wrote there", sample.Timestamp)
			}
			if i > 0 && !sample.Timestamp.After(samples[i-1].Timestamp) {
				t.Fatalf("sample at %s follows %s", sample.Timestamp, samples[i-1].Timestamp)
			}
		}
	}
	var completed, abandoned int
	for deadline := time.Now().Add(1500 * time.Millisecond); time.Now().Before(deadline); {
		if err := reader.Refresh(); err != nil {
			t.Fatalf("Refresh: %v", err)
		}
		// The raw tier scan, unretried: abandoning it is fine, tearing is not.
		raw, err := reader.tiers[0].ReadRange(start, start.Add(24*time.Hour))
		switch {
		case errors.Is(err, errHistorySnapshotExpired):
			abandoned++
		case err != nil:
			t.Fatalf("ReadRange: %v", err)
		default:
			verify(raw)
			completed++
		}
		// The history query over the whole retained ring at native resolution,
		// through the batched scanner and the store's retries.
		ranges, _ := reader.RetainedRanges()
		if len(ranges) == 0 {
			continue
		}
		result, err := reader.QueryRangeWithMeta(ranges[0].From, ranges[0].To, 7200)
		switch {
		case errors.Is(err, errHistorySnapshotExpired):
			abandoned++
		case err != nil:
			t.Fatalf("QueryRangeWithMeta: %v", err)
		default:
			verify(result.Samples)
			completed++
		}
	}
	close(stop)
	<-done
	t.Logf("%d reads completed, %d abandoned while the owner wrote", completed, abandoned)
	if completed == 0 {
		t.Fatal("no read completed while the owner wrote")
	}
}

func TestReadOnlyStoreReportsLayoutMismatch(t *testing.T) {
	cfg := readOnlyTestConfig(t.TempDir(), 1024*1024)
	owner, err := NewStore(cfg)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	writeCPUSamples(t, owner, time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC), 3, 10)
	_ = owner.Close()

	open := func(cfg config.StorageConfig) string {
		reader, err := OpenReadOnly(cfg)
		if err != nil {
			t.Fatalf("OpenReadOnly: %v", err)
		}
		defer func() { _ = reader.Close() }()
		return reader.LayoutMismatch()
	}
	if note := open(cfg); note != "" {
		t.Fatalf("matching config reported %q", note)
	}
	if note := open(readOnlyTestConfig(cfg.Directory, 2*1024*1024)); !strings.Contains(note, "tier 0 is 1 MiB on disk but 2 MiB") {
		t.Fatalf("size mismatch reported %q", note)
	}
	none := readOnlyTestConfig(cfg.Directory, 1024*1024)
	none.Tiers = nil
	if note := open(none); !strings.Contains(note, "more tiers than the 0 configured") {
		t.Fatalf("extra tier file reported %q", note)
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
