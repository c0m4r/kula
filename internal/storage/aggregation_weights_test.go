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
