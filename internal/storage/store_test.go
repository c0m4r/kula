package storage

import (
	"kula/internal/collector"
	"kula/internal/config"
	"os"
	"sync"
	"testing"
	"time"
)

// ---- Store helpers ----------------------------------------------------------

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	cfg := config.StorageConfig{
		Directory: dir,
		Tiers: []config.TierConfig{
			{Resolution: time.Second, MaxSize: "10MB", MaxBytes: 10 * 1024 * 1024},
		},
	}
	store, err := NewStore(cfg)
	if err != nil {
		t.Fatalf("NewStore() error: %v", err)
	}
	return store
}

func newMultiTierStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	cfg := config.StorageConfig{
		Directory: dir,
		Tiers: []config.TierConfig{
			{Resolution: time.Second, MaxSize: "10MB", MaxBytes: 10 * 1024 * 1024},
			{Resolution: time.Minute, MaxSize: "10MB", MaxBytes: 10 * 1024 * 1024},
			{Resolution: 5 * time.Minute, MaxSize: "10MB", MaxBytes: 10 * 1024 * 1024},
		},
	}
	store, err := NewStore(cfg)
	if err != nil {
		t.Fatalf("NewStore() error: %v", err)
	}
	return store
}

func makeSample(ts time.Time) *collector.Sample {
	return &collector.Sample{
		Timestamp: ts,
		CPU: collector.CPUStats{
			Total: collector.CPUCoreStats{Usage: 42.0},
		},
		LoadAvg: collector.LoadAvg{Load1: 1.0, Load5: 0.8, Load15: 0.5},
		Memory:  collector.MemoryStats{Total: 1024, Used: 512},
		System:  collector.SystemStats{Hostname: "test"},
	}
}

func makeSampleWithCPU(ts time.Time, usage float64) *collector.Sample {
	s := makeSample(ts)
	s.CPU.Total.Usage = usage
	return s
}

// ---- Basic CRUD -------------------------------------------------------------

func TestNewStore(t *testing.T) {
	store := newTestStore(t)
	defer func() { _ = store.Close() }()

	if len(store.tiers) != 1 {
		t.Errorf("Tier count = %d, want 1", len(store.tiers))
	}
}

func TestNewStoreInvalidDirectory(t *testing.T) {
	cfg := config.StorageConfig{
		Directory: "/proc/nonexistent/kula_test_dir_that_cannot_be_created",
		Tiers: []config.TierConfig{
			{Resolution: time.Second, MaxSize: "1MB", MaxBytes: 1024 * 1024},
		},
	}
	_, err := NewStore(cfg)
	if err == nil {
		t.Error("NewStore() with unwritable directory should return error")
	}
}

