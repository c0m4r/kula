package storage

import (
	"math"
	"reflect"
	"sort"
	"strings"
	"time"

	"kula/internal/collector"
)

// Aggregation policies live next to the collector schema as `agg` struct tags.
// This keeps a new numeric metric from silently inheriting an arbitrary
// reducer. The storage tests walk the complete Sample graph and fail when a
// numeric field has no policy or a dynamic collection has no stable identity.
const (
	aggMean             = "mean"
	aggMeanNonNegative  = "mean_nonnegative"
	aggLast             = "last"
	aggIdentity         = "identity"
	aggIdentityFallback = "identity_fallback"
)

const currentAggregationVersion = uint8(2)

type reductionMode uint8

const (
	reduceData reductionMode = iota
	reduceMin
	reduceMax
)

type weightedValue struct {
	value  reflect.Value
	weight float64
	means  map[string]meanAccumulator
}

// Sparse sufficient statistics preserve missing-member weights and fractional
// integer means across rollups. Fully observed, unrounded means need no entry.
type meanAccumulator struct {
	Sum    float64
	Weight float64
}

type meanReduction struct {
	weight float64
	stats  map[string]meanAccumulator
}

var timeValueType = reflect.TypeOf(time.Time{})

// aggregateSamples is retained for direct raw-reducer tests. Production writes
// pass the tier-0 AggregatedSample wrappers to aggregateAggregated so their
// duration and provenance remain available.
func (s *Store) aggregateSamples(samples []*collector.Sample, dur time.Duration) *AggregatedSample {
	if len(samples) == 0 {
		return nil
	}

	perSample := time.Duration(0)
	if dur > 0 {
		perSample = dur / time.Duration(len(samples))
	}
	wrapped := make([]*AggregatedSample, 0, len(samples))
	for _, sample := range samples {
		if sample == nil {
			continue
		}
		wrapped = append(wrapped, &AggregatedSample{
			Timestamp:          sample.Timestamp,
			Duration:           perSample,
			Data:               sample,
			AggregationVersion: currentAggregationVersion,
		})
	}
	return s.aggregateAggregated(wrapped, dur)
}

// aggregateAggregated reduces raw samples or existing buckets using the
// explicit policies on collector.Sample. Means are duration-weighted when the
// inputs are already buckets; sparse statistics retain each field's effective
// weight and unrounded sum when its contribution differs. Extrema
// use the inner envelopes when they are trustworthy and always include Data as
// a fallback for dynamic identities missing from a legacy envelope.
func (s *Store) aggregateAggregated(samples []*AggregatedSample, dur time.Duration) *AggregatedSample {
	dataValues := make([]weightedValue, 0, len(samples))
	minValues := make([]weightedValue, 0, len(samples)*2)
	maxValues := make([]weightedValue, 0, len(samples)*2)
	trustedExtrema := true
	completeWeights := true
	var timestamp time.Time
	var summedDuration time.Duration

	for _, sample := range samples {
		if sample == nil || sample.Data == nil {
			continue
		}

		weight := 1.0
		if sample.Duration > 0 {
			weight = sample.Duration.Seconds()
		}
		isRaw := sample.Min == nil && sample.Max == nil
		data := reflect.ValueOf(sample.Data).Elem()
		dataValues = append(dataValues, weightedValue{value: data, weight: weight, means: sample.MeanStats})
		if !isRaw && !sample.MeanWeightsComplete {
			completeWeights = false
		}

		// Data is redundant for complete envelopes, but including it is useful
		// for old records whose dynamic Min/Max slices omitted an identity. It
		// cannot change a correct numeric extremum.
		if sample.Min != nil {
			minValues = append(minValues, weightedValue{value: reflect.ValueOf(sample.Min).Elem(), weight: 1})
		}
		minValues = append(minValues, weightedValue{value: data, weight: 1})
		if sample.Max != nil {
			maxValues = append(maxValues, weightedValue{value: reflect.ValueOf(sample.Max).Elem(), weight: 1})
		}
		maxValues = append(maxValues, weightedValue{value: data, weight: 1})

		if !isRaw && sample.AggregationVersion < currentAggregationVersion {
			trustedExtrema = false
		}
		timestamp = sample.Timestamp
		if sample.Duration > 0 {
			summedDuration += sample.Duration
		}
	}

	if len(dataValues) == 0 {
		return nil
	}
	if summedDuration > 0 {
		dur = summedDuration
	}

	weight := dur.Seconds()
	if weight <= 0 {
		weight = 1
	}
	means := &meanReduction{weight: weight, stats: make(map[string]meanAccumulator)}
	data := reduceSample(dataValues, reduceData, means)
	minSample := reduceSample(minValues, reduceMin, nil)
	maxSample := reduceSample(maxValues, reduceMax, nil)
	version := uint8(0)
	if trustedExtrema {
		version = currentAggregationVersion
	}

	return &AggregatedSample{
		Timestamp:           timestamp,
		Duration:            dur,
		Data:                data,
		Min:                 minSample,
		Max:                 maxSample,
		AggregationVersion:  version,
		MeanStats:           means.stats,
		MeanWeightsComplete: completeWeights,
	}
}

func reduceSample(values []weightedValue, mode reductionMode, means *meanReduction) *collector.Sample {
	values = matchingValues(values, reflect.TypeFor[collector.Sample]())
	if len(values) == 0 {
		return nil
	}
	sample := &collector.Sample{}
	sampleReducer(reflect.ValueOf(sample).Elem(), values, mode, "", means)
	return sample
}

