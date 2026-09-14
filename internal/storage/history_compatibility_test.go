package storage

import (
	"encoding/binary"
	"reflect"
	"slices"
	"testing"
	"time"

	"kula/internal/collector"
)

// Emit the actual pre-policy binary layout, not a new writer's unversioned
// envelope. This exercises detection independently of query-only metadata.
func legacyHistoryPayload(t *testing.T, sample *AggregatedSample) []byte {
	t.Helper()
	payload := appendPreamble(nil, sample)
	flags := binary.LittleEndian.Uint16(payload[16:]) &^ (flagReducerV2 | flagHasMeanStats | flagHasDiskIDs)
	binary.LittleEndian.PutUint16(payload[16:], flags)
	for _, block := range []*collector.Sample{sample.Data, sample.Min, sample.Max} {
		if block == nil {
			continue
		}
		payload = appendFixed(payload, block)
		variable, err := appendVariable(nil, block)
		if err != nil {
			t.Fatal(err)
		}
		ids, err := appendDiskIDs(nil, block)
		if err != nil {
			t.Fatal(err)
		}
		payload = append(payload, variable[:len(variable)-len(ids)]...)
	}
	return payload
}

func legacyHistorySample(t *testing.T, ts time.Time) *AggregatedSample {
	t.Helper()
	sample := &AggregatedSample{Timestamp: ts, Duration: time.Minute,
		Data: makeSampleWithCPU(ts, 0), Min: makeSampleWithCPU(ts, 0.004), Max: makeSampleWithCPU(ts, 0.008)}
	sample.Data.Network.TCP.CurrEstab = 3
	sample.Min.Network.TCP.CurrEstab = 2
	sample.Max.Network.TCP.CurrEstab = 2
	decoded, err := decodeSample(legacyHistoryPayload(t, sample))
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}

func TestLegacyHistoryCompatibilityProfiles(t *testing.T) {
	base := time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)
	legacy := legacyHistorySample(t, base)
	if legacy.ExtremaProfile != "legacy" || legacy.AggregationVersion != 0 {
		t.Fatalf("legacy provenance: %+v", legacy)
	}
	result := &HistoryResult{Samples: []*AggregatedSample{legacy}}
	annotateHistoryAggregations(result)
	if !reflect.DeepEqual(result.ValidAggregations, []string{"data"}) || !reflect.DeepEqual(result.AvailableAggregations, []string{"data", "min", "max"}) {
		t.Fatalf("valid=%v available=%v", result.ValidAggregations, result.AvailableAggregations)
	}
	fields := result.ExtremaProfiles["legacy"]
	if !slices.Contains(fields, "cpu.total.usage") || slices.Contains(fields, "net.tcp.curr_estab") || slices.Contains(fields, "*") {
		t.Fatalf("unsafe compatibility fields: %v", fields)
	}
	cloned := cloneHistoryResult(result)
	cloned.ExtremaProfiles["legacy"][0] = "net.tcp.curr_estab"
	cloned.AvailableAggregations[0] = "max"
	if result.ExtremaProfiles["legacy"][0] != "cpu.total.usage" || result.AvailableAggregations[0] != "data" {
		t.Fatal("compatibility metadata shares cache ownership")
	}
	store := &Store{}
	reduced := store.aggregateAggregated([]*AggregatedSample{legacy, legacy}, 0)
	if reduced.ExtremaProfile != "legacy" || reduced.Min.CPU.Total.Usage != legacy.Min.CPU.Total.Usage {
		t.Fatal("rounded representative value contaminated legacy extrema")
	}
	current := store.aggregateSamples([]*collector.Sample{makeSampleWithCPU(base, 10), makeSampleWithCPU(base.Add(time.Second), 20)}, 2*time.Second)
	mixed := store.aggregateAggregated([]*AggregatedSample{reduced, current}, 0)
	if mixed.ExtremaProfile != "mixed" || mixed.AggregationVersion != 0 || mixed.Max.CPU.Total.Usage != 20 {
		t.Fatalf("mixed provenance/extrema lost: %+v", mixed)
	}
	// A writer's incomplete envelope must not become certified legacy history
	// when query metadata is discarded by the positional codec.
	encoded, err := encodeSample(mixed)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := decodeSample(encoded)
	if err != nil || reloaded.ExtremaProfile != "none" {
		t.Fatalf("incomplete modern record promoted: %v, %v", reloaded, err)
	}
}

func TestLegacyHistoryCompatibilityAcrossDecodeBatches(t *testing.T) {
	store := newMultiTierStore(t)
	t.Cleanup(func() { _ = store.Close() })
	base := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	tier := store.tiers[1]
	for i := 1; i <= historyDecodeBatchSize+2; i++ {
		ts := base.Add(time.Duration(i) * time.Minute)
		sample := legacyHistorySample(t, ts)
		payload := append([]byte{recordKindBinary}, legacyHistoryPayload(t, sample)...)
		record := binary.LittleEndian.AppendUint32(nil, uint32(len(payload)))
		record = append(record, payload...)
		if _, err := tier.file.WriteAt(record, headerSize+tier.writeOff); err != nil {
			t.Fatal(err)
		}
		tier.writeOff += int64(len(record))
		tier.count++
		if i == 1 {
			tier.oldestTS = ts
		}
		tier.newestTS = ts
	}
	if err := tier.writeHeader(); err != nil {
		t.Fatal(err)
	}
	for _, points := range []int{1, 1000} {
		result, err := store.QueryRangeWithMeta(base, tier.newestTS, points)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(result.AvailableAggregations, "min") || len(result.ValidAggregations) != 1 {
			t.Fatalf("points=%d: availability lost: %+v", points, result)
		}
		for _, sample := range result.Samples {
			if sample.ExtremaProfile != "legacy" || sample.Min.CPU.Total.Usage <= 0 {
				t.Fatalf("points=%d: cross-batch provenance or extrema lost", points)
			}
		}
	}
}

func TestLegacyDataOnlyHistoryRemainsUnavailableAcrossBatches(t *testing.T) {
	store := newMultiTierStore(t)
	t.Cleanup(func() { _ = store.Close() })
	base := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	for i := 1; i <= historyDecodeBatchSize+2; i++ {
		ts := base.Add(time.Duration(i) * time.Minute)
		if err := store.tiers[1].Write(&AggregatedSample{Timestamp: ts, Duration: time.Minute, Data: makeSampleWithCPU(ts, float64(i))}); err != nil {
			t.Fatal(err)
		}
	}
	result, err := store.QueryRangeWithMeta(base, base.Add((historyDecodeBatchSize+2)*time.Minute), 1)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.AvailableAggregations, []string{"data"}) || result.Samples[0].ExtremaProfile != "none" {
		t.Fatalf("Data-only coarse history was promoted: %+v", result)
	}
}