func TestWriteAndQuerySample(t *testing.T) {
	store := newTestStore(t)
	defer func() { _ = store.Close() }()

	now := time.Now()
	if err := store.WriteSample(makeSample(now)); err != nil {
		t.Fatalf("WriteSample() error: %v", err)
	}

	from := now.Add(-time.Minute)
	to := now.Add(time.Minute)
	results, err := store.QueryRange(from, to)
	if err != nil {
		t.Fatalf("QueryRange() error: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("QueryRange() returned no results")
	}
	if results[0].Data.CPU.Total.Usage != 42.0 {
		t.Errorf("CPU Usage = %f, want 42.0", results[0].Data.CPU.Total.Usage)
	}
}

func TestWriteMultipleSamples(t *testing.T) {
	store := newTestStore(t)
	defer func() { _ = store.Close() }()

	base := time.Now()
	for i := 0; i < 10; i++ {
		ts := base.Add(time.Duration(i) * time.Second)
		if err := store.WriteSample(makeSample(ts)); err != nil {
			t.Fatalf("WriteSample(%d) error: %v", i, err)
		}
	}

	results, err := store.QueryRange(base.Add(-time.Second), base.Add(11*time.Second))
	if err != nil {
		t.Fatalf("QueryRange() error: %v", err)
	}
	if len(results) != 10 {
		t.Errorf("QueryRange() returned %d results, want 10", len(results))
	}
}

// ---- QueryLatest ------------------------------------------------------------

func TestQueryLatest(t *testing.T) {
	store := newTestStore(t)
	defer func() { _ = store.Close() }()

	now := time.Now()
	if err := store.WriteSample(makeSample(now)); err != nil {
		t.Fatalf("WriteSample() error: %v", err)
	}
	if err := store.WriteSample(makeSample(now.Add(time.Second))); err != nil {
		t.Fatalf("WriteSample() error: %v", err)
	}

	latest, err := store.QueryLatest()
	if err != nil {
		t.Fatalf("QueryLatest() error: %v", err)
	}
	if latest == nil {
		t.Fatal("QueryLatest() returned nil")
	}
}

func TestQueryLatestEmptyStore(t *testing.T) {
	store := newTestStore(t)
	defer func() { _ = store.Close() }()

	latest, err := store.QueryLatest()
	if err != nil {
		t.Fatalf("QueryLatest() on empty store error: %v", err)
	}
	// Should return nil, not an error
	if latest != nil {
		t.Errorf("QueryLatest() on empty store returned non-nil: %+v", latest)
	}
}

func TestQueryLatestUsesCache(t *testing.T) {
	// Verifies that after WriteSample the latestCache is set correctly
	// and QueryLatest returns that sample without a disk scan.
	store := newTestStore(t)
	defer func() { _ = store.Close() }()

	now := time.Now().Truncate(time.Second)
	sample := makeSampleWithCPU(now, 77.7)
	if err := store.WriteSample(sample); err != nil {
		t.Fatalf("WriteSample: %v", err)
	}

	last, err := store.QueryLatest()
	if err != nil {
		t.Fatalf("QueryLatest: %v", err)
	}
	if last == nil {
		t.Fatal("QueryLatest returned nil after WriteSample")
	}
	if last.Data.CPU.Total.Usage != 77.7 {
		t.Errorf("QueryLatest CPU = %f, want 77.7", last.Data.CPU.Total.Usage)
	}

	// Write a second sample and verify the cache advances
	sample2 := makeSampleWithCPU(now.Add(time.Second), 88.8)
	if err := store.WriteSample(sample2); err != nil {
		t.Fatalf("WriteSample 2: %v", err)
	}
	last2, _ := store.QueryLatest()
	if last2 == nil || last2.Data.CPU.Total.Usage != 88.8 {
		t.Errorf("QueryLatest after second write: CPU = %v, want 88.8", last2)
	}
}

func TestQueryLatestAfterRestartUsesWarmCache(t *testing.T) {
	// Simulates a process restart: write samples, close the store,
	// reopen it, and verify warmLatestCache restores the latest sample
	// without an explicit WriteSample call.
	dir := t.TempDir()
	cfg := config.StorageConfig{
		Directory: dir,
		Tiers:     []config.TierConfig{{Resolution: time.Second, MaxSize: "10MB", MaxBytes: 10 * 1024 * 1024}},
	}

	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	// First run — write and close
	{
		store1, err := NewStore(cfg)
		if err != nil {
			t.Fatalf("NewStore (first): %v", err)
		}
		for i := 0; i < 5; i++ {
			_ = store1.WriteSample(makeSampleWithCPU(base.Add(time.Duration(i)*time.Second), float64(i*10)))
		}
		_ = store1.Close()
	}

	// Second run — reopen, no WriteSample; warmLatestCache should restore data
	store2, err := NewStore(cfg)
	if err != nil {
		t.Fatalf("NewStore (second): %v", err)
	}
	defer func() { _ = store2.Close() }()

	latest, err := store2.QueryLatest()
	if err != nil {
		t.Fatalf("QueryLatest after restart: %v", err)
	}
	if latest == nil {
		t.Fatal("QueryLatest after restart returned nil — warmLatestCache did not fire")
	}
	// The last written sample had CPU = 40.0 (i=4)
	if latest.Data.CPU.Total.Usage != 40.0 {
		t.Errorf("After restart QueryLatest CPU = %f, want 40.0", latest.Data.CPU.Total.Usage)
	}
}

// ---- QueryRange edge cases --------------------------------------------------

func TestQueryRangeEmpty(t *testing.T) {
	store := newTestStore(t)
	defer func() { _ = store.Close() }()

	results, err := store.QueryRange(time.Now(), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("QueryRange() error: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("Empty store should return 0 results, got %d", len(results))
	}
}

func TestQueryRangeOutOfBounds(t *testing.T) {
	store := newTestStore(t)
	defer func() { _ = store.Close() }()

	// Write a sample at now
	now := time.Now()
	if err := store.WriteSample(makeSample(now)); err != nil {
		t.Fatalf("WriteSample() error: %v", err)
	}

	// Query in the distant future
	results, err := store.QueryRange(now.Add(time.Hour), now.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("QueryRange() error: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("Future query should return 0 results, got %d", len(results))
	}
}

func TestQueryRangePreservesChronologicalOrder(t *testing.T) {
	store := newTestStore(t)
	defer func() { _ = store.Close() }()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	n := 20
	for i := 0; i < n; i++ {
		ts := base.Add(time.Duration(i) * time.Second)
		if err := store.WriteSample(makeSampleWithCPU(ts, float64(i))); err != nil {
			t.Fatalf("WriteSample(%d): %v", i, err)
		}
	}

	results, err := store.QueryRange(base, base.Add(time.Duration(n)*time.Second))
	if err != nil {
		t.Fatalf("QueryRange: %v", err)
	}
	for i := 1; i < len(results); i++ {
		if results[i].Timestamp.Before(results[i-1].Timestamp) {
			t.Errorf("Results not in chronological order at index %d: %v < %v",
				i, results[i].Timestamp, results[i-1].Timestamp)
		}
	}
}

// ---- Ring buffer wrap -------------------------------------------------------

func TestRingBufferWrapRead(t *testing.T) {
	dir := t.TempDir()
	cfg := config.StorageConfig{
		Directory: dir,
		Tiers: []config.TierConfig{
			{Resolution: time.Second, MaxSize: "128KB", MaxBytes: 128 * 1024},
		},
	}
	store, err := NewStore(cfg)
	if err != nil {
		t.Fatalf("NewStore() error: %v", err)
	}
	defer func() { _ = store.Close() }()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	totalSamples := 500

	for i := 0; i < totalSamples; i++ {
		ts := base.Add(time.Duration(i) * time.Second)
		if err := store.WriteSample(makeSample(ts)); err != nil {
			t.Fatalf("WriteSample(%d) error: %v", i, err)
		}
	}

	queryFrom := base.Add(time.Duration(totalSamples-10) * time.Second)
	queryTo := base.Add(time.Duration(totalSamples) * time.Second)
	results, err := store.QueryRange(queryFrom, queryTo)
	if err != nil {
		t.Fatalf("QueryRange() error: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("QueryRange() returned 0 results after ring buffer wrap")
	}
	if len(results) < 8 {
		t.Errorf("QueryRange() returned only %d results, expected ~10 recent samples", len(results))
	}
	t.Logf("QueryRange() returned %d results (expected ~10)", len(results))

	for _, r := range results {
		if r.Timestamp.Before(queryFrom) || r.Timestamp.After(queryTo) {
			t.Errorf("Sample timestamp %v outside query range [%v, %v]",
				r.Timestamp, queryFrom, queryTo)
		}
	}
}

func TestRingBufferOldestNewestTimestamp(t *testing.T) {
	dir := t.TempDir()
	cfg := config.StorageConfig{
		Directory: dir,
		Tiers: []config.TierConfig{
			{Resolution: time.Second, MaxSize: "64KB", MaxBytes: 64 * 1024},
		},
	}
	store, err := NewStore(cfg)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer func() { _ = store.Close() }()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	n := 300 // should cause wrap in 64KB

	first := base
	var last time.Time
	for i := 0; i < n; i++ {
		last = base.Add(time.Duration(i) * time.Second)
		if err := store.WriteSample(makeSample(last)); err != nil {
			t.Fatalf("WriteSample(%d): %v", i, err)
		}
	}

	tier := store.tiers[0]
	newest := tier.NewestTimestamp()
	oldest := tier.OldestTimestamp()

	if newest.IsZero() {
		t.Error("NewestTimestamp() is zero")
	}
	if oldest.IsZero() {
		t.Error("OldestTimestamp() is zero")
	}
	if !newest.Equal(last) {
		t.Errorf("NewestTimestamp = %v, want %v", newest, last)
	}
	// After wrap, oldest must be after the absolute first
	if oldest.Before(first) {
		t.Errorf("OldestTimestamp %v is before first written %v — stale oldest after wrap", oldest, first)
	}
	if !oldest.Before(last) {
		t.Errorf("OldestTimestamp %v should be before last %v", oldest, last)
	}
	t.Logf("After %d samples in 64KB ring: oldest=%v newest=%v", n, oldest, newest)
}

// ---- InspectTierFile --------------------------------------------------------

func TestInspectTierFile(t *testing.T) {
	store := newTestStore(t)

	now := time.Now().Truncate(time.Millisecond)
	for i := 0; i < 5; i++ {
		_ = store.WriteSample(makeSample(now.Add(time.Duration(i) * time.Second)))
	}
	_ = store.Close()

	path := store.tiers[0].path
	info, err := InspectTierFile(path)
	if err != nil {
		t.Fatalf("InspectTierFile() error: %v", err)
	}
	if info.Count == 0 {
		t.Error("InspectTierFile() returned Count = 0")
	}
	if info.Version != codecVersion2 {
		t.Errorf("InspectTierFile() Version = %d, want %d", info.Version, codecVersion2)
	}
	if info.NewestTS.IsZero() {
		t.Error("InspectTierFile() NewestTS is zero")
	}
	if info.Wrapped {
		t.Errorf("InspectTierFile() Wrapped = true; want false for a tier with 5 samples in 10MB")
	}
}

// TestInspectTierFileWrapped verifies wrap detection after the ring buffer
// has cycled. Reproduces issue #24 where a full tier reported a tiny
// fullness percentage and Wrapped=false. The old heuristic compared the
// file's size against headerSize+maxData, which the file never quite
// reaches because the last record before a wrap typically leaves a gap.
func TestInspectTierFileWrapped(t *testing.T) {
	dir := t.TempDir()
	cfg := config.StorageConfig{
		Directory: dir,
		Tiers: []config.TierConfig{
			{Resolution: time.Second, MaxSize: "64KB", MaxBytes: 64 * 1024},
		},
	}
	store, err := NewStore(cfg)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Write enough samples to force the 64KB tier to wrap at least once.
	for i := 0; i < 300; i++ {
		if err := store.WriteSample(makeSample(base.Add(time.Duration(i) * time.Second))); err != nil {
			t.Fatalf("WriteSample(%d): %v", i, err)
		}
	}

	if !store.tiers[0].wrapped {
		t.Fatal("test precondition failed: tier did not wrap")
	}
	path := store.tiers[0].path
	_ = store.Close()

	info, err := InspectTierFile(path)
	if err != nil {
		t.Fatalf("InspectTierFile(): %v", err)
	}
	if !info.Wrapped {
		t.Errorf("InspectTierFile() Wrapped = false; want true for a tier that has cycled")
	}

	// Re-open the store and confirm the runtime path (readHeader) also
	// detects the wrap. Without this, after a process restart on a wrapped
	// tier, subsequent writes would not refresh oldestTS and queries
	// could be routed to a coarser tier than necessary.
	store2, err := NewStore(cfg)
	if err != nil {
		t.Fatalf("NewStore (reopen): %v", err)
	}
	defer func() { _ = store2.Close() }()
	if !store2.tiers[0].wrapped {
		t.Errorf("after reopen, tier.wrapped = false; want true")
	}
}

func TestInspectTierFileMissing(t *testing.T) {
	_, err := InspectTierFile("/nonexistent/path/tier.dat")
	if err == nil {
		t.Error("InspectTierFile() on missing file should error")
	}
}

func TestInspectTierFileCorrupted(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "corrupt_*.dat")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write([]byte("this is not a valid tier file"))
	_ = f.Close()

	_, err = InspectTierFile(f.Name())
	if err == nil {
		t.Error("InspectTierFile() on corrupted file should error")
	}
}

// ---- Tier selection (QueryRangeWithMeta) ------------------------------------

func TestQueryRangeWithMetaReturnsTierInfo(t *testing.T) {
	store := newTestStore(t)
	defer func() { _ = store.Close() }()

	now := time.Now()
	if err := store.WriteSample(makeSample(now)); err != nil {
		t.Fatalf("WriteSample: %v", err)
	}

	result, err := store.QueryRangeWithMeta(now.Add(-time.Minute), now.Add(time.Minute), 450)
	if err != nil {
		t.Fatalf("QueryRangeWithMeta: %v", err)
	}
	if result.Tier != 0 {
		t.Errorf("Tier = %d, want 0 (single-tier store)", result.Tier)
	}
	if result.Resolution == "" {
		t.Error("Resolution is empty")
	}
	if len(result.Samples) == 0 {
		t.Error("Samples is empty, expected at least one")
	}
}

func TestQueryRangeWithMetaEmptyStore(t *testing.T) {
	store := newTestStore(t)
	defer func() { _ = store.Close() }()

	result, err := store.QueryRangeWithMeta(
		time.Now().Add(-time.Minute),
		time.Now(),
		450,
	)
	if err != nil {
		t.Fatalf("QueryRangeWithMeta on empty store: %v", err)
	}
	if len(result.Samples) != 0 {
		t.Errorf("expected 0 samples, got %d", len(result.Samples))
	}
}

// ---- Multi-tier aggregation -------------------------------------------------

func TestMultiTierAggregation(t *testing.T) {
	store := newMultiTierStore(t)
	defer func() { _ = store.Close() }()

	// Write 60 consecutive 1-second samples — enough to trigger one tier-2 aggregation.
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 60; i++ {
		ts := base.Add(time.Duration(i) * time.Second)
		if err := store.WriteSample(makeSampleWithCPU(ts, float64(i))); err != nil {
			t.Fatalf("WriteSample(%d): %v", i, err)
		}
	}

	// Tier 2 should have exactly 1 aggregated sample after 60 tier-1 writes.
	tier2 := store.tiers[1]
	if tier2.Count() != 1 {
		t.Errorf("Tier 2 count = %d, want 1 after 60 tier-1 writes", tier2.Count())
	}

	// The aggregated CPU should be the average of 0..59 = 29.5.
	samples, err := tier2.ReadRange(base, base.Add(time.Minute))
	if err != nil {
		t.Fatalf("Tier 2 ReadRange: %v", err)
	}
	if len(samples) != 1 {
		t.Fatalf("Tier 2 returned %d samples, want 1", len(samples))
	}
	got := samples[0].Data.CPU.Total.Usage
	want := 29.5
	if got < want-0.5 || got > want+0.5 {
		t.Errorf("Aggregated CPU = %.2f, want ~%.2f", got, want)
	}
}

func TestMultiTierPeakPreservation(t *testing.T) {
	store := newMultiTierStore(t)
	defer func() { _ = store.Close() }()

	base := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	peakUsage := 99.0
	for i := 0; i < 60; i++ {
		ts := base.Add(time.Duration(i) * time.Second)
		usage := 10.0
		if i == 30 {
			usage = peakUsage // spike in the middle
		}
		if err := store.WriteSample(makeSampleWithCPU(ts, usage)); err != nil {
			t.Fatalf("WriteSample(%d): %v", i, err)
		}
	}

	samples, err := store.tiers[1].ReadRange(base, base.Add(time.Minute))
	if err != nil {
		t.Fatalf("Tier 2 ReadRange: %v", err)
	}
	if len(samples) == 0 {
		t.Fatal("No samples in tier 2")
	}
	agg := samples[0]
	if agg.Max == nil {
		t.Fatal("Max is nil in aggregated sample")
	}
	if agg.Max.CPU.Total.Usage < peakUsage-0.1 {
		t.Errorf("Max CPU = %.2f, want >= %.2f (spike should be preserved)", agg.Max.CPU.Total.Usage, peakUsage)
	}
}

// ---- fmtRes -----------------------------------------------------------------

func TestFmtRes(t *testing.T) {
	cases := []struct {
		dur  time.Duration
		want string
	}{
		{time.Second, "1s"},
		{30 * time.Second, "30s"},
		{time.Minute, "1m"},
		{5 * time.Minute, "5m"},
		{time.Hour, "1h"},
		{3 * time.Hour, "3h"},
		// Non-round seconds fall through to d.String()
		{500 * time.Millisecond, "500ms"},
		{90*time.Second + 500*time.Millisecond, "1m30.5s"},
	}
	for _, tc := range cases {
		got := fmtRes(tc.dur)
		if got != tc.want {
			t.Errorf("fmtRes(%v) = %q, want %q", tc.dur, got, tc.want)
		}
	}
}

// ---- Concurrent writes ------------------------------------------------------

func TestConcurrentWrites(t *testing.T) {
	store := newTestStore(t)
	defer func() { _ = store.Close() }()

	const goroutines = 8
	const writesPerGoroutine = 50

	base := time.Now()
	var wg sync.WaitGroup
	errCh := make(chan error, goroutines*writesPerGoroutine)

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			for i := 0; i < writesPerGoroutine; i++ {
				ts := base.Add(time.Duration(gid*1000+i) * time.Millisecond)
				if err := store.WriteSample(makeSample(ts)); err != nil {
					errCh <- err
				}
			}
		}(g)
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("Concurrent WriteSample error: %v", err)
	}

	results, err := store.QueryRange(base.Add(-time.Second), base.Add(10*time.Second))
	if err != nil {
		t.Fatalf("QueryRange error: %v", err)
	}
	t.Logf("Concurrent write test: %d/%d samples retrievable",
		len(results), goroutines*writesPerGoroutine)
}

// ============================================================================
// Benchmarks
// ============================================================================

// newBenchStore creates a temp store for benchmarks, failing the benchmark on error.
func newBenchStore(b *testing.B, size string, maxBytes int64) *Store {
	b.Helper()
	dir := b.TempDir()
	cfg := config.StorageConfig{
		Directory: dir,
		Tiers: []config.TierConfig{
			{Resolution: time.Second, MaxSize: size, MaxBytes: maxBytes},
		},
	}
	store, err := NewStore(cfg)
	if err != nil {
		b.Fatalf("NewStore: %v", err)
	}
	return store
}

// BenchmarkWrite measures sustained sequential write throughput.
func BenchmarkWrite(b *testing.B) {
	store := newBenchStore(b, "100MB", 100*1024*1024)
	defer func() { _ = store.Close() }()

	base := time.Now()
	sample := makeSample(base)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sample.Timestamp = base.Add(time.Duration(i) * time.Second)
		if err := store.WriteSample(sample); err != nil {
			b.Fatalf("WriteSample: %v", err)
		}
	}
	b.SetBytes(int64(b.N)) // samples/op isn't bytes, but useful for rate display
}

