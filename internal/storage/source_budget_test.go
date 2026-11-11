package storage

import (
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	"kula/internal/collector"
	"kula/internal/config"
)

func TestQuerySourceBudget(t *testing.T) {
	for _, interval := range []time.Duration{time.Second, time.Millisecond} {
		t.Run(interval.String(), func(t *testing.T) {
			store := newTestStore(t)
			defer func() { _ = store.Close() }()
			base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			for i := 1; i <= maxHistorySourceRecords+1; i++ {
				writeTierSample(t, store.tiers[0], base.Add(time.Duration(i)*interval), interval, 42)
			}
			end := base.Add(maxHistorySourceRecords * interval)
			if result, err := store.QueryRangeWithMeta(base, end, 2); err != nil || len(result.Samples) == 0 {
				t.Fatalf("exact budget should succeed: result=%v err=%v", result, err)
			}
			// Dense ranges still include every record, independently of the
			// nominal source resolution or tier-selection target.
			result, err := store.QueryRangeWithMeta(base, end.Add(interval), 2)
			if err != nil || result == nil {
				t.Fatalf("over budget: result=%v err=%v", result, err)
			}
			count := 0
			for _, sample := range result.Samples {
				count += sample.SampleCount
			}
			if count != maxHistorySourceRecords+1 || !result.ExactComplete {
				t.Fatalf("truncated streamed result: contributors=%d complete=%v", count, result.ExactComplete)
			}
			if _, err := store.QueryRangeWithMeta(end, end.Add(interval), 2); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestTierSourceBudgetAcrossWrap(t *testing.T) {
	tier, err := OpenTier(t.TempDir()+"/tier.dat", 8192)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tier.Close() }()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range 100 {
		if err := tier.Write(varSample(base.Add(time.Duration(i)*time.Second), 1)); err != nil {
			t.Fatal(err)
		}
	}
	all, err := tier.ReadRange(base, base.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !tier.wrapped || len(all) < 3 {
		t.Fatal("fixture did not retain wrapped data")
	}
	var got []*AggregatedSample
	_, err = tier.scanRange(base, base.Add(time.Hour), 3, func(batch []*AggregatedSample) error {
		if len(batch) > 3 {
			t.Fatalf("batch limit exceeded: %d", len(batch))
		}
		got = append(got, batch...)
		return nil
	})
	if err != nil || !reflect.DeepEqual(got, all) {
		t.Fatalf("wrapped scan mismatch: err=%v got=%d want=%d", err, len(got), len(all))
	}
}

func TestHistoryThirtyDays(t *testing.T) {
	store, err := NewStore(config.StorageConfig{Directory: t.TempDir(), Tiers: []config.TierConfig{
		{Resolution: time.Second, MaxBytes: 1024 * 1024},
		{Resolution: time.Minute, MaxBytes: 1024 * 1024},
		{Resolution: 5 * time.Minute, MaxBytes: 64 * 1024 * 1024},
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const count = 30 * 24 * 12
	for i := 1; i <= count; i++ {
		input := makeSampleWithCPU(base.Add(time.Duration(i)*5*time.Minute), float64(i%100))
		if i == historyDecodeBatchSize+1 {
			input.CPU.Total.Usage = 999
		}
		agg := &AggregatedSample{Timestamp: input.Timestamp, Duration: 5 * time.Minute,
			Data: input, Min: input, Max: input, AggregationVersion: currentAggregationVersion,
			MeanWeightsComplete: true, MeanStats: map[string]meanAccumulator{}}
		if err := store.tiers[2].Write(agg); err != nil {
			t.Fatal(err)
		}
	}
	to := base.Add(30 * 24 * time.Hour)
	for _, points := range []int{1, 450, 5000} {
		result, err := store.QueryRangeWithMeta(base, to, points)
		if err != nil {
			t.Fatal(err)
		}
		if result.Tier != 2 || !result.Complete || !result.ExactComplete || len(result.Samples) == 0 || len(result.Samples) > points {
			t.Fatalf("30d coverage: tier=%d complete=%v exact=%v points=%d", result.Tier, result.Complete, result.ExactComplete, len(result.Samples))
		}
		var contributors int
		var maximum float64
		for _, sample := range result.Samples {
			contributors += sample.SampleCount
			maximum = math.Max(maximum, sample.Max.CPU.Total.Usage)
		}
		if contributors != count || maximum != 999 {
			t.Fatalf("lost observations or peak: count=%d peak=%v", contributors, maximum)
		}
	}
}

func TestBatchedScanAllowsWrites(t *testing.T) {
	store := newTestStore(t)
	defer func() { _ = store.Close() }()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range 10 {
		writeTierSample(t, store.tiers[0], base.Add(time.Duration(i)*time.Second), time.Second, float64(i))
	}
	var got []*AggregatedSample
	_, err := store.tiers[0].scanRange(base, base.Add(time.Hour), 3, func(batch []*AggregatedSample) error {
		got = append(got, batch...)
		// Synchronous write would deadlock if reduction retained the tier lock.
		return store.WriteSample(makeSample(base.Add(time.Duration(10+len(got)) * time.Second)))
	})
	if err != nil || len(got) != 10 {
		t.Fatalf("snapshot included new writes or lost data: count=%d err=%v", len(got), err)
	}
}

func TestBatchedScanDetectsOverwrittenSnapshot(t *testing.T) {
	tier, err := OpenTier(t.TempDir()+"/tier.dat", 8192)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tier.Close() }()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range 10 {
		if err := tier.Write(varSample(base.Add(time.Duration(i)*time.Second), 1)); err != nil {
			t.Fatal(err)
		}
	}
	_, err = tier.scanRange(base, base.Add(time.Hour), 3, func(batch []*AggregatedSample) error {
		for i := range 100 {
			if err := tier.Write(varSample(base.Add(time.Duration(100+i)*time.Second), 1)); err != nil {
				return err
			}
		}
		return nil
	})
	if !errors.Is(err, errHistorySnapshotExpired) {
		t.Fatalf("overwritten snapshot accepted: %v", err)
	}
}

func TestStreamingMatchesWholeRange(t *testing.T) {
	store := newTestStore(t)
	defer func() { _ = store.Close() }()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 1; i <= 3*historyDecodeBatchSize+7; i++ {
		input := aggregationFixture(base.Add(time.Duration(i) * time.Second))
		setAggregationFixtureNumbers(reflect.ValueOf(input), float64(i%7))
		if i > 1 && i < historyDecodeBatchSize {
			input.Apps.Containers = nil
			input.Apps.Custom = nil
		}
		if i%3 == 0 {
			input.Apps.Mysql.ReplicaSecondsBehind = -1
		}
		if err := store.tiers[0].Write(&AggregatedSample{
			Timestamp: input.Timestamp, Duration: time.Second + time.Duration(i%3)*time.Millisecond,
			Data: input, AggregationVersion: currentAggregationVersion,
		}); err != nil {
			t.Fatal(err)
		}
	}
	from, to := base.Add(123*time.Millisecond), base.Add(2000*time.Second)
	for _, points := range []int{1, 40, 5000} {
		all, err := store.tiers[0].ReadRange(from, to)
		if err != nil {
			t.Fatal(err)
		}
		want, step := store.downsampleHistory(all, from, to, points, time.Second, true)
		got, err := store.readHistory(store.tiers[0], from, to, points, time.Second, true)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Samples) != len(want) || got.Resolution != fmtRes(step) {
			t.Fatalf("streamed bucket layout differs for %d points", points)
		}
		for i, sample := range got.Samples {
			other := want[i]
			if sample.SampleCount != other.SampleCount || sample.Duration != other.Duration || !sample.BucketStart.Equal(other.BucketStart) || !sample.BucketEnd.Equal(other.BucketEnd) {
				t.Fatalf("bucket %d metadata differs across batch boundaries", i)
			}
			for j, data := range []*collector.Sample{sample.Data, sample.Min, sample.Max} {
				expected := []*collector.Sample{other.Data, other.Min, other.Max}[j]
				if data == nil || expected == nil {
					if data != expected {
						t.Fatal("envelope presence changed")
					}
					continue
				}
				if math.Abs(data.CPU.Total.Usage-expected.CPU.Total.Usage) > 1e-9 ||
					data.Apps.Mysql.ReplicaSecondsBehind != expected.Apps.Mysql.ReplicaSecondsBehind ||
					data.Apps.Nginx.ActiveConnections != expected.Apps.Nginx.ActiveConnections {
					t.Fatalf("bucket %d means or extrema changed across batches", i)
				}
				if len(data.Apps.Containers) != len(expected.Apps.Containers) {
					t.Fatal("dynamic identity lost")
				}
				if len(data.Apps.Containers) > 0 && math.Abs(data.Apps.Containers[0].CPUPct-expected.Apps.Containers[0].CPUPct) > 1e-9 {
					t.Fatal("sparse mean lost its contributing weights")
				}
				if len(data.Apps.Custom["group"]) != len(expected.Apps.Custom["group"]) {
					t.Fatal("custom metric lost")
				}
				if len(data.Apps.Custom["group"]) > 0 && math.Abs(data.Apps.Custom["group"][0].Value-expected.Apps.Custom["group"][0].Value) > 1e-9 {
					t.Fatal("custom metric weights changed")
				}
			}
		}
	}
}

func TestBatchedScanAcrossConcurrentWrap(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	encoded, err := encodeSampleV(varSample(base, 1))
	if err != nil {
		t.Fatal(err)
	}
	tier, err := OpenTier(t.TempDir()+"/tier.dat", headerSize+12*int64(4+len(encoded)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tier.Close() }()
	for i := range 10 {
		if err := tier.Write(varSample(base.Add(time.Duration(i)*time.Second), 1)); err != nil {
			t.Fatal(err)
		}
	}
	want, err := tier.ReadRange(base, base.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var got []*AggregatedSample
	writes := 10
	_, err = tier.scanRange(base, base.Add(time.Hour), 3, func(batch []*AggregatedSample) error {
		got = append(got, batch...)
		writes++
		return tier.Write(varSample(base.Add(time.Duration(writes)*time.Second), 1))
	})
	if err != nil || !reflect.DeepEqual(got, want) || tier.writeCycle == 0 {
		t.Fatalf("snapshot lost during safe wrap: err=%v got=%d want=%d cycle=%d", err, len(got), len(want), tier.writeCycle)
	}
}

// budgetFixture returns a three-tier store holding a little over three hours
// of live-written history starting at base, and the newest record's
// timestamp. Three hours of raw records are past the read budget; the same
// hours of tier-1 rollups are well within it.
func budgetFixture(t *testing.T, base time.Time) (*Store, time.Time) {
	t.Helper()
	store := newMultiTierStore(t)
	t.Cleanup(func() { _ = store.Close() })
	const seconds = 3*3600 + 120
	writeOffsetSamples(t, store, base, 1, seconds, 370*time.Millisecond, func(i int) float64 { return float64(10 + i%40) })
	return store, base.Add(seconds*time.Second + 370*time.Millisecond)
}

func budgetQuery(t *testing.T, store *Store, from, to time.Time, hint int) *HistoryResult {
	t.Helper()
	result, err := store.QueryRangeFromTier(from, to, 450, hint)
	if err != nil {
		t.Fatalf("QueryRangeFromTier(%s, %s, hint %d): %v", from.Format(time.RFC3339), to.Format(time.RFC3339), hint, err)
	}
	if len(result.Samples) == 0 {
		t.Fatalf("QueryRangeFromTier(%s, %s, hint %d): no samples", from.Format(time.RFC3339), to.Format(time.RFC3339), hint)
	}
	return result
}

// TestTierHintStaysWithinTheSourceBudget verifies that a tier hint moves a
// window onto the named tier only while reading it there fits the read
// budget. The tail of a view does; `?tier=0` on a long window used to read
// every raw record the tier retained of it.
func TestTierHintStaysWithinTheSourceBudget(t *testing.T) {
	store, edge := budgetFixture(t, time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC))

	view := edge.Add(-3 * time.Hour)
	if got := budgetQuery(t, store, view, edge, -1).Tier; got != 1 {
		t.Fatalf("three hours without a hint came from tier %d, want 1", got)
	}
	if got := budgetQuery(t, store, view, edge, 0).Tier; got != 1 {
		t.Errorf("three hours hinted onto the raw tier came from tier %d, want 1: the hint passed the read budget", got)
	}

	// A view's newest buckets fit the budget on any tier, so the hint decides.
	tail := edge.Add(-5 * time.Minute)
	for _, hint := range []int{0, 1} {
		if got := budgetQuery(t, store, tail, edge, hint).Tier; got != hint {
			t.Errorf("five-minute tail hinted onto tier %d came from tier %d", hint, got)
		}
	}
}

// TestFutureWindowIsPlannedAsEndingNow verifies that the part of a window
// after the present does not count against the tiers: no tier can cover it,
// and with every tier partial the raw tier used to win on overlap and be read
// for all it retained of the window.
func TestFutureWindowIsPlannedAsEndingNow(t *testing.T) {
	// These records are ahead of any test machine's clock, which makes the
	// newest of them the present whatever that clock reads (planningEnd).
	store, edge := budgetFixture(t, time.Date(2100, 3, 1, 9, 0, 0, 0, time.UTC))

	from := edge.Add(-3 * time.Hour)
	present := budgetQuery(t, store, from, edge, -1)
	future := budgetQuery(t, store, from, edge.Add(28*24*time.Hour), -1)
	if present.Tier != 1 || future.Tier != 1 {
		t.Fatalf("three hours ending now came from tier %d, ending in four weeks from tier %d, want 1 for both", present.Tier, future.Tier)
	}
	if !future.Complete || future.ExactComplete {
		t.Errorf("window ending in the future: complete=%v exact_complete=%v, want retention-complete but not exact", future.Complete, future.ExactComplete)
	}

	// Without such records the present is the clock's.
	empty := newTestStore(t)
	defer func() { _ = empty.Close() }()
	past := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	if got := empty.planningEnd(past); !got.Equal(past) {
		t.Errorf("planningEnd moved a past end from %s to %s", past, got)
	}
	if got := empty.planningEnd(time.Now().Add(time.Hour)); got.After(time.Now()) {
		t.Errorf("planningEnd left an end in the future: %s", got)
	}
}

// TestPartialTiersStayWithinTheSourceBudget covers a window no tier retains
// in full, such as a week asked of a young store. The tier with the greatest
// overlap is then the raw one, by the seconds its rollups trail it, and it
// used to be read whole. A coarser tier covering as much now answers; raw
// records are still read past the budget when no other tier holds them.
func TestPartialTiersStayWithinTheSourceBudget(t *testing.T) {
	store, edge := budgetFixture(t, time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC))
	week := edge.Add(-7 * 24 * time.Hour)
	for _, hint := range []int{-1, 0} {
		result := budgetQuery(t, store, week, edge, hint)
		if result.Tier != 1 || result.Complete {
			t.Errorf("a week of a three-hour store (hint %d): tier %d complete=%v, want tier 1, partial", hint, result.Tier, result.Complete)
		}
	}

	// Rollups for the last half hour only: the raw tier alone holds the rest.
	sparse := newMultiTierStore(t)
	defer func() { _ = sparse.Close() }()
	base := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	const rawOnly, total = 9000, 10800
	for i := 1; i <= rawOnly; i++ {
		writeTierSample(t, sparse.tiers[0], base.Add(time.Duration(i)*time.Second), time.Second, 42)
	}
	writeOffsetSamples(t, sparse, base, rawOnly+1, total, 0, func(int) float64 { return 42 })
	end := base.Add(total * time.Second)
	result := budgetQuery(t, sparse, end.Add(-7*24*time.Hour), end, -1)
	contributors := 0
	for _, sample := range result.Samples {
		contributors += sample.SampleCount
	}
	if result.Tier != 0 || contributors != total {
		t.Errorf("raw-only history: tier %d with %d contributing records, want tier 0 with %d", result.Tier, contributors, total)
	}
}
