package storage

import (
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestCodecPersistsEveryAggregatedNumericField guards against a metric that is
// collected, aggregated and charted but never written to the positional codec:
// such a field shows values on the live tail and reads 0 for every stored
// record. TestAggregationPoliciesCoverSampleSchema only proves each field has a
// policy; this proves each one survives encode → decode in every block.
//
// Every numeric field carrying an agg policy gets its own value, so a decoder
// that swaps two fields fails as well as one that drops a field. Values stay in
// 2..91 so they survive the narrowest encodings (uint8, clamped percentages)
// and are exact in float32.
func TestCodecPersistsEveryAggregatedNumericField(t *testing.T) {
	ts := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	data := aggregationFixture(ts)
	minSample := aggregationFixture(ts)
	maxSample := aggregationFixture(ts)
	next := 0
	for _, sample := range []any{data, minSample, maxSample} {
		setDistinctCodecFixtureNumbers(reflect.ValueOf(sample), "", &next)
	}

	encoded, err := encodeSample(&AggregatedSample{
		Timestamp: ts, Duration: time.Minute,
		Data: data, Min: minSample, Max: maxSample,
		AggregationVersion: currentAggregationVersion,
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, err := decodeSample(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	var lost []string
	for _, block := range []struct {
		name      string
		want, got any
	}{
		{"data", data, decoded.Data},
		{"min", minSample, decoded.Min},
		{"max", maxSample, decoded.Max},
	} {
		compareCodecFixtureNumbers(reflect.ValueOf(block.want), reflect.ValueOf(block.got), block.name, &lost)
	}
	if len(lost) > 0 {
		t.Fatalf("numeric fields with an agg policy did not survive encode → decode:\n  %s", strings.Join(lost, "\n  "))
	}
}

func setDistinctCodecFixtureNumbers(value reflect.Value, policy string, next *int) {
	if !value.IsValid() {
		return
	}
	if value.Kind() == reflect.Pointer {
		if !value.IsNil() {
			setDistinctCodecFixtureNumbers(value.Elem(), policy, next)
		}
		return
	}
	if value.Type() == timeValueType {
		return
	}
	switch value.Kind() {
	case reflect.Struct:
		typ := value.Type()
		for i := 0; i < value.NumField(); i++ {
			if typ.Field(i).IsExported() {
				setDistinctCodecFixtureNumbers(value.Field(i), typ.Field(i).Tag.Get("agg"), next)
			}
		}
	case reflect.Slice:
		for i := 0; i < value.Len(); i++ {
			setDistinctCodecFixtureNumbers(value.Index(i), "", next)
		}
	case reflect.Map:
		iter := value.MapRange()
		for iter.Next() {
			entry := reflect.New(value.Type().Elem()).Elem()
			entry.Set(iter.Value())
			setDistinctCodecFixtureNumbers(entry, "", next)
			value.SetMapIndex(iter.Key(), entry)
		}
	default:
		if !isNumericKind(value.Kind()) || policy == "" || policy == aggIdentity || policy == aggIdentityFallback {
			return
		}
		*next++
		n := *next%90 + 2
		switch {
		case value.CanInt():
			value.SetInt(int64(n))
		case value.CanUint():
			value.SetUint(uint64(n))
		default:
			value.SetFloat(float64(n))
		}
	}
}

func compareCodecFixtureNumbers(want, got reflect.Value, path string, lost *[]string) {
	if want.Kind() == reflect.Pointer {
		if want.IsNil() {
			return
		}
		if got.IsNil() {
			*lost = append(*lost, path+": dropped")
			return
		}
		compareCodecFixtureNumbers(want.Elem(), got.Elem(), path, lost)
		return
	}
	if want.Type() == timeValueType {
		return
	}
	switch want.Kind() {
	case reflect.Struct:
		typ := want.Type()
		for i := 0; i < want.NumField(); i++ {
			field := typ.Field(i)
			if !field.IsExported() {
				continue
			}
			childPath := path + "." + aggregationJSONName(field)
			if isNumericKind(field.Type.Kind()) {
				policy := field.Tag.Get("agg")
				if policy == "" || policy == aggIdentity || policy == aggIdentityFallback {
					continue
				}
				w, g := codecFixtureNumber(want.Field(i)), codecFixtureNumber(got.Field(i))
				if math.Abs(w-g) > 1e-6*math.Max(1, math.Abs(w)) {
					*lost = append(*lost, childPath+": wrote "+formatCodecFixtureNumber(w)+", read "+formatCodecFixtureNumber(g))
				}
				continue
			}
			compareCodecFixtureNumbers(want.Field(i), got.Field(i), childPath, lost)
		}
	case reflect.Slice:
		if want.Len() != got.Len() {
			*lost = append(*lost, path+": length changed")
			return
		}
		for i := 0; i < want.Len(); i++ {
			compareCodecFixtureNumbers(want.Index(i), got.Index(i), path+"[]", lost)
		}
	case reflect.Map:
		iter := want.MapRange()
		for iter.Next() {
			entry := got.MapIndex(iter.Key())
			if !entry.IsValid() {
				*lost = append(*lost, path+": map entry dropped")
				continue
			}
			compareCodecFixtureNumbers(iter.Value(), entry, path+"{}", lost)
		}
	}
}

func codecFixtureNumber(v reflect.Value) float64 {
	switch {
	case v.CanInt():
		return float64(v.Int())
	case v.CanUint():
		return float64(v.Uint())
	default:
		return v.Float()
	}
}

func formatCodecFixtureNumber(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}
