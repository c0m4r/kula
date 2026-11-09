package storage

import (
	"math"
	"testing"
	"time"

	"kula/internal/config"
)

// writeOffsetSamples writes one raw sample per second at base+i+offset for i in
// [first, last], as a live collector does: each source interval straddles a
// window boundary by a fraction of a second.
func writeOffsetSamples(t *testing.T, store *Store, base time.Time, first, last int, offset time.Duration, usage func(int) float64) {
	t.Helper()
	for i := first; i <= last; i++ {
		ts := base.Add(time.Duration(i)*time.Second + offset)
		if err := store.WriteSample(makeSampleWithCPU(ts, usage(i))); err != nil {
			t.Fatalf("WriteSample(%d): %v", i, err)
		}
	}
}

// TestRollupsCoverWallClockWindows pins S3's fix: tier 1 rolls up whole
// epoch-aligned minutes and tier 2 whole five minutes, whatever second the
// process started in, and splitting straddling samples loses no duration.
func TestRollupsCoverWallClockWindows(t *testing.T) {
	store := newMultiTierStore(t)
	defer func() { _ = store.Close() }()
	base := time.Date(2026, 3, 1, 10, 0, 17, 0, time.UTC) // starts mid-minute
	writeOffsetSamples(t, store, base, 0, 16*60, 370*time.Millisecond, func(i int) float64 { return float64(i % 50) })

	for tier, window := range map[int]time.Duration{1: time.Minute, 2: 5 * time.Minute} {
		records, err := store.tiers[tier].ReadRange(base.Add(-time.Hour), base.Add(time.Hour))
		if err != nil || len(records) < 3 {
			t.Fatalf("tier %d: %d records, err %v", tier, len(records), err)
		}
		for i, record := range records {
			if record.Timestamp.UnixNano()%int64(window) != 0 {
				t.Fatalf("tier %d record %d ends at %s, not on a %s boundary", tier, i, record.Timestamp.Format(time.RFC3339Nano), window)
			}
			// The first window began before collection did; every later one
			// is fully covered.
			if i > 0 && record.Duration != window {
				t.Fatalf("tier %d record %d covers %s, want %s", tier, i, record.Duration, window)
			}
		}
	}

	// Every raw second up to the newest rollup's end is in tier 1 exactly once.
	records, _ := store.tiers[1].ReadRange(base.Add(-time.Hour), base.Add(time.Hour))
	var rolled time.Duration
	for _, record := range records {
		rolled += record.Duration
	}
	first := base.Add(370 * time.Millisecond).Add(-time.Second) // the first sample's interval start
	if want := records[len(records)-1].Timestamp.Sub(first); rolled != want {
		t.Fatalf("tier 1 rolled up %s of raw data, want %s", rolled, want)
	}
}

