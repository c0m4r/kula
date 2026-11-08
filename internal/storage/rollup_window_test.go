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