// BenchmarkWriteWrapping benchmarks writes into a small (wrapping) ring buffer.
func BenchmarkWriteWrapping(b *testing.B) {
	store := newBenchStore(b, "128KB", 128*1024)
	defer func() { _ = store.Close() }()

	base := time.Now()
	sample := makeSample(base)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sample.Timestamp = base.Add(time.Duration(i) * time.Second)
		if err := store.WriteSample(sample); err != nil {
			b.Fatalf("WriteSample: %v", err)
		}
	}
}

// BenchmarkWriteParallel measures concurrent write throughput and lock contention.
func BenchmarkWriteParallel(b *testing.B) {
	store := newBenchStore(b, "100MB", 100*1024*1024)
	defer func() { _ = store.Close() }()

	base := time.Now()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			s := makeSample(base.Add(time.Duration(i) * time.Millisecond))
			if err := store.WriteSample(s); err != nil {
				b.Errorf("WriteSample: %v", err)
			}
			i++
		}
	})
}

// seedStore writes n samples starting at base and returns the store.
func seedStore(b *testing.B, store *Store, n int) time.Time {
	b.Helper()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		ts := base.Add(time.Duration(i) * time.Second)
		if err := store.WriteSample(makeSample(ts)); err != nil {
			b.Fatalf("seed WriteSample(%d): %v", i, err)
		}
	}
	return base
}

