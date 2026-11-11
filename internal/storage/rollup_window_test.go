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

// TestRestartBoundsRawDurations replays raw records whose duration claims a
// day, as records written before 0.20.0 carried a whole collection gap and a
// crafted tier file can claim anything. A raw record claiming a year was split
// into one part per minute of that year at startup, allocating 8.9 GB and
// overwriting tier 1. It must roll up as the one-resolution record
// WriteSample writes after a gap.
func TestRestartBoundsRawDurations(t *testing.T) {
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	writeRaw := func(t *testing.T, store *Store, ts time.Time, duration time.Duration, usage float64) {
		t.Helper()
		if err := store.tiers[0].Write(&AggregatedSample{
			Timestamp: ts, Duration: duration, Data: makeSampleWithCPU(ts, usage),
			AggregationVersion: currentAggregationVersion, ExtremaProfile: "raw",
		}); err != nil {
			t.Fatal(err)
		}
	}
	restart := func(t *testing.T, store *Store) *Store {
		t.Helper()
		cfg, dir := store.configs, store.dir
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		return reopenStore(t, dir, cfg)
	}

	t.Run("before the first rollup", func(t *testing.T) {
		store := newMultiTierStore(t)
		writeRaw(t, store, base.Add(300*time.Millisecond), time.Second, 7)
		writeRaw(t, store, base.Add(1300*time.Millisecond), 24*time.Hour, 100)
		store = restart(t, store)
		defer func() { _ = store.Close() }()
		// The first record's part before 00:00 closes that minute; the rest
		// stays buffered.
		if got := store.tiers[1].Count(); got != 1 {
			t.Fatalf("tier 1 holds %d records after the restart, want 1: the day-long record was split per minute", got)
		}
		if oldest := store.tiers[1].OldestTimestamp(); !oldest.Equal(base) {
			t.Fatalf("tier 1 starts at %s, want %s", oldest.Format(time.RFC3339), base.Format(time.RFC3339))
		}
	})

	t.Run("straddling the last rollup", func(t *testing.T) {
		// As TestRestartRollsUpAWindowACrashLeftOpen, but the record that
		// closes the minute claims a day, reaching back past the newest
		// rollup: it must not stand in for the minute's other samples.
		store := newMultiTierStore(t)
		writeOffsetSamples(t, store, base, 0, 59, 300*time.Millisecond, func(int) float64 { return 7 })
		before := store.tiers[1].Count()
		writeRaw(t, store, base.Add(60*time.Second+300*time.Millisecond), 24*time.Hour, 100)
		store = restart(t, store)
		defer func() { _ = store.Close() }()
		if got := store.tiers[1].Count(); got != before+1 {
			t.Fatalf("tier 1 holds %d records after the restart, want %d", got, before+1)
		}
		records, err := store.tiers[1].ReadRange(base, base.Add(2*time.Minute))
		if err != nil || len(records) == 0 {
			t.Fatalf("tier 1: %d records, err %v", len(records), err)
		}
		last := records[len(records)-1]
		if !last.Timestamp.Equal(base.Add(time.Minute)) || last.Duration != time.Minute {
			t.Fatalf("recovered rollup ends %s and covers %s, want 00:01:00 and 1m", last.Timestamp.Format(time.RFC3339Nano), last.Duration)
		}
		// 59.3 s at 7 % and the last 0.7 s at 100 %.
		if usage := last.Data.CPU.Total.Usage; math.Abs(usage-(59.3*7+0.7*100)/60) > 0.01 {
			t.Fatalf("recovered minute averages %.2f %% CPU, want %.2f %%", usage, (59.3*7+0.7*100)/60)
		}
	})
}

// TestRollupsTolerateClockReadingJitter covers durations that miss the
// wall-clock spacing of their timestamps by nanoseconds, as every raw record
// written before 0.22.0 does (they were monotonic-clock deltas): a restart
// must roll the pending minute up as one record, not one per sample, and a
// short outage must still split tier 2 as it splits tier 1.
func TestRollupsTolerateClockReadingJitter(t *testing.T) {
	store := newMultiTierStore(t)
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	for i := 1; i <= 40; i++ {
		if i > 20 && i <= 30 {
			continue // a ten-second outage
		}
		ts := base.Add(time.Duration(i)*time.Second + 300*time.Millisecond)
		if err := store.tiers[0].Write(&AggregatedSample{
			Timestamp: ts, Duration: time.Second - 10*time.Nanosecond, Data: makeSampleWithCPU(ts, 7),
			AggregationVersion: currentAggregationVersion, ExtremaProfile: "raw",
		}); err != nil {
			t.Fatal(err)
		}
	}
	cfg, dir := store.configs, store.dir
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = reopenStore(t, dir, cfg)
	defer func() { _ = store.Close() }()
	writeOffsetSamples(t, store, base, 41, 300, 300*time.Millisecond, func(int) float64 { return 7 })

	// The outage splits the first minute, and so the first five, in two.
	for tier, want := range map[int]int{1: 6, 2: 2} {
		if got := store.tiers[tier].Count(); got != uint64(want) {
			t.Fatalf("tier %d holds %d records, want %d", tier, got, want)
		}
	}
}

