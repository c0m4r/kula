package storage

import (
	"reflect"
	"sync"
	"testing"
	"time"

	"kula/internal/collector"
)

func TestHistoryQueryReleasesLocks(t *testing.T) {
	for _, scenario := range []string{"no tiers", "empty", "fresh", "cached", "expired cache", "outside retention"} {
		t.Run(scenario, func(t *testing.T) {
			store := &Store{}
			if scenario != "no tiers" {
				store = newTestStore(t)
			}
			defer func() { _ = store.Close() }()
			assertUnlocked := func() {
				t.Helper()
				if !store.mu.TryLock() {
					t.Fatal("history query retained the collection lock")
				}
				store.mu.Unlock()
				if !store.queryCacheMu.TryLock() {
					t.Fatal("history query retained the cache lock")
				}
				store.queryCacheMu.Unlock()
			}
			base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			if scenario != "no tiers" && scenario != "empty" {
				if err := store.WriteSample(makeSample(base)); err != nil {
					t.Fatal(err)
				}
			}
			from, to := base.Add(-time.Second), base.Add(time.Second)
			if scenario == "cached" || scenario == "expired cache" {
				store.queryCacheTTL = time.Hour
				if scenario == "expired cache" {
					store.queryCacheTTL = -time.Second
				}
				if _, err := store.QueryRangeWithMeta(from, to, 100); err != nil {
					t.Fatal(err)
				}
				assertUnlocked()
				if len(store.queryCache) != 1 {
					t.Fatal("fixture did not populate the query cache")
				}
			}
			if scenario == "outside retention" {
				from, to = base.Add(time.Hour), base.Add(2*time.Hour)
			}
			_, err := store.QueryRangeWithMeta(from, to, 100)
			assertUnlocked()
			if err != nil {
				t.Fatalf("unexpected query error: %v", err)
			}
		})
	}
}

func TestHistoryOwnsCompleteSampleGraph(t *testing.T) {
	for _, points := range []int{1, 100} {
		t.Run(fmtRes(time.Duration(points)*time.Second), func(t *testing.T) {
			store := newTestStore(t)
			defer func() { _ = store.Close() }()
			base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			for i := 0; i < 3; i++ {
				if err := store.WriteSample(aggregationFixture(base.Add(time.Duration(i) * time.Second))); err != nil {
					t.Fatal(err)
				}
			}
			query := func() *HistoryResult {
				result, err := store.QueryRangeWithMeta(base.Add(-time.Second), base.Add(2*time.Second), points)
				if err != nil {
					t.Fatal(err)
				}
				return result
			}
			first := query()
			expected := query()
			mutate := func(result *HistoryResult) {
				for _, sample := range result.Samples {
					sample.Coverage = -99
					for _, data := range []*collector.Sample{sample.Data, sample.Min, sample.Max} {
						setAggregationFixtureNumbers(reflect.ValueOf(data), 99)
					}
					if sample.MeanStats != nil {
						sample.MeanStats["probe"] = meanAccumulator{Sum: 99, Weight: 1}
					}
				}
			}
			mutate(first)
			if got := query(); !reflect.DeepEqual(got, expected) {
				t.Fatal("cache insertion shared nested data")
			}
			// Concurrent consumers may mutate their own results, including maps,
			// while another reader obtains and inspects a cache hit.
			var wg sync.WaitGroup
			for range 4 {
				wg.Go(func() {
					for range 10 {
						result, err := store.QueryRangeWithMeta(base.Add(-time.Second), base.Add(2*time.Second), points)
						if err != nil {
							t.Error(err)
							return
						}
						if !reflect.DeepEqual(result, expected) {
							t.Error("cache hit shared nested data")
							return
						}
						mutate(result)
					}
				})
			}
			wg.Wait()
		})
	}
}

func TestWriteAndLatestOwnSamples(t *testing.T) {
	store := newTestStore(t)
	defer func() { _ = store.Close() }()
	input := aggregationFixture(time.Now())
	if err := store.WriteSample(input); err != nil {
		t.Fatal(err)
	}
	setAggregationFixtureNumbers(reflect.ValueOf(input), 88)
	first, err := store.QueryLatest()
	if err != nil {
		t.Fatal(err)
	}
	expected := aggregationFixture(input.Timestamp)
	if !reflect.DeepEqual(first.Data, expected) {
		t.Fatal("WriteSample retained caller-owned fields")
	}
	setAggregationFixtureNumbers(reflect.ValueOf(first.Data), 99)
	second, err := store.QueryLatest()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(second.Data, expected) {
		t.Fatal("QueryLatest shared nested fields")
	}
}

func TestHistoryConcurrentCollection(t *testing.T) {
	store := newTestStore(t)
	defer func() { _ = store.Close() }()
	store.queryCacheTTL = time.Hour
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range 200 {
		if err := store.WriteSample(aggregationFixture(base.Add(time.Duration(i) * time.Second))); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Go(func() {
		<-start
		for i := 200; i < 300; i++ {
			if err := store.WriteSample(aggregationFixture(base.Add(time.Duration(i) * time.Second))); err != nil {
				t.Error(err)
				return
			}
		}
	})
	for range 4 {
		wg.Go(func() {
			<-start
			for range 10 {
				result, err := store.QueryRangeWithMeta(base.Add(-time.Second), base.Add(time.Hour), 100)
				if err != nil {
					t.Error(err)
					return
				}
				for _, sample := range result.Samples {
					setAggregationFixtureNumbers(reflect.ValueOf(sample.Data), 99)
				}
			}
		})
	}
	close(start)
	wg.Wait()
	result, err := store.QueryRangeWithMeta(base.Add(-time.Second), base.Add(time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	if result.ActualTo == nil || !result.ActualTo.Equal(base.Add(299*time.Second)) {
		t.Fatalf("obsolete snapshot cached after concurrent writes: %v", result.ActualTo)
	}
	for _, sample := range result.Samples {
		if sample.Data.CPU.Total.Usage != 0 {
			t.Fatal("caller mutation leaked into concurrent query")
		}
	}
}

func TestRollupFailureInvalidatesLiveCache(t *testing.T) {
	store := newMultiTierStore(t)
	defer func() { _ = store.Close() }()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < store.ratio1-1; i++ {
		if err := store.WriteSample(makeSample(base.Add(time.Duration(i) * time.Second))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.QueryRangeWithMeta(base, base.Add(time.Hour), 100); err != nil {
		t.Fatal(err)
	}
	if len(store.queryCache) == 0 {
		t.Fatal("fixture did not populate cache")
	}
	if err := store.tiers[1].file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteSample(makeSample(base.Add(time.Duration(store.ratio1-1) * time.Second))); err == nil {
		t.Fatal("expected rollup failure")
	}
	if len(store.queryCache) != 0 {
		t.Fatal("successful raw write left stale live history after rollup failure")
	}
}