// BenchmarkQueryRange_Small benchmarks a small read (last 60s of 300 samples).
func BenchmarkQueryRange_Small(b *testing.B) {
	store := newBenchStore(b, "50MB", 50*1024*1024)
	defer func() { _ = store.Close() }()

	base := seedStore(b, store, 300)
	from := base.Add(240 * time.Second)
	to := base.Add(300 * time.Second)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := store.QueryRange(from, to)
		if err != nil {
			b.Fatalf("QueryRange: %v", err)
		}
	}
}

// BenchmarkQueryRange_Large benchmarks a full-range read (all 3600 samples).
func BenchmarkQueryRange_Large(b *testing.B) {
	store := newBenchStore(b, "50MB", 50*1024*1024)
	defer func() { _ = store.Close() }()

	n := 3600
	base := seedStore(b, store, n)
	from := base
	to := base.Add(time.Duration(n) * time.Second)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		results, err := store.QueryRange(from, to)
		if err != nil {
			b.Fatalf("QueryRange: %v", err)
		}
		if len(results) == 0 {
			b.Fatal("QueryRange returned 0 results")
		}
	}
}

// BenchmarkQueryRange_Wrapped benchmarks reads after the ring buffer has wrapped.
func BenchmarkQueryRange_Wrapped(b *testing.B) {
	store := newBenchStore(b, "512KB", 512*1024)
	defer func() { _ = store.Close() }()

	// Write enough to wrap multiple times
	n := 2000
	base := seedStore(b, store, n)
	from := base.Add(time.Duration(n-60) * time.Second)
	to := base.Add(time.Duration(n) * time.Second)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := store.QueryRange(from, to)
		if err != nil {
			b.Fatalf("QueryRange: %v", err)
		}
	}
}

