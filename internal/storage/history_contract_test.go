package storage

import (
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"kula/internal/collector"
)

// The dashboard decides from /api/history metadata whether to stream
// WebSocket samples onto a view or refresh it, which aggregations to offer,
// and which extrema to trust. These scenarios query real stores the way the
// dashboard does (unaligned millisecond bounds and its point budget), assert
// the server side here, then hand the encoded responses to the dashboard's own
// modules (history_contract_test.mjs) with the conclusions they must reach.

type historyContractRequest struct {
	From           time.Time `json:"from"`
	To             time.Time `json:"to"`
	Points         int       `json:"points"`
	LiveIntervalMS int64     `json:"live_interval_ms"`
}

// historyContractExpect lists what the dashboard must conclude.
type historyContractExpect struct {
	Live         bool     `json:"live"`
	Aggregations []string `json:"aggregations"`
	Gaps         int      `json:"gaps"`
}

type historyContractScenario struct {
	Name     string                 `json:"name"`
	Request  historyContractRequest `json:"request"`
	Expect   historyContractExpect  `json:"expect"`
	Response json.RawMessage        `json:"response"`
}

type historyContractResolution struct {
	Text string  `json:"text"`
	MS   float64 `json:"ms"`
}

// historyContractTail is one rolling tail refresh: the snapshot the dashboard
// holds, the tail it fetches some seconds later, and the full window it must
// end up showing.
type historyContractTail struct {
	Name     string          `json:"name"`
	RangeMS  int64           `json:"range_ms"`
	Points   int             `json:"points"`
	TailFrom time.Time       `json:"tail_from"`
	TailTo   time.Time       `json:"tail_to"`
	Tail     int             `json:"tail_points"`
	Before   json.RawMessage `json:"before"`
	Refresh  json.RawMessage `json:"refresh"`
	After    json.RawMessage `json:"after"`
}

type historyContractFixture struct {
	Scenarios   []historyContractScenario   `json:"scenarios"`
	Tails       []historyContractTail       `json:"tails"`
	Resolutions []historyContractResolution `json:"resolutions"`
}

func contractQuery(t *testing.T, store *Store, from, to time.Time, points int) *HistoryResult {
	t.Helper()
	result, err := store.QueryRangeWithMeta(from, to, points)
	if err != nil {
		t.Fatalf("QueryRangeWithMeta(%s, %s, %d): %v", from.Format(time.RFC3339Nano), to.Format(time.RFC3339Nano), points, err)
	}
	return result
}

// jsonNumbersClose reports whether two JSON documents are equal, numbers
// within a relative 1e-9.
func jsonNumbersClose(t *testing.T, a, b []byte) bool {
	t.Helper()
	var left, right any
	if json.Unmarshal(a, &left) != nil || json.Unmarshal(b, &right) != nil {
		t.Fatal("contract documents are not JSON")
	}
	var close func(x, y any) bool
	close = func(x, y any) bool {
		switch xv := x.(type) {
		case float64:
			yv, ok := y.(float64)
			return ok && math.Abs(xv-yv) <= 1e-9*math.Max(1, math.Max(math.Abs(xv), math.Abs(yv)))
		case map[string]any:
			yv, ok := y.(map[string]any)
			if !ok || len(xv) != len(yv) {
				return false
			}
			for key, value := range xv {
				if other, found := yv[key]; !found || !close(value, other) {
					return false
				}
			}
			return true
		case []any:
			yv, ok := y.([]any)
			if !ok || len(xv) != len(yv) {
				return false
			}
			for i := range xv {
				if !close(xv[i], yv[i]) {
					return false
				}
			}
			return true
		default:
			return reflect.DeepEqual(x, y)
		}
	}
	return close(left, right)
}

func contractProfiles(result *HistoryResult) []string {
	profiles := make([]string, 0, len(result.ExtremaProfiles))
	for profile := range result.ExtremaProfiles {
		profiles = append(profiles, profile)
	}
	slices.Sort(profiles)
	return profiles
}

