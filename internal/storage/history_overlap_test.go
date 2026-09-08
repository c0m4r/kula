package storage

import (
	"math"
	"testing"
	"time"

	"kula/internal/config"
)

func TestRollupsPreserveCollectionOutages(t *testing.T) {
	for _, restart := range []string{"none", "before gap", "after gap"} {
		t.Run(restart, func(t *testing.T) {
			cfg := config.StorageConfig{Directory: t.TempDir(), Tiers: []config.TierConfig{
				{Resolution: time.Second, MaxBytes: 1024 * 1024},
				{Resolution: time.Minute, MaxBytes: 1024 * 1024},
				{Resolution: 5 * time.Minute, MaxBytes: 1024 * 1024},
			}}
			store, err := NewStore(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = store.Close() }()
			reopen := func() {
				t.Helper()
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
				store, err = NewStore(cfg)
				if err != nil {
					t.Fatal(err)
				}
			}
			base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			write := func(seconds int, usage float64) {
				t.Helper()
				if err := store.WriteSample(makeSampleWithCPU(base.Add(time.Duration(seconds)*time.Second), usage)); err != nil {
					t.Fatal(err)
				}
			}
			for i := 1; i <= 59; i++ {
				write(i, 100)
			}
			if restart == "before gap" {
				reopen()
			}
			for i := 0; i < 541; i++ {
				write(3660+i, 0)
				if restart == "after gap" && i == 0 {
					reopen()
				}
			}
			reopen() // Coverage must survive the positional codec and recovery.
			from, to := base.Add(3601*time.Second), base.Add(3650*time.Second)
			result, err := store.QueryRangeWithMeta(from, to, 300)
			if err != nil || len(result.Samples) != 0 || result.Complete || result.ExactComplete {
				t.Fatalf("outage query: result=%+v err=%v", result, err)
			}
			for tier := 1; tier <= 2; tier++ {
				result, err := store.readHistory(store.tiers[tier], from, to, 300, cfg.Tiers[tier].Resolution, false)
				if err != nil || len(result.Samples) != 0 {
					t.Fatalf("tier %d filled an outage: result=%+v err=%v", tier, result, err)
				}
				records, err := store.tiers[tier].ReadRange(base, base.Add(2*time.Hour))
				if err != nil || len(records) < 2 {
					t.Fatalf("tier %d lost contiguous runs: records=%d err=%v", tier, len(records), err)
				}
				var weight time.Duration
				for _, record := range records {
					weight += record.Duration
					if usage := record.Data.CPU.Total.Usage; usage != 0 && usage != 100 {
						t.Fatalf("tier %d mixed pre/post-outage values: %v", tier, usage)
					}
				}
				if tier == 1 && weight != 600*time.Second {
					t.Fatalf("partial rollups lost observations: total weight=%s", weight)
				}
			}
		})
	}
}

func TestRawOutageDoesNotFallBackToLegacyRollup(t *testing.T) {
	store := newMultiTierStore(t)
	defer func() { _ = store.Close() }()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	writeTierSample(t, store.tiers[0], base.Add(59*time.Second), time.Second, 100)
	writeTierSample(t, store.tiers[0], base.Add(3660*time.Second), time.Second, 0)
	writeTierSample(t, store.tiers[1], base.Add(3660*time.Second), time.Minute, 98)
	result, err := store.QueryRangeWithMeta(base.Add(3601*time.Second), base.Add(3650*time.Second), 300)
	if err != nil || len(result.Samples) != 0 || result.Complete || result.ExactComplete {
		t.Fatalf("raw outage was replaced with a legacy mean: result=%+v err=%v", result, err)
	}
}

func TestHistoryIncludesOverlappingCoarseIntervals(t *testing.T) {
	for _, tc := range []struct {
		name       string
		from, to   time.Duration
		points     int
		wantMean   float64
		wantCount  int
		wantWeight time.Duration
	}{
		{"inside oldest bucket", 10 * time.Second, 50 * time.Second, 100, 10, 1, 40 * time.Second},
		{"both boundaries", 30 * time.Second, 90 * time.Second, 100, 20, 2, time.Minute},
		{"reduced boundaries", 30 * time.Second, 150 * time.Second, 1, 30, 3, 2 * time.Minute},
		{"touching intervals excluded", time.Minute, 3 * time.Minute, 1, 40, 2, 2 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newMultiTierStore(t)
			defer func() { _ = store.Close() }()
			base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			for i, usage := range []float64{10, 30, 50, 999} {
				writeTierSample(t, store.tiers[1], base.Add(time.Duration(i+1)*time.Minute), time.Minute, usage)
			}
			from, to := base.Add(tc.from), base.Add(tc.to)
			result, err := store.QueryRangeWithMeta(from, to, tc.points)
			if err != nil {
				t.Fatal(err)
			}
			if result.Tier != 1 || !result.Complete || !result.ExactComplete ||
				result.ActualFrom == nil || !result.ActualFrom.Equal(from) ||
				result.ActualTo == nil || !result.ActualTo.Equal(to) {
				t.Fatalf("overlapping range not covered: %+v", result)
			}
			if len(result.Samples) == 0 || len(result.Samples) > tc.points {
				t.Fatalf("got %d samples for budget %d", len(result.Samples), tc.points)
			}
			var weight time.Duration
			var sum float64
			var count int
			for _, sample := range result.Samples {
				if sample.Timestamp.Before(from) || sample.Timestamp.After(to) {
					t.Fatalf("sample timestamp outside viewport: %s", sample.Timestamp)
				}
				weight += sample.Duration
				sum += sample.Data.CPU.Total.Usage * sample.Duration.Seconds()
				count += sample.SampleCount
			}
			if weight != tc.wantWeight || count != tc.wantCount || math.Abs(sum/weight.Seconds()-tc.wantMean) > 1e-6 {
				t.Fatalf("weight/count/mean = %s/%d/%v, want %s/%d/%v", weight, count, sum/weight.Seconds(), tc.wantWeight, tc.wantCount, tc.wantMean)
			}
		})
	}
}