// BenchmarkQueryLatest_Cache benchmarks the warmed in-memory cache path.
// This is the steady-state hot path — called every second during live monitoring.
func BenchmarkQueryLatest_Cache(b *testing.B) {
	store := newBenchStore(b, "10MB", 10*1024*1024)
	defer func() { _ = store.Close() }()
	seedStore(b, store, 100) // ensures cache is populated

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := store.QueryLatest()
		if err != nil {
			b.Fatalf("QueryLatest: %v", err)
		}
	}
}

// BenchmarkQueryLatest_ColdDisk benchmarks the cold-start disk-scan path.
// This runs only once per process lifetime (warmLatestCache in NewStore).
// Kept to catch regressions in the full-scan fallback code path.
func BenchmarkQueryLatest_ColdDisk(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		store := newBenchStore(b, "10MB", 10*1024*1024)
		// Seed via tiers[0].Write directly to bypass the cache update in WriteSample
		base := time.Now()
		for j := 0; j < 100; j++ {
			ts := base.Add(time.Duration(j) * time.Second)
			as := &AggregatedSample{
				Timestamp: ts,
				Duration:  time.Second,
				Data:      makeSample(ts),
			}
			_ = store.tiers[0].Write(as)
		}
		// Nil the cache to simulate a cold read (before warmLatestCache)
		store.latestCache = nil
		b.StartTimer()
		_, _ = store.tiers[0].ReadLatest(1)
		b.StopTimer()
		_ = store.Close()
		b.StartTimer()
	}
}

