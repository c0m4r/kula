package storage

import (
	"math"
	"testing"
	"time"

	"kula/internal/collector"
)

func TestAggregationCascadeWithMissingDynamicMember(t *testing.T) {
	store := &Store{}
	base := time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
	raw := make([]*AggregatedSample, 120)
	for i := range raw {
		s := &collector.Sample{Timestamp: base.Add(time.Duration(i) * time.Second)}
		if i == 0 || i >= 60 {
			value := 0.0
			if i == 0 {
				value = 100
			}
			s.Apps.Containers = []collector.ContainerStats{{ID: "stable-id", Name: "api", CPUPct: value}}
		}
		raw[i] = &AggregatedSample{Timestamp: s.Timestamp, Duration: time.Second, Data: s, AggregationVersion: currentAggregationVersion}
	}
	direct := store.aggregateAggregated(raw, 0)
	first := store.aggregateAggregated(raw[:60], 0)
	second := store.aggregateAggregated(raw[60:], 0)
	cascade := store.aggregateAggregated([]*AggregatedSample{roundTripMeanBucket(t, first), roundTripMeanBucket(t, second)}, 0)
	d := direct.Data.Apps.Containers[0].CPUPct
	c := cascade.Data.Apps.Containers[0].CPUPct
	t.Logf("direct mean = %.8f, cascading mean = %.8f; first duration=%s, second duration=%s, validity=%v", d, c, first.Duration, second.Duration, validHistoryAggregations([]*AggregatedSample{cascade}))
	if math.Abs(d-c) > 1e-9 {
		t.Fatalf("dynamic member receives full parent duration: direct %.8f != cascade %.8f", d, c)
	}
}

func TestAggregationCascadeWithUnavailableReadings(t *testing.T) {
	store := &Store{}
	base := time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
	raw := make([]*AggregatedSample, 120)
	for i := range raw {
		value := -1
		if i == 0 {
			value = 100
		}
		if i >= 60 {
			value = 0
		}
		s := &collector.Sample{Timestamp: base.Add(time.Duration(i) * time.Second), Apps: collector.ApplicationsStats{Mysql: &collector.MysqlStats{ReplicaSecondsBehind: value}}}
		raw[i] = &AggregatedSample{Timestamp: s.Timestamp, Duration: time.Second, Data: s, AggregationVersion: currentAggregationVersion}
	}
	direct := store.aggregateAggregated(raw, 0)
	cascade := store.aggregateAggregated([]*AggregatedSample{roundTripMeanBucket(t, store.aggregateAggregated(raw[:60], 0)), roundTripMeanBucket(t, store.aggregateAggregated(raw[60:], 0))}, 0)
	d := direct.Data.Apps.Mysql.ReplicaSecondsBehind
	c := cascade.Data.Apps.Mysql.ReplicaSecondsBehind
	t.Logf("direct lag mean = %d, cascading lag mean = %d", d, c)
	if d != c {
		t.Fatalf("filtered readings lose their weights: direct %d != cascade %d", d, c)
	}
}