func TestHistoryResponsesMatchDashboardContract(t *testing.T) {
	base := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	at := func(seconds float64) time.Time {
		return base.Add(time.Duration(seconds * float64(time.Second)))
	}

	// Raw tier-0 history: one sample per second with a 100s outage.
	raw := newTestStore(t)
	t.Cleanup(func() { _ = raw.Close() })
	for i := 1; i <= 1000; i++ {
		if i > 200 && i < 300 {
			continue
		}
		if err := raw.WriteSample(makeSampleWithCPU(at(float64(i)), float64(10+i%17))); err != nil {
			t.Fatal(err)
		}
	}

	// Tier-1 history written before and after the Min/Max policy change. The
	// boundary falls inside a 10-minute bucket, so a reduced view also holds a
	// mixed bucket.
	coarse := newMultiTierStore(t)
	t.Cleanup(func() { _ = coarse.Close() })
	for minute := 1; minute <= 180; minute++ {
		end := base.Add(time.Duration(minute) * time.Minute)
		if minute <= 97 {
			writeLegacyTierRecord(t, coarse.tiers[1], legacyHistorySample(t, end))
			continue
		}
		observations := make([]*collector.Sample, 0, 60)
		for second := 59; second >= 0; second-- {
			ts := end.Add(-time.Duration(second) * time.Second)
			observations = append(observations, makeSampleWithCPU(ts, float64(20+second%7)))
		}
		if err := coarse.tiers[1].Write(coarse.aggregateSamples(observations, time.Minute)); err != nil {
			t.Fatal(err)
		}
	}

	dataOnly := []string{"data"}
	extrema := []string{"data", "min", "max"}
	var fixture historyContractFixture
	add := func(name string, store *Store, from, to time.Time, points int, expect historyContractExpect,
		check func(*HistoryResult)) {
		t.Helper()
		result := contractQuery(t, store, from, to, points)
		check(result)
		encoded, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		fixture.Scenarios = append(fixture.Scenarios, historyContractScenario{
			Name: name,
			Request: historyContractRequest{From: from, To: to, Points: points,
				LiveIntervalMS: time.Second.Milliseconds()},
			Expect:   expect,
			Response: encoded,
		})
	}
	metadata := func(name string, result *HistoryResult, tier int, resolution string, downsampled bool,
		available []string) {
		t.Helper()
		if result.Tier != tier || result.Resolution != resolution || result.Downsampled != downsampled ||
			!reflect.DeepEqual(result.AvailableAggregations, available) {
			t.Fatalf("%s: tier=%d resolution=%s downsampled=%v available=%v", name,
				result.Tier, result.Resolution, result.Downsampled, result.AvailableAggregations)
		}
	}

	// A desktop 5-minute view at the live edge is raw and streams.
	add("raw five minutes at the live edge", raw, at(700.377), at(1000.377), 534,
		historyContractExpect{Live: true, Aggregations: dataOnly},
		func(r *HistoryResult) {
			metadata("raw", r, 0, "1s", false, dataOnly)
			if len(r.Samples) != 300 {
				t.Fatalf("raw: %d samples, want 300", len(r.Samples))
			}
		})
	// The same window on a phone: an unaligned 5 minutes spans 301 one-second
	// buckets, so 300 points are answered in 2s buckets with extrema.
	add("five minutes in 2s buckets", raw, at(700.377), at(1000.377), 300,
		historyContractExpect{Live: false, Aggregations: extrema},
		func(r *HistoryResult) { metadata("bucketed", r, 0, "2s", true, extrema) })
	// A record past the requested end is clipped at native resolution. The
	// output is resampled but still carries no extrema, so it keeps streaming.
	add("native view clipped at the right edge", raw, at(600.377), at(900.377), 534,
		historyContractExpect{Live: true, Aggregations: dataOnly},
		func(r *HistoryResult) { metadata("clipped", r, 0, "1s", true, dataOnly) })
	add("raw view across a collection outage", raw, at(100.377), at(400.377), 534,
		historyContractExpect{Live: true, Aggregations: dataOnly, Gaps: 1},
		func(r *HistoryResult) { metadata("outage", r, 0, "1s", true, dataOnly) })
	add("empty window inside retained raw history", raw, at(210.5), at(290.5), 534,
		historyContractExpect{Live: true, Aggregations: dataOnly},
		func(r *HistoryResult) {
			metadata("empty", r, 0, "1s", false, dataOnly)
			if len(r.Samples) != 0 {
				t.Fatalf("empty: %d samples", len(r.Samples))
			}
		})
	add("tier-1 legacy and current buckets", coarse, base.Add(250*time.Millisecond),
		base.Add(3*time.Hour+250*time.Millisecond), 534,
		historyContractExpect{Live: false, Aggregations: extrema},
		func(r *HistoryResult) {
			metadata("tier-1", r, 1, "1m", false, extrema)
			if got := contractProfiles(r); !reflect.DeepEqual(got, []string{"current", "legacy"}) {
				t.Fatalf("tier-1: profiles=%v", got)
			}
		})
	add("tier-1 reduction with a mixed bucket", coarse, base.Add(250*time.Millisecond),
		base.Add(3*time.Hour+250*time.Millisecond), 20,
		historyContractExpect{Live: false, Aggregations: extrema},
		func(r *HistoryResult) {
			metadata("mixed", r, 1, "10m", true, extrema)
			if got := contractProfiles(r); !slices.Contains(got, "mixed") {
				t.Fatalf("mixed: profiles=%v", got)
			}
		})

	// Tier-1 rollups written live cover wall-clock minutes, so a rolling view
	// (native records) and a past window ending mid-minute (re-bucketed for its
	// clipped right edge) describe the same minutes with the same values.
	aligned := newMultiTierStore(t)
	t.Cleanup(func() { _ = aligned.Close() })
	writeOffsetSamples(t, aligned, base, 1, 3*3600+40, 370*time.Millisecond, func(i int) float64 { return float64(10 + i%29) })
	alignedEdge := base.Add(3*3600*time.Second + 40*time.Second + 370*time.Millisecond)
	add("tier-1 rolling view of wall-clock rollups", aligned, alignedEdge.Add(-3*time.Hour), alignedEdge, 534,
		historyContractExpect{Live: false, Aggregations: extrema},
		func(r *HistoryResult) {
			metadata("aligned rolling", r, 1, "1m", false, extrema)
			for _, sample := range r.Samples[1:] {
				if sample.BucketStart.UnixNano()%int64(time.Minute) != 0 || sample.BucketEnd.Sub(sample.BucketStart) != time.Minute {
					t.Fatalf("aligned rolling: bucket %s–%s is not a wall-clock minute",
						sample.BucketStart.Format(time.RFC3339Nano), sample.BucketEnd.Format(time.RFC3339Nano))
				}
			}
		})
	add("tier-1 past window ending mid-minute", aligned, base.Add(30*time.Second+500*time.Millisecond),
		base.Add(2*time.Hour+30*time.Minute+30*time.Second+500*time.Millisecond), 534,
		historyContractExpect{Live: false, Aggregations: extrema},
		func(r *HistoryResult) { metadata("aligned past", r, 1, "1m", true, extrema) })

	// A rolling view refreshed by its tail. Records end 0.37s into each second
	// so source intervals straddle every bucket boundary, as they do live.
	rolling := newTestStore(t)
	t.Cleanup(func() { _ = rolling.Close() })
	writeRolling := func(first, last int) {
		t.Helper()
		for i := first; i <= last; i++ {
			if err := rolling.WriteSample(makeSampleWithCPU(at(float64(i)+0.37), float64(10+i%23))); err != nil {
				t.Fatal(err)
			}
		}
	}
	marshal := func(result *HistoryResult) json.RawMessage {
		t.Helper()
		encoded, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	const rollingRange, rollingPoints = 300 * time.Second, 300
	writeRolling(1, 990)
	before := contractQuery(t, rolling, at(990.9).Add(-rollingRange), at(990.9), rollingPoints)
	metadata("tail before", before, 0, "2s", true, extrema)
	writeRolling(991, 1000)
	step := 2 * time.Second
	tailFrom := before.Samples[len(before.Samples)-1].BucketStart.Add(-2 * step)
	tailPoints := int(alignedBucketCount(tailFrom, at(1000.9), step))
	refresh := contractQuery(t, rolling, tailFrom, at(1000.9), tailPoints)
	after := contractQuery(t, rolling, at(1000.9).Add(-rollingRange), at(1000.9), rollingPoints)
	if refresh.Tier != after.Tier || refresh.Resolution != after.Resolution ||
		refresh.SourceResolution != after.SourceResolution || refresh.Downsampled != after.Downsampled ||
		!reflect.DeepEqual(refresh.AvailableAggregations, after.AvailableAggregations) ||
		!reflect.DeepEqual(refresh.ValidAggregations, after.ValidAggregations) ||
		!reflect.DeepEqual(refresh.ExtremaProfiles, after.ExtremaProfiles) {
		t.Fatalf("tail answered as tier=%d %s/%s downsampled=%v available=%v profiles=%v, want the full view's tier=%d %s/%s",
			refresh.Tier, refresh.Resolution, refresh.SourceResolution, refresh.Downsampled,
			refresh.AvailableAggregations, refresh.ExtremaProfiles, after.Tier, after.Resolution, after.SourceResolution)
	}
	fullBuckets := make(map[int64]json.RawMessage, len(after.Samples))
	for _, sample := range after.Samples {
		fullBuckets[sample.BucketStart.UnixNano()] = marshal(&HistoryResult{Samples: []*AggregatedSample{sample}})
	}
	if len(refresh.Samples) != tailPoints {
		t.Fatalf("tail: %d buckets, want %d", len(refresh.Samples), tailPoints)
	}
	// Equal up to float rounding: where a scan cuts its decode batches depends
	// on where it starts, and a bucket merged across a batch boundary can
	// differ in the last bit of a mean or of an extremum taken from one.
	for _, sample := range refresh.Samples {
		want, ok := fullBuckets[sample.BucketStart.UnixNano()]
		if got := marshal(&HistoryResult{Samples: []*AggregatedSample{sample}}); !ok || !jsonNumbersClose(t, got, want) {
			t.Fatalf("tail bucket %s differs from the full refresh:\n got %s\nwant %s",
				sample.BucketStart.Format(time.RFC3339Nano), got, want)
		}
	}
	fixture.Tails = append(fixture.Tails, historyContractTail{
		Name:     "rolling five minutes in 2s buckets",
		RangeMS:  rollingRange.Milliseconds(),
		Points:   rollingPoints,
		TailFrom: tailFrom,
		TailTo:   at(1000.9),
		Tail:     tailPoints,
		Before:   marshal(before),
		Refresh:  marshal(refresh),
		After:    marshal(after),
	})

	// Every output step and supported tier resolution the server can format.
	seen := make(map[string]bool)
	resolutions := append([]time.Duration{
		50 * time.Millisecond, 100 * time.Millisecond, 250 * time.Millisecond, 500 * time.Millisecond,
		1500 * time.Millisecond,
	}, niceHistorySteps...)
	for _, resolution := range resolutions {
		text := fmtRes(resolution)
		if !seen[text] {
			seen[text] = true
			fixture.Resolutions = append(fixture.Resolutions, historyContractResolution{
				Text: text, MS: float64(resolution) / float64(time.Millisecond),
			})
		}
	}

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; skipping the dashboard side of the contract")
	}
	path := filepath.Join(t.TempDir(), "history-contract.json")
	encoded, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command(node, "../web/testdata/history_contract_test.mjs", path).CombinedOutput()
	if err != nil {
		t.Fatalf("dashboard contract failed: %v\n%s", err, output)
	}
}