// BenchmarkAggregateSamples benchmarks the aggregation path triggered by multi-tier writes.
func BenchmarkAggregateSamples(b *testing.B) {
	dir := b.TempDir()
	cfg := config.StorageConfig{
		Directory: dir,
		Tiers: []config.TierConfig{
			{Resolution: time.Second, MaxSize: "100MB", MaxBytes: 100 * 1024 * 1024},
			{Resolution: time.Minute, MaxSize: "100MB", MaxBytes: 100 * 1024 * 1024},
		},
	}
	store, err := NewStore(cfg)
	if err != nil {
		b.Fatalf("NewStore: %v", err)
	}
	defer func() { _ = store.Close() }()

	base := time.Now()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Every 60th write triggers aggregateSamples
		ts := base.Add(time.Duration(i) * time.Second)
		if err := store.WriteSample(makeSample(ts)); err != nil {
			b.Fatalf("WriteSample: %v", err)
		}
	}
}

// BenchmarkDownsampling benchmarks the inline downsampler in QueryRangeWithMeta
// that kicks in when a query returns >800 samples.
func BenchmarkDownsampling(b *testing.B) {
	store := newBenchStore(b, "100MB", 100*1024*1024)
	defer func() { _ = store.Close() }()

	// Seed with 3600 samples (1 hour at 1s res) — downsampling kicks in at >800.
	n := 3600
	base := seedStore(b, store, n)
	from := base
	to := base.Add(time.Duration(n) * time.Second)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		result, err := store.QueryRangeWithMeta(from, to, 450)
		if err != nil {
			b.Fatalf("QueryRangeWithMeta: %v", err)
		}
		if len(result.Samples) == 0 {
			b.Fatal("no samples returned")
		}
	}
}

// ============================================================================
// New feature tests
// ============================================================================

// ---- Query cache ------------------------------------------------------------

// TestQueryCacheHit verifies that a second identical query is served from the
// in-process cache and returns the same results.
func TestQueryCacheHit(t *testing.T) {
	store := newTestStore(t)
	defer func() { _ = store.Close() }()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		ts := base.Add(time.Duration(i) * time.Second)
		if err := store.WriteSample(makeSampleWithCPU(ts, float64(i*10))); err != nil {
			t.Fatalf("WriteSample(%d): %v", i, err)
		}
	}

	from := base.Add(-time.Second)
	to := base.Add(10 * time.Second)

	r1, err := store.QueryRangeWithMeta(from, to, 100)
	if err != nil {
		t.Fatalf("first QueryRangeWithMeta: %v", err)
	}
	if len(r1.Samples) == 0 {
		t.Fatal("first query returned 0 samples")
	}

	r2, err := store.QueryRangeWithMeta(from, to, 100)
	if err != nil {
		t.Fatalf("second QueryRangeWithMeta: %v", err)
	}
	if len(r1.Samples) != len(r2.Samples) {
		t.Errorf("cache hit returned different sample count: first=%d second=%d",
			len(r1.Samples), len(r2.Samples))
	}

	store.queryCacheMu.Lock()
	cacheSize := len(store.queryCache)
	store.queryCacheMu.Unlock()
	if cacheSize == 0 {
		t.Error("queryCache is empty after two identical queries — cache not populated")
	}
}

// TestQueryCacheInvalidatedOnWrite verifies that WriteSample evicts cache
// entries whose window reaches the live edge, so the next fetch reflects the
// newly written sample and stale live results are never returned.
func TestQueryCacheInvalidatedOnWrite(t *testing.T) {
	store := newTestStore(t)
	defer func() { _ = store.Close() }()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := store.WriteSample(makeSampleWithCPU(base, 10.0)); err != nil {
		t.Fatalf("WriteSample: %v", err)
	}

	from := base.Add(-time.Second)
	to := base.Add(3 * time.Second)

	// Populate the cache.
	r1, _ := store.QueryRangeWithMeta(from, to, 100)

	// A second write should clear the cache.
	if err := store.WriteSample(makeSampleWithCPU(base.Add(time.Second), 99.0)); err != nil {
		t.Fatalf("WriteSample 2: %v", err)
	}

	store.queryCacheMu.Lock()
	cacheSize := len(store.queryCache)
	store.queryCacheMu.Unlock()
	if cacheSize != 0 {
		t.Errorf("queryCache should be empty after WriteSample, got %d entries", cacheSize)
	}

	// Re-query: should now include the second sample.
	r2, err := store.QueryRangeWithMeta(from, to, 100)
	if err != nil {
		t.Fatalf("QueryRangeWithMeta after invalidation: %v", err)
	}
	if len(r2.Samples) <= len(r1.Samples) {
		t.Errorf("expected more samples after cache invalidation: before=%d after=%d",
			len(r1.Samples), len(r2.Samples))
	}
}