func TestHistoryLookaheadDoesNotFillGaps(t *testing.T) {
	store := newMultiTierStore(t)
	defer func() { _ = store.Close() }()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	writeTierSample(t, store.tiers[1], base.Add(time.Minute), time.Minute, 10)
	writeTierSample(t, store.tiers[1], base.Add(150*time.Second), time.Minute, 999)
	result, err := store.QueryRangeWithMeta(base.Add(30*time.Second), base.Add(90*time.Second), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Samples) != 1 || result.Samples[0].Data.CPU.Total.Usage != 10 ||
		result.ExactComplete || result.ActualTo == nil || !result.ActualTo.Equal(base.Add(time.Minute)) {
		t.Fatalf("lookahead included a nonoverlapping interval: %+v", result)
	}
}

func TestHistoryIncludesOverlappingRawJitter(t *testing.T) {
	store := newTestStore(t)
	defer func() { _ = store.Close() }()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	writeTierSample(t, store.tiers[0], base.Add(1500*time.Millisecond), 1500*time.Millisecond, 42)
	result, err := store.QueryRangeWithMeta(base, base.Add(200*time.Millisecond), 100)
	if err != nil {
		t.Fatal(err)
	}
	if !result.ExactComplete || len(result.Samples) != 1 ||
		result.Samples[0].Duration != 200*time.Millisecond || result.Samples[0].Data.CPU.Total.Usage != 42 {
		t.Fatalf("jittered raw interval was not clipped: %+v", result)
	}
}

func TestHistoryCacheInvalidatesForOverlappingWrite(t *testing.T) {
	store := newTestStore(t)
	defer func() { _ = store.Close() }()
	store.queryCacheTTL = time.Minute
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := store.WriteSample(makeSampleWithCPU(base.Add(time.Second), 10)); err != nil {
		t.Fatal(err)
	}
	from, to := base.Add(500*time.Millisecond), base.Add(1500*time.Millisecond)
	first, err := store.QueryRangeWithMeta(from, to, 100)
	if err != nil || len(first.Samples) != 1 || first.ExactComplete {
		t.Fatalf("initial partial query: result=%+v err=%v", first, err)
	}
	if err := store.WriteSample(makeSampleWithCPU(base.Add(2*time.Second), 30)); err != nil {
		t.Fatal(err)
	}
	second, err := store.QueryRangeWithMeta(from, to, 100)
	if err != nil || !second.ExactComplete || len(second.Samples) != 2 {
		t.Fatalf("new overlapping record did not refresh cached history: result=%+v err=%v", second, err)
	}
}

func TestHistoryCacheInvalidatesForOverlappingRollup(t *testing.T) {
	store := newMultiTierStore(t)
	defer func() { _ = store.Close() }()
	store.queryCacheTTL = time.Minute
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	writeTierSample(t, store.tiers[1], base.Add(time.Minute), time.Minute, 10)
	for i := 61; i < 120; i++ {
		if err := store.WriteSample(makeSampleWithCPU(base.Add(time.Duration(i)*time.Second), 30)); err != nil {
			t.Fatal(err)
		}
	}
	from, to := base.Add(30*time.Second), base.Add(90*time.Second)
	first, err := store.QueryRangeWithMeta(from, to, 100)
	if err != nil || first.Tier != 1 || first.ExactComplete {
		t.Fatalf("initial coarse query: result=%+v err=%v", first, err)
	}
	// This completes a minute rollup whose endpoint is after the cached range.
	if err := store.WriteSample(makeSampleWithCPU(base.Add(2*time.Minute), 30)); err != nil {
		t.Fatal(err)
	}
	second, err := store.QueryRangeWithMeta(from, to, 100)
	if err != nil || second.Tier != 1 || !second.ExactComplete || len(second.Samples) != 2 {
		t.Fatalf("new rollup did not refresh cached history: result=%+v err=%v", second, err)
	}
}

func TestHistoryClipsRightBoundaryAfterDecodeBatch(t *testing.T) {
	store := newMultiTierStore(t)
	defer func() { _ = store.Close() }()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 1; i <= historyDecodeBatchSize+1; i++ {
		writeTierSample(t, store.tiers[1], base.Add(time.Duration(i)*time.Minute), time.Minute, 42)
	}
	from := base.Add(30 * time.Second)
	to := from.Add(historyDecodeBatchSize * time.Minute)
	result, err := store.QueryRangeWithMeta(from, to, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var weight time.Duration
	for _, sample := range result.Samples {
		weight += sample.Duration
	}
	if !result.ExactComplete || !result.Downsampled || len(result.Samples) != historyDecodeBatchSize+1 || weight != to.Sub(from) {
		t.Fatalf("boundary clipping lost pending observations: samples=%d weight=%s complete=%v reduced=%v",
			len(result.Samples), weight, result.ExactComplete, result.Downsampled)
	}
}