func roundTripMeanBucket(t *testing.T, bucket *AggregatedSample) *AggregatedSample {
	t.Helper()
	encoded, err := encodeSample(bucket)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeSample(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !decoded.MeanWeightsComplete {
		t.Fatal("new bucket lost weight provenance")
	}
	return decoded
}

func TestAggregationCascadeOptionalPointersAndRounding(t *testing.T) {
	store := &Store{}
	base := time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
	raw := make([]*AggregatedSample, 6)
	for i := range raw {
		sample := &collector.Sample{Timestamp: base.Add(time.Duration(i) * time.Second)}
		if i == 0 || i >= 3 {
			value := 0.0
			if i == 0 || i == 5 {
				value = 1
			}
			sample.Apps.Nginx = &collector.NginxStats{ActiveConnections: int(value)}
			sample.Apps.Custom = map[string][]collector.CustomMetricValue{
				"fans{\"[]": {{Name: "front]\"", Value: value}},
			}
		}
		raw[i] = &AggregatedSample{Timestamp: sample.Timestamp, Duration: time.Second, Data: sample}
	}
	direct := store.aggregateAggregated(raw, 0)
	first := roundTripMeanBucket(t, store.aggregateAggregated(raw[:3], 0))
	second := roundTripMeanBucket(t, store.aggregateAggregated(raw[3:], 0))
	cascade := roundTripMeanBucket(t, store.aggregateAggregated([]*AggregatedSample{first, second}, 0))
	if direct.Data.Apps.Nginx.ActiveConnections != cascade.Data.Apps.Nginx.ActiveConnections {
		t.Fatalf("rounded pointer means: direct %d != cascade %d", direct.Data.Apps.Nginx.ActiveConnections, cascade.Data.Apps.Nginx.ActiveConnections)
	}
	for group, metrics := range direct.Data.Apps.Custom {
		if got := cascade.Data.Apps.Custom[group][0].Value; math.Abs(got-metrics[0].Value) > 1e-6 {
			t.Fatalf("nested identity mean: direct %v != cascade %v", metrics[0].Value, got)
		}
	}
	// A legacy bucket still has useful extrema, but cannot certify its means.
	first.MeanStats = nil
	first.MeanWeightsComplete = false
	mixed := store.aggregateAggregated([]*AggregatedSample{first, second}, 0)
	if mixed.MeanWeightsComplete || mixed.AggregationVersion != currentAggregationVersion {
		t.Fatal("legacy means must remain approximate without discarding valid extrema")
	}
	encoded, err := encodeSample(mixed)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeSample(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.MeanWeightsComplete {
		t.Fatal("codec promoted legacy weight provenance")
	}
}

func TestAggregationConsumesPersistedMeanPaths(t *testing.T) {
	base := time.Date(2026, 9, 5, 1, 0, 0, 0, time.UTC)
	first, second := aggregationFixture(base), aggregationFixture(base.Add(2*time.Second))
	first.CPU.Total.Usage, second.CPU.Total.Usage = 10, 20
	first.Apps.Nginx.ActiveConnections, second.Apps.Nginx.ActiveConnections = 11, 20
	first.Apps.Containers[0].CPUPct, second.Apps.Containers[0].CPUPct = 12, 20
	first.Disks.Devices[0].ID, second.Disks.Devices[0].ID = "stable", "stable"
	first.Disks.Devices[0].Sensors[0].Value, second.Disks.Devices[0].Sensors[0].Value = 30, 50
	first.Apps.Custom = map[string][]collector.CustomMetricValue{
		"alpha":    {{Name: "latency", Value: 40}, {Name: "rate", Value: 50}},
		`fans{"[]`: {{Name: `front]"`, Value: 60}},
	}
	second.Apps.Custom = map[string][]collector.CustomMetricValue{
		"alpha":    {{Name: "rate", Value: 100}, {Name: "latency", Value: 80}},
		`fans{"[]`: {{Name: `front]"`, Value: 120}},
	}
	// Literal keys from the existing storage contract: generating both the keys
	// and the consuming plan together could hide a backwards-incompatible change.
	stored := &AggregatedSample{
		Timestamp: base, Duration: 4 * time.Second, Data: first, Min: first, Max: first,
		AggregationVersion: currentAggregationVersion, MeanWeightsComplete: true,
		MeanStats: map[string]meanAccumulator{
			"sample.cpu.total.usage":                                           {Sum: 10, Weight: 1},
			"sample.apps.nginx.active_conn":                                    {Sum: 21, Weight: 2},
			`sample.apps.containers["ID=container-1"].cpu_pct`:                 {Sum: 24, Weight: 2},
			`sample.disk.devices["ID=stable"].sensors["Name=composite"].value`: {Sum: 30, Weight: 1},
			`sample.apps.custom{"alpha"}["Name=latency"].value`:                {Sum: 40, Weight: 1},
			`sample.apps.custom{"alpha"}["Name=rate"].value`:                   {Sum: 100, Weight: 2},
			`sample.apps.custom{"fans{\"[]"}["Name=front]\""].value`:           {Sum: 60, Weight: 1},
		},
	}
	result := (&Store{}).aggregateAggregated([]*AggregatedSample{
		roundTripMeanBucket(t, stored),
		{Timestamp: second.Timestamp, Duration: 2 * time.Second, Data: second},
	}, 0)
	for _, check := range []struct {
		name string
		got  float64
		want float64
	}{
		{"fixed field", result.Data.CPU.Total.Usage, 50.0 / 3},
		{"pointer integer", float64(result.Data.Apps.Nginx.ActiveConnections), 15},
		{"fallback identity", result.Data.Apps.Containers[0].CPUPct, 16},
		{"nested identity", result.Data.Disks.Devices[0].Sensors[0].Value, 130.0 / 3},
		{"map identity", aggregationCustomValue(result.Data.Apps.Custom, "alpha", "latency"), 200.0 / 3},
		{"reordered sibling", aggregationCustomValue(result.Data.Apps.Custom, "alpha", "rate"), 75},
		{"quoted identity", aggregationCustomValue(result.Data.Apps.Custom, `fans{"[]`, `front]"`), 100},
	} {
		if math.Abs(check.got-check.want) > 1e-9 {
			t.Errorf("%s mean = %v, want %v", check.name, check.got, check.want)
		}
	}
	if got := result.MeanStats["sample.apps.nginx.active_conn"]; got != (meanAccumulator{Sum: 61, Weight: 4}) {
		t.Fatalf("fractional integer statistics = %+v, want sum=61 weight=4", got)
	}
}