// TestQueryCacheRetainsPastWindowOnWrite verifies that a cached query for a
// window ending before the live edge survives a subsequent WriteSample — past
// windows are immutable, so they no longer need to be discarded on every tick.
func TestQueryCacheRetainsPastWindowOnWrite(t *testing.T) {
	store := newTestStore(t)
	defer func() { _ = store.Close() }()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		ts := base.Add(time.Duration(i) * time.Second)
		if err := store.WriteSample(makeSampleWithCPU(ts, float64(i*10))); err != nil {
			t.Fatalf("WriteSample(%d): %v", i, err)
		}
	}

	// Query a window that ends well before the newest sample (a past window).
	from := base
	to := base.Add(2 * time.Second)
	if _, err := store.QueryRangeWithMeta(from, to, 100); err != nil {
		t.Fatalf("QueryRangeWithMeta: %v", err)
	}

	store.queryCacheMu.Lock()
	populated := len(store.queryCache)
	store.queryCacheMu.Unlock()
	if populated == 0 {
		t.Fatal("expected past-window query to be cached")
	}

	// A later write at the live edge must not evict the past-window entry.
	if err := store.WriteSample(makeSampleWithCPU(base.Add(10*time.Second), 99.0)); err != nil {
		t.Fatalf("WriteSample (live edge): %v", err)
	}

	store.queryCacheMu.Lock()
	retained := len(store.queryCache)
	store.queryCacheMu.Unlock()
	if retained == 0 {
		t.Error("past-window cache entry should survive a write to the live edge")
	}
}

// ---- ts.After(to) early exit ------------------------------------------------