// TestRollingAndPastCoarseViewsAgree reproduces S3: the same tier-1 data used
// to read 100% in a rolling view and 48% in a past window, because rollups
// covered process-aligned minutes and past windows re-bucketed them onto epoch
// minutes. Both views now return the same buckets.
func TestRollingAndPastCoarseViewsAgree(t *testing.T) {
	store := newMultiTierStore(t)
	defer func() { _ = store.Close() }()
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	const spikeStart, spikeEnd = 10*60 + 30, 11*60 + 30 // a 60 s spike from 00:10:30
	writeOffsetSamples(t, store, base, 1, 3*3600+40, 300*time.Millisecond, func(i int) float64 {
		if i >= spikeStart && i < spikeEnd {
			return 100
		}
		return 0
	})

	liveEdge := base.Add(3*3600*time.Second + 40*time.Second + 300*time.Millisecond)
	rolling, err := store.QueryRangeWithMeta(liveEdge.Add(-3*time.Hour), liveEdge, 1000)
	if err != nil {
		t.Fatal(err)
	}
	pastTo := base.Add(2*time.Hour + 30*time.Minute + 30*time.Second + 500*time.Millisecond)
	past, err := store.QueryRangeWithMeta(pastTo.Add(-150*time.Minute), pastTo, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if rolling.Tier != 1 || past.Tier != 1 || rolling.Resolution != "1m" || past.Resolution != "1m" {
		t.Fatalf("views answered from tier %d/%s and %d/%s, want tier 1 at 1m", rolling.Tier, rolling.Resolution, past.Tier, past.Resolution)
	}

	usage := func(result *HistoryResult) map[int64]float64 {
		buckets := make(map[int64]float64, len(result.Samples))
		for _, sample := range result.Samples {
			if sample.BucketEnd.Sub(sample.BucketStart) == time.Minute {
				buckets[sample.BucketStart.UnixNano()] = sample.Data.CPU.Total.Usage
			}
		}
		return buckets
	}
	rollingBuckets, pastBuckets := usage(rolling), usage(past)
	shared := 0
	for start, want := range rollingBuckets {
		got, ok := pastBuckets[start]
		if !ok || !time.Unix(0, start).Before(pastTo.Add(-time.Minute)) {
			continue
		}
		shared++
		if math.Abs(got-want) > 1e-9 {
			t.Fatalf("bucket %s: past window %v, rolling view %v", time.Unix(0, start).UTC().Format(time.RFC3339), got, want)
		}
	}
	if shared < 100 {
		t.Fatalf("only %d buckets shared by both views", shared)
	}
	// The spike is split across its two epoch minutes in both views; neither
	// shows a full-height minute the other halves.
	for _, minute := range []int{10, 11} {
		start := base.Add(time.Duration(minute) * time.Minute).UnixNano()
		if v := rollingBuckets[start]; v < 40 || v > 60 {
			t.Fatalf("spike minute 00:%d: %v%%, want about half", minute, v)
		}
	}
}

// TestRestartRollsUpAWindowACrashLeftOpen covers a crash between the raw write
// that completed a window and that window's rollup: recovery replays the raw
// tail through the same windows and writes the missing rollup.
func TestRestartRollsUpAWindowACrashLeftOpen(t *testing.T) {
	store := newMultiTierStore(t)
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	writeOffsetSamples(t, store, base, 0, 59, 300*time.Millisecond, func(int) float64 { return 7 })
	before := store.tiers[1].Count()
	// The sample at 00:01:00.3 closes the first full minute; write it to tier
	// 0 only, as if the process died before rolling the minute up.
	ts := base.Add(60*time.Second + 300*time.Millisecond)
	if err := store.tiers[0].Write(&AggregatedSample{
		Timestamp: ts, Duration: time.Second, Data: makeSampleWithCPU(ts, 7),
		AggregationVersion: currentAggregationVersion, ExtremaProfile: "raw",
	}); err != nil {
		t.Fatal(err)
	}
	cfg := store.configs
	dir := store.dir
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store = reopenStore(t, dir, cfg)
	defer func() { _ = store.Close() }()
	if got := store.tiers[1].Count(); got != before+1 {
		t.Fatalf("tier 1 holds %d records after recovery, want %d", got, before+1)
	}
	records, err := store.tiers[1].ReadRange(base, base.Add(2*time.Minute))
	if err != nil || len(records) == 0 {
		t.Fatalf("tier 1: %d records, err %v", len(records), err)
	}
	last := records[len(records)-1]
	if !last.Timestamp.Equal(base.Add(time.Minute)) || last.Duration != time.Minute {
		t.Fatalf("recovered rollup ends %s and covers %s, want 00:01:00 and 1m", last.Timestamp.Format(time.RFC3339Nano), last.Duration)
	}
	if len(store.tier1Buf) != 1 || store.tier1Buf[0].Duration != 300*time.Millisecond {
		t.Fatalf("pending tier-1 buffer = %d records, want the 0.3 s after the minute", len(store.tier1Buf))
	}
}

func reopenStore(t *testing.T, dir string, tiers []config.TierConfig) *Store {
	t.Helper()
	store, err := NewStore(config.StorageConfig{Directory: dir, Tiers: tiers})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return store
}

// TestShortViewAfterAStartPrefersRawData covers the first minutes after a
// fresh start: the first tier-1 rollup holds only the part of its minute
// collected so far, and must not outrank the raw tier, or a five-minute view
// is answered with one coarse bucket instead of streaming raw samples.
func TestShortViewAfterAStartPrefersRawData(t *testing.T) {
	store := newMultiTierStore(t)
	defer func() { _ = store.Close() }()
	base := time.Date(2026, 3, 1, 12, 0, 20, 0, time.UTC) // starts 20 s into a minute
	writeOffsetSamples(t, store, base, 0, 45, 300*time.Millisecond, func(int) float64 { return 5 })
	if store.tiers[1].Count() == 0 {
		t.Fatal("precondition: the first minute boundary produced no tier-1 rollup")
	}
	now := base.Add(45*time.Second + 300*time.Millisecond)
	result, err := store.QueryRangeWithMeta(now.Add(-5*time.Minute), now, 534)
	if err != nil {
		t.Fatal(err)
	}
	if result.Tier != 0 || len(result.Samples) < 40 {
		t.Fatalf("five minutes after a start: tier %d with %d samples, want the raw tier's ~46", result.Tier, len(result.Samples))
	}
}

// restartedAfterADowntime returns a store that collected for 2 h 20 min, was
// restarted after five minutes down, and has collected for 15 s since, with
// the time of its newest sample. Tier 1's newest rollup is then the partial
// minute before the shutdown, and the minute in progress is not rolled up yet.
func restartedAfterADowntime(t *testing.T) (*Store, time.Time) {
	t.Helper()
	store := newMultiTierStore(t)
	base := time.Date(2026, 3, 1, 8, 0, 20, 0, time.UTC)
	usage := func(i int) float64 { return float64(10 + i%13) }
	writeOffsetSamples(t, store, base, 0, 8400, 300*time.Millisecond, usage)
	cfg, dir := store.configs, store.dir
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = reopenStore(t, dir, cfg)
	t.Cleanup(func() { _ = store.Close() })
	resume := base.Add(8400*time.Second + 5*time.Minute + 20*time.Second) // 10:25:40
	writeOffsetSamples(t, store, resume, 0, 14, 300*time.Millisecond, usage)
	return store, resume.Add(14*time.Second + 300*time.Millisecond)
}

// TestRestartAfterADowntimeKeepsTierOneAtTheLiveEdge covers the minute after
// a restart: tier 1's newest rollup predates the downtime, but every minute
// since has closed with nothing to roll up, so a view longer than the raw
// tier's source budget is still answered from tier 1, not from 5 m rollups.
func TestRestartAfterADowntimeKeepsTierOneAtTheLiveEdge(t *testing.T) {
	store, edge := restartedAfterADowntime(t)
	if newest := store.tiers[1].NewestTimestamp(); newest.After(edge.Add(-5 * time.Minute)) {
		t.Fatalf("precondition: tier 1's newest rollup ends %s, not before the downtime", newest.Format(time.RFC3339Nano))
	}
	result, err := store.QueryRangeWithMeta(edge.Add(-130*time.Minute), edge, 534)
	if err != nil {
		t.Fatal(err)
	}
	if result.Tier != 1 || !result.Complete {
		t.Fatalf("2 h 10 min view after a restart: tier %d, complete %v; want tier 1, complete", result.Tier, result.Complete)
	}

	// A window still buffered before the one in progress (its write failed)
	// is not final: the coverage stays at the newest rollup.
	store.mu.Lock()
	defer store.mu.Unlock()
	pending := store.tier1Buf
	defer func() { store.tier1Buf = pending }()
	if got, want := store.rolledUpThrough(1), edge.Truncate(time.Minute); !got.Equal(want) {
		t.Fatalf("tier 1 rolled up through %s, want the minute in progress's start %s", got.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
	}
	stuck := &AggregatedSample{Timestamp: edge.Add(-3 * time.Minute), Duration: time.Second}
	store.tier1Buf = append([]*AggregatedSample{stuck}, pending...)
	if got, want := store.rolledUpThrough(1), store.tiers[1].NewestTimestamp(); !got.Equal(want) {
		t.Fatalf("with an older window still buffered, tier 1 rolled up through %s, want its newest rollup %s",
			got.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
	}
}