func matchingValues(values []weightedValue, typ reflect.Type) []weightedValue {
	// Filtering must not modify caller-owned buffers shared across reductions.
	// Child plans already know their field types; only the root needs this check.
	matchCount := 0
	for _, value := range values {
		if value.value.IsValid() && value.value.Type() == typ {
			matchCount++
		}
	}
	if matchCount == len(values) {
		return values
	}
	if matchCount == 0 {
		return nil
	}

	matched := make([]weightedValue, 0, matchCount)
	for _, value := range values {
		if value.value.IsValid() && value.value.Type() == typ {
			matched = append(matched, value)
		}
	}
	return matched
}

func reduceNumber(out reflect.Value, values []weightedValue, mode reductionMode, policy string) {
	if policy == aggIdentity || policy == aggIdentityFallback || mode == reduceData {
		out.Set(values[len(values)-1].value)
		return
	}

	var best reflect.Value
	found := false
	for _, value := range values {
		if !numericUsable(value.value, policy == aggMeanNonNegative) {
			continue
		}
		if !found || numericLess(value.value, best) == (mode == reduceMin) {
			best = value.value
			found = true
		}
	}
	if !found {
		best = values[len(values)-1].value
	}
	out.Set(best)
}

func numericMean(out reflect.Value, values []weightedValue, nonNegative bool, path string, means *meanReduction) {
	var sum, weights float64
	for _, value := range values {
		if stat, ok := value.means[path]; ok {
			sum += stat.Sum
			weights += stat.Weight
			continue
		}
		if nonNegative && numericNegative(value.value) {
			continue
		}
		weight := value.weight
		if weight <= 0 || math.IsNaN(weight) || math.IsInf(weight, 0) {
			weight = 1
		}
		number := numericAsFloat(value.value)
		if math.IsNaN(number) || math.IsInf(number, 0) {
			continue
		}
		sum += number * weight
		weights += weight
	}
	if weights == 0 {
		means.stats[path] = meanAccumulator{}
		out.Set(values[len(values)-1].value)
		return
	}

	mean := sum / weights
	switch out.Kind() {
	case reflect.Float32, reflect.Float64:
		out.SetFloat(mean)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		out.SetInt(int64(math.Round(mean)))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		out.SetUint(uint64(math.Round(mean)))
	}
	if weights != means.weight || numericAsFloat(out) != mean {
		means.stats[path] = meanAccumulator{Sum: sum, Weight: weights}
	}
}

func numericAsFloat(value reflect.Value) float64 {
	switch value.Kind() {
	case reflect.Float32, reflect.Float64:
		return value.Float()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return float64(value.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return float64(value.Uint())
	default:
		return 0
	}
}

func numericNegative(value reflect.Value) bool {
	switch value.Kind() {
	case reflect.Float32, reflect.Float64:
		return value.Float() < 0
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return value.Int() < 0
	default:
		return false
	}
}

func numericUsable(value reflect.Value, nonNegative bool) bool {
	if nonNegative && numericNegative(value) {
		return false
	}
	if value.Kind() == reflect.Float32 || value.Kind() == reflect.Float64 {
		number := value.Float()
		return !math.IsNaN(number) && !math.IsInf(number, 0)
	}
	return true
}

func numericLess(a, b reflect.Value) bool {
	switch a.Kind() {
	case reflect.Float32, reflect.Float64:
		return a.Float() < b.Float()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return a.Int() < b.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return a.Uint() < b.Uint()
	default:
		return false
	}
}

func aggregationJSONName(field reflect.StructField) string {
	name := strings.Split(field.Tag.Get("json"), ",")[0]
	if name == "" {
		return field.Name
	}
	return name
}

// aggregationSchemaIssues returns every missing or invalid policy in the
// collector.Sample graph. It is deliberately available to package tests rather
// than enforced with an init-time panic.
func aggregationSchemaIssues() []string {
	issues := make([]string, 0)
	validateAggregationType(reflect.TypeOf(collector.Sample{}), "sample", &issues)
	sort.Strings(issues)
	return issues
}

func validateAggregationType(typ reflect.Type, path string, issues *[]string) {
	if typ == timeValueType {
		return
	}
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}

	switch typ.Kind() {
	case reflect.Struct:
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			if !field.IsExported() || aggregationJSONName(field) == "-" {
				continue
			}
			childPath := path + "." + aggregationJSONName(field)
			policy := field.Tag.Get("agg")
			if policy != "" && policy != aggMean && policy != aggMeanNonNegative && policy != aggLast &&
				policy != aggIdentity && policy != aggIdentityFallback {
				*issues = append(*issues, childPath+": unknown agg policy "+policy)
			}
			if isNumericKind(field.Type.Kind()) && policy == "" {
				*issues = append(*issues, childPath+": numeric field has no agg policy")
			}
			validateAggregationType(field.Type, childPath, issues)
		}

	case reflect.Slice:
		elem := typ.Elem()
		for elem.Kind() == reflect.Pointer {
			elem = elem.Elem()
		}
		if elem.Kind() == reflect.Struct && !hasAggregationIdentity(elem) {
			*issues = append(*issues, path+": dynamic collection has no agg identity")
		}
		validateAggregationType(typ.Elem(), path+"[]", issues)

	case reflect.Map:
		validateAggregationType(typ.Elem(), path+"{}", issues)
	}
}

func hasAggregationIdentity(typ reflect.Type) bool {
	for i := 0; i < typ.NumField(); i++ {
		policy := typ.Field(i).Tag.Get("agg")
		if policy == aggIdentity || policy == aggIdentityFallback {
			return true
		}
	}
	return false
}

func isNumericKind(kind reflect.Kind) bool {
	return kind >= reflect.Int && kind <= reflect.Float64 && kind != reflect.Uintptr
}