// TestReadRangeEarlyExitAfterWindow verifies that ReadRange returns only
// records within [from, to] and does not include records past 'to'.
func TestReadRangeEarlyExitAfterWindow(t *testing.T) {
	store := newTestStore(t)
	defer func() { _ = store.Close() }()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const n = 20
	for i := 0; i < n; i++ {
		ts := base.Add(time.Duration(i) * time.Second)
		if err := store.WriteSample(makeSampleWithCPU(ts, float64(i))); err != nil {
			t.Fatalf("WriteSample(%d): %v", i, err)
		}
	}

	from := base.Add(7 * time.Second)
	to := base.Add(12 * time.Second)

	results, err := store.tiers[0].ReadRange(from, to)
	if err != nil {
		t.Fatalf("ReadRange: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("ReadRange returned 0 results for mid-window query")
	}
	for _, r := range results {
		if r.Timestamp.Before(from) || r.Timestamp.After(to) {
			t.Errorf("result timestamp %v outside window [%v, %v]",
				r.Timestamp, from, to)
		}
	}
}

// ---- Timestamp propagation --------------------------------------------------

// TestBinaryTimestampPropagation verifies that collector.Sample.Timestamp is
// correctly set after binary encode→decode. This was the bug that caused the
// dashboard to display all historical points at year 1.
func TestBinaryTimestampPropagation(t *testing.T) {
	store := newTestStore(t)
	defer func() { _ = store.Close() }()

	want := time.Date(2026, 3, 19, 1, 0, 0, 0, time.UTC)
	if err := store.WriteSample(makeSample(want)); err != nil {
		t.Fatalf("WriteSample: %v", err)
	}

	results, err := store.tiers[0].ReadRange(want.Add(-time.Second), want.Add(time.Second))
	if err != nil {
		t.Fatalf("ReadRange: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("ReadRange returned no results")
	}

	got := results[0]
	if got.Timestamp.IsZero() {
		t.Error("AggregatedSample.Timestamp is zero after decode")
	}
	if !got.Timestamp.Equal(want) {
		t.Errorf("AggregatedSample.Timestamp = %v, want %v", got.Timestamp, want)
	}
	if got.Data == nil {
		t.Fatal("AggregatedSample.Data is nil")
	}
	// Key regression: collector.Sample.Timestamp must be propagated from the outer ts.
	if got.Data.Timestamp.IsZero() {
		t.Error("collector.Sample.Timestamp is zero after decode — this was the dashboard bug")
	}
	if !got.Data.Timestamp.Equal(want) {
		t.Errorf("collector.Sample.Timestamp = %v, want %v", got.Data.Timestamp, want)
	}
}

// ---- Ratio caching ----------------------------------------------------------

// TestRatioCachingMultiTier verifies that ratio1/ratio2 are computed once at
// NewStore and match the configured tier resolutions.
func TestRatioCachingMultiTier(t *testing.T) {
	dir := t.TempDir()
	cfg := config.StorageConfig{
		Directory: dir,
		Tiers: []config.TierConfig{
			{Resolution: time.Second, MaxSize: "10MB", MaxBytes: 10 * 1024 * 1024},
			{Resolution: time.Minute, MaxSize: "10MB", MaxBytes: 10 * 1024 * 1024},
			{Resolution: 5 * time.Minute, MaxSize: "10MB", MaxBytes: 10 * 1024 * 1024},
		},
	}
	store, err := NewStore(cfg)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer func() { _ = store.Close() }()

	// ratio1 = tier1.Resolution / tier0.Resolution = 60s / 1s = 60
	if store.ratio1 != 60 {
		t.Errorf("ratio1 = %d, want 60", store.ratio1)
	}
	// ratio2 = tier2.Resolution / tier1.Resolution = 5m / 1m = 5
	if store.ratio2 != 5 {
		t.Errorf("ratio2 = %d, want 5", store.ratio2)
	}
}

// TestRatioCachingSingleTier verifies that a single-tier store (no
// aggregation) has both ratios set to 0.
func TestRatioCachingSingleTier(t *testing.T) {
	store := newTestStore(t)
	defer func() { _ = store.Close() }()

	if store.ratio1 != 0 {
		t.Errorf("single-tier store ratio1 = %d, want 0", store.ratio1)
	}
	if store.ratio2 != 0 {
		t.Errorf("single-tier store ratio2 = %d, want 0", store.ratio2)
	}
}

// ---- Segment-1 skip for wrapped tiers ---------------------------------------

// TestWrappedTierRecentQueryCorrect verifies that after the ring buffer wraps,
// a query for the last N seconds returns only records from that window.
func TestWrappedTierRecentQueryCorrect(t *testing.T) {
	dir := t.TempDir()
	cfg := config.StorageConfig{
		Directory: dir,
		Tiers: []config.TierConfig{
			{Resolution: time.Second, MaxSize: "64KB", MaxBytes: 64 * 1024},
		},
	}
	store, err := NewStore(cfg)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer func() { _ = store.Close() }()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	total := 300
	for i := 0; i < total; i++ {
		ts := base.Add(time.Duration(i) * time.Second)
		if err := store.WriteSample(makeSampleWithCPU(ts, float64(i))); err != nil {
			t.Fatalf("WriteSample(%d): %v", i, err)
		}
	}

	if !store.tiers[0].wrapped {
		t.Skip("tier did not wrap — increase total or decrease MaxBytes")
	}

	from := base.Add(time.Duration(total-10) * time.Second)
	to := base.Add(time.Duration(total) * time.Second)

	results, err := store.tiers[0].ReadRange(from, to)
	if err != nil {
		t.Fatalf("ReadRange: %v", err)
	}
	if len(results) < 8 {
		t.Errorf("ReadRange after wrap returned %d results, expected ~10", len(results))
	}
	for _, r := range results {
		if r.Timestamp.Before(from) || r.Timestamp.After(to) {
			t.Errorf("result timestamp %v outside window [%v, %v]",
				r.Timestamp, from, to)
		}
	}
}

// TestWriteSampleClampsBackwardClock verifies a wall-clock regression does not
// produce a record older than the previous one on disk; the timestamp is
// clamped so stored order stays monotonic (otherwise range scans skip the
// finest tier or hide records).
func TestWriteSampleClampsBackwardClock(t *testing.T) {
	dir := t.TempDir()
	cfg := config.StorageConfig{
		Directory: dir,
		Tiers:     []config.TierConfig{{Resolution: time.Second, MaxSize: "1MB", MaxBytes: 1024 * 1024}},
	}
	store, err := NewStore(cfg)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer func() { _ = store.Close() }()

	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	if err := store.WriteSample(makeVarSample(base, 2)); err != nil {
		t.Fatalf("WriteSample 0: %v", err)
	}
	if err := store.WriteSample(makeVarSample(base.Add(time.Second), 2)); err != nil {
		t.Fatalf("WriteSample 1: %v", err)
	}

	// Clock steps back an hour.
	back := makeVarSample(base.Add(-time.Hour), 2)
	if err := store.WriteSample(back); err != nil {
		t.Fatalf("WriteSample back: %v", err)
	}
	if back.Timestamp.Before(base.Add(time.Second)) {
		t.Errorf("sample timestamp not clamped: got %s, want >= %s",
			back.Timestamp.Format(time.RFC3339), base.Add(time.Second).Format(time.RFC3339))
	}

	// Resume forward, then verify everything on disk is non-decreasing.
	if err := store.WriteSample(makeVarSample(base.Add(2*time.Second), 2)); err != nil {
		t.Fatalf("WriteSample 2: %v", err)
	}
	res, err := store.QueryRange(base.Add(-2*time.Hour), base.Add(time.Hour))
	if err != nil {
		t.Fatalf("QueryRange: %v", err)
	}
	if len(res) == 0 {
		t.Fatal("QueryRange returned nothing")
	}
	for i := 1; i < len(res); i++ {
		if res[i].Timestamp.Before(res[i-1].Timestamp) {
			t.Errorf("non-monotonic stored order at %d: %s before %s",
				i, res[i].Timestamp.Format(time.RFC3339), res[i-1].Timestamp.Format(time.RFC3339))
		}
	}
}

// TestAggBufferBoundedOnTierWriteFailure verifies that if a lower tier's writes
// keep failing, the in-memory aggregation buffer stays bounded (a stuck disk
// must not OOM the process). Raw samples remain safe in tier 0.
func TestAggBufferBoundedOnTierWriteFailure(t *testing.T) {
	dir := t.TempDir()
	cfg := config.StorageConfig{
		Directory: dir,
		Tiers: []config.TierConfig{
			{Resolution: time.Second, MaxSize: "1MB", MaxBytes: 1024 * 1024},
			{Resolution: 2 * time.Second, MaxSize: "1MB", MaxBytes: 1024 * 1024},
		},
	}
	store, err := NewStore(cfg)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer func() { _ = store.Close() }()

	// Break tier 1 so every tier-1 flush fails.
	_ = store.tiers[1].file.Close()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 2000; i++ {
		// Errors are expected once tier-1 flushing starts failing; tier 0 still
		// stores the raw sample.
		_ = store.WriteSample(makeVarSample(base.Add(time.Duration(i)*time.Second), 2))
	}

	if store.ratio1 <= 0 {
		t.Fatalf("precondition: ratio1 should be > 0, got %d", store.ratio1)
	}
	cap1 := store.ratio1 * maxAggBufferFactor
	if got := len(store.tier1Buf); got > cap1 {
		t.Errorf("tier1Buf grew to %d after 2000 failing writes; expected bounded by %d", got, cap1)
	}
	// Tier 0 must still hold the data despite tier-1 failures.
	if n := store.tiers[0].Count(); n == 0 {
		t.Error("tier 0 lost data during tier-1 failures")
	}
}
