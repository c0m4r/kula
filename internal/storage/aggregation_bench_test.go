package storage

import (
	"reflect"
	"testing"
	"time"

	"kula/internal/collector"
)

// BenchmarkAggregation isolates the reducer from file writes and query caching.
func BenchmarkAggregation(b *testing.B) {
	for _, fixture := range []struct {
		name   string
		sample func(time.Time) *collector.Sample
	}{
		{"Minimal", makeSample},
		{"AllSections", aggregationFixture},
	} {
		b.Run(fixture.name, func(b *testing.B) {
			store := &Store{}
			base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			raw := make([]*AggregatedSample, 300)
			for i := range raw {
				ts := base.Add(time.Duration(i) * time.Second)
				sample := fixture.sample(ts)
				setAggregationFixtureNumbers(reflect.ValueOf(sample), float64(i%17))
				raw[i] = &AggregatedSample{
					Timestamp:          ts,
					Duration:           time.Second,
					Data:               sample,
					AggregationVersion: currentAggregationVersion,
				}
			}
			cascade := make([]*AggregatedSample, 5)
			for i := range cascade {
				cascade[i] = store.aggregateAggregated(raw[i*60:(i+1)*60], 0)
			}
			for _, input := range []struct {
				name    string
				samples []*AggregatedSample
			}{
				{"Raw60", raw[:60]},
				{"Cascade5", cascade},
			} {
				b.Run(input.name, func(b *testing.B) {
					b.ReportAllocs()
					for i := 0; i < b.N; i++ {
						result := store.aggregateAggregated(input.samples, 0)
						if result == nil || result.Data == nil || result.Min == nil || result.Max == nil {
							b.Fatal("incomplete aggregate")
						}
					}
				})
			}
		})
	}
}