// TestRollupsKeepWholeWindowsWhenCollectionStalls: a sample more than two
// intervals after the previous one starts a new run, so a struggling host
// writes many tier-1 records per five minutes. Tier 2 must hold all of them,
// across a restart too; the bound meant for failing writes used to keep only
// the newest 20 records of a window, 20 s of every 2 min here.
func TestRollupsKeepWholeWindowsWhenCollectionStalls(t *testing.T) {
	store := newMultiTierStore(t)
	cfg, dir := store.configs, store.dir
	base := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	ts := base
	writeUntil := func(end time.Time) {
		for ts.Before(end) {
			ts = ts.Add(2500 * time.Millisecond)
			if err := store.WriteSample(makeSampleWithCPU(ts, 50)); err != nil {
				t.Fatal(err)
			}
		}
	}
	writeUntil(base.Add(7*time.Minute + 30*time.Second))
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = reopenStore(t, dir, cfg)
	defer func() { _ = store.Close() }()
	writeUntil(base.Add(20 * time.Minute))

	const window = 5 * time.Minute
	held := func(tier int) map[int64]time.Duration {
		records, err := store.tiers[tier].ReadRange(base, base.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		out := map[int64]time.Duration{}
		for _, record := range records {
			out[epochBucketIndex(record.Timestamp.Add(-time.Nanosecond), window)] += record.Duration
		}
		return out
	}
	tier1, tier2 := held(1), held(2)
	// 120 samples of 1 s each per window: a gap's duration is capped.
	for w := epochBucketIndex(base, window); w < epochBucketIndex(base.Add(20*time.Minute), window); w++ {
		start := time.Unix(0, w*int64(window)).UTC().Format("15:04")
		if tier1[w] != 2*time.Minute || tier2[w] != 2*time.Minute {
			t.Errorf("window %s: tier 1 holds %s, tier 2 %s, want 2m0s each", start, tier1[w], tier2[w])
		}
	}
}

// TestRestartAfterAClockStepBackRollsUpEachSampleOnce: after the clock steps
// back, the raw tail holds samples later than tier 1's newest rollup that were
// rolled up before the step, and tier 1's tail a pending rollup earlier than
// tier 2's newest. A restart must tell them apart by write order; picking them
// by timestamp rolled the first up twice and dropped the second. In the last
// case the step cuts tier 2's run mid-window and the collection after it
// passes that cut before the restart, so a pending tier-1 record holds tier
// 2's newest end without being the record it ended on.
func TestRestartAfterAClockStepBackRollsUpEachSampleOnce(t *testing.T) {
	base := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		until, back  time.Duration
		restartAfter int
	}{
		{20*time.Minute + 30*time.Second, 5 * time.Minute, 30},
		{20*time.Minute + 30*time.Second, 5 * time.Minute, 90},
		{20*time.Minute + 30*time.Second, 5 * time.Minute, 200},
		{20*time.Minute + 30*time.Second, 5 * time.Minute, 400},
		{23*time.Minute + 2*time.Second, 90 * time.Second, 180},
	} {
		store := newMultiTierStore(t)
		cfg, dir := store.configs, store.dir
		samples := 0
		write := func(ts time.Time) {
			if err := store.WriteSample(makeSampleWithCPU(ts, 50)); err != nil {
				t.Fatal(err)
			}
			samples++
		}
		for ts := base.Add(time.Second); !ts.After(base.Add(tc.until)); ts = ts.Add(time.Second) {
			write(ts)
		}
		stepped := base.Add(tc.until - tc.back)
		for i := 1; i <= tc.restartAfter; i++ {
			write(stepped.Add(time.Duration(i) * time.Second))
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		store = reopenStore(t, dir, cfg)
		for i := tc.restartAfter + 1; i <= tc.restartAfter+600; i++ {
			write(stepped.Add(time.Duration(i) * time.Second))
		}

		sum := func(records []*AggregatedSample) (total time.Duration) {
			for _, record := range records {
				total += record.Duration
			}
			return total
		}
		held := func(tier int) time.Duration {
			records, err := store.tiers[tier].ReadRange(base.Add(-time.Hour), base.Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			return sum(records)
		}
		raw := time.Duration(samples) * time.Second
		tier1 := held(1) + sum(store.tier1Buf)
		tier2 := held(2) + sum(store.tier2Buf) + sum(store.tier1Buf)
		if tier1 != raw || tier2 != raw {
			t.Errorf("%s back at %s, restart %d s later: tier 1 holds %s and tier 2 %s of %s collected",
				tc.back, base.Add(tc.until).Format("15:04:05"), tc.restartAfter, tier1, tier2, raw)
		}
		_ = store.Close()
	}
}

// TestWriteSampleDurationsTileWallClockTimestamps: a collector's timestamps
// come from time.Now(), whose monotonic reading Sub would otherwise use.
func TestWriteSampleDurationsTileWallClockTimestamps(t *testing.T) {
	store := newTestStore(t)
	defer func() { _ = store.Close() }()
	var previous time.Time
	for i := 0; i < 500; i++ {
		now := time.Now()
		if err := store.WriteSample(makeSampleWithCPU(now, 1)); err != nil {
			t.Fatal(err)
		}
		latest, err := store.QueryLatest()
		if err != nil {
			t.Fatal(err)
		}
		if want := now.Round(0).Sub(previous); i > 0 && want > 0 && latest.Duration != want {
			t.Fatalf("sample %d covers %s, its timestamps are %s apart", i, latest.Duration, want)
		}
		previous = now.Round(0)
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

// TestFreshStoreKeepsShortViewsOnTheRawTier: a coarse tier's left coverage
// tolerance assumes its oldest rollup spans a whole window, while a store's
// first rollup holds only the part of the window it has run for. Forty seconds
// after a fresh start that crossed a five-minute boundary, a 5-minute view
// must stay on the raw samples, not become one partial 5m point.
func TestFreshStoreKeepsShortViewsOnTheRawTier(t *testing.T) {
	store := newMultiTierStore(t)
	defer func() { _ = store.Close() }()
	base := time.Date(2026, 3, 1, 10, 4, 20, 0, time.UTC)
	writeOffsetSamples(t, store, base, 0, 40, 300*time.Millisecond, func(int) float64 { return 7 })
	if store.tiers[2].Count() == 0 {
		t.Fatal("precondition: no 5m rollup after crossing a five-minute boundary")
	}

	to := base.Add(40*time.Second + 300*time.Millisecond)
	for _, points := range []int{300, 1800} {
		result, err := store.QueryRangeWithMeta(to.Add(-5*time.Minute), to, points)
		if err != nil {
			t.Fatal(err)
		}
		if result.Tier != 0 || result.Complete {
			t.Fatalf("5-minute view at %d points: tier %d (%s), complete %v; want tier 0, partial",
				points, result.Tier, result.Resolution, result.Complete)
		}
	}
}

// TestYoungStoreAcrossRestartsKeepsTheRawTier is the case the 0.22.0 audit
// measured: a 10-minute view at 2,000 points on a store whose data began nine
// minutes earlier, with two restarts in between, was answered from tier 2.
func TestYoungStoreAcrossRestartsKeepsTheRawTier(t *testing.T) {
	store := newMultiTierStore(t)
	cfg, dir := store.configs, store.dir
	base := time.Date(2026, 3, 1, 10, 1, 40, 0, time.UTC) // nine minutes before 10:10:40
	writeOffsetSamples(t, store, base, 0, 170, 250*time.Millisecond, func(int) float64 { return 3 })
	for _, run := range [][2]int{{200, 350}, {380, 540}} { // 30 s down before each run
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		store = reopenStore(t, dir, cfg)
		writeOffsetSamples(t, store, base, run[0], run[1], 250*time.Millisecond, func(int) float64 { return 3 })
	}
	defer func() { _ = store.Close() }()
	if store.tiers[2].Count() == 0 {
		t.Fatal("precondition: no 5m rollup")
	}

	to := base.Add(540*time.Second + 250*time.Millisecond)
	result, err := store.QueryRangeWithMeta(to.Add(-10*time.Minute), to, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if result.Tier != 0 || result.Complete {
		t.Fatalf("10-minute view of a nine-minute store: tier %d (%s), complete %v; want tier 0, partial",
			result.Tier, result.Resolution, result.Complete)
	}
}
