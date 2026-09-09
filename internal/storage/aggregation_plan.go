package storage

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"kula/internal/collector"
)

// A reducer writes into an owned destination. Input values and their backing
// slice remain read-only, and reducers never retain them after returning. This
// lets a struct reuse its field-value buffer across all of its children.
type valueReducer func(dst reflect.Value, values []weightedValue, mode reductionMode, prefix string, means *meanReduction)

// Plans are immutable and shared across queries and rollups. All scratch space
// belongs to an individual call; no sample data is captured by these closures.
var sampleReducer = compileReducer(reflect.TypeFor[collector.Sample](), "", "sample")

// path is the static suffix since the last dynamic identity. Fixed fields have
// their complete paths compiled once; only map keys and slice identities need
// to extend prefix while reducing. These paths also identify on-disk MeanStats.
func compileReducer(typ reflect.Type, policy, path string) valueReducer {
	if typ == timeValueType {
		return reduceLastValue
	}

	switch typ.Kind() {
	case reflect.Pointer:
		elem := compileReducer(typ.Elem(), policy, path)
		return func(dst reflect.Value, values []weightedValue, mode reductionMode, prefix string, means *meanReduction) {
			elems := make([]weightedValue, 0, len(values))
			for _, value := range values {
				if !value.value.IsNil() {
					elems = append(elems, weightedValue{value: value.value.Elem(), weight: value.weight, means: value.means})
				}
			}
			if len(elems) == 0 {
				dst.SetZero()
				return
			}
			out := reflect.New(typ.Elem())
			elem(out.Elem(), elems, mode, prefix, means)
			dst.Set(out)
		}

	case reflect.Struct:
		type fieldReducer struct {
			index  int
			reduce valueReducer
		}
		fields := make([]fieldReducer, 0, typ.NumField())
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			if !field.IsExported() || aggregationJSONName(field) == "-" {
				continue
			}
			fields = append(fields, fieldReducer{
				index:  i,
				reduce: compileReducer(field.Type, field.Tag.Get("agg"), path+"."+aggregationJSONName(field)),
			})
		}
		return func(dst reflect.Value, values []weightedValue, mode reductionMode, prefix string, means *meanReduction) {
			fieldValues := make([]weightedValue, len(values))
			for _, field := range fields {
				for i, value := range values {
					fieldValues[i] = weightedValue{value: value.value.Field(field.index), weight: value.weight, means: value.means}
				}
				field.reduce(dst.Field(field.index), fieldValues, mode, prefix, means)
			}
		}

	case reflect.Slice:
		return compileSliceReducer(typ, path)

	case reflect.Map:
		return compileMapReducer(typ, path)

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		isMean := policy == aggMean || policy == aggMeanNonNegative
		nonNegative := policy == aggMeanNonNegative
		return func(dst reflect.Value, values []weightedValue, mode reductionMode, prefix string, means *meanReduction) {
			if mode == reduceData && isMean {
				numericMean(dst, values, nonNegative, prefix+path, means)
				return
			}
			reduceNumber(dst, values, mode, policy)
		}

	case reflect.String:
		return func(dst reflect.Value, values []weightedValue, _ reductionMode, _ string, _ *meanReduction) {
			// Retain the latest non-empty metadata, as in the schema reducer.
			for i := len(values) - 1; i >= 0; i-- {
				if values[i].value.String() != "" {
					dst.Set(values[i].value)
					return
				}
			}
			dst.Set(values[len(values)-1].value)
		}

	default:
		return reduceLastValue
	}
}

func reduceLastValue(dst reflect.Value, values []weightedValue, _ reductionMode, _ string, _ *meanReduction) {
	dst.Set(values[len(values)-1].value)
}

func compileSliceReducer(typ reflect.Type, path string) valueReducer {
	elem := compileReducer(typ.Elem(), "", "")
	identity := compileAggregationIdentity(typ.Elem())
	return func(dst reflect.Value, values []weightedValue, mode reductionMode, prefix string, means *meanReduction) {
		type group struct {
			key    string
			values []weightedValue
		}
		var groups []group
		groupIndex := make(map[string]int)
		for _, parent := range values {
			for i := 0; i < parent.value.Len(); i++ {
				item := parent.value.Index(i)
				key, ok := identity(item)
				if !ok {
					key = "#" + strconv.Itoa(i)
				}
				idx, exists := groupIndex[key]
				if !exists {
					idx = len(groups)
					groupIndex[key] = idx
					groups = append(groups, group{key: key})
				}
				groups[idx].values = append(groups[idx].values, weightedValue{value: item, weight: parent.weight, means: parent.means})
			}
		}
		if len(groups) == 0 {
			dst.SetZero()
			return
		}
		out := reflect.MakeSlice(typ, len(groups), len(groups))
		for i, group := range groups {
			childPrefix := ""
			if means != nil {
				childPrefix = prefix + path + "[" + strconv.Quote(group.key) + "]"
			}
			elem(out.Index(i), group.values, mode, childPrefix, means)
		}
		dst.Set(out)
	}
}

func compileMapReducer(typ reflect.Type, path string) valueReducer {
	elem := compileReducer(typ.Elem(), "", "")
	return func(dst reflect.Value, values []weightedValue, mode reductionMode, prefix string, means *meanReduction) {
		keySet := make(map[string]reflect.Value)
		for _, parent := range values {
			iter := parent.value.MapRange()
			for iter.Next() {
				keySet[fmt.Sprint(iter.Key().Interface())] = iter.Key()
			}
		}
		if len(keySet) == 0 {
			dst.SetZero()
			return
		}
		keys := make([]string, 0, len(keySet))
		for key := range keySet {
			keys = append(keys, key)
		}
		sort.Strings(keys)

		out := reflect.MakeMapWithSize(typ, len(keys))
		mapValues := make([]weightedValue, 0, len(values))
		item := reflect.New(typ.Elem()).Elem()
		for _, printableKey := range keys {
			key := keySet[printableKey]
			mapValues = mapValues[:0]
			for _, parent := range values {
				value := parent.value.MapIndex(key)
				if value.IsValid() {
					mapValues = append(mapValues, weightedValue{value: value, weight: parent.weight, means: parent.means})
				}
			}
			childPrefix := ""
			if means != nil {
				childPrefix = prefix + path + "{" + strconv.Quote(printableKey) + "}"
			}
			item.SetZero()
			elem(item, mapValues, mode, childPrefix, means)
			out.SetMapIndex(key, item)
		}
		dst.Set(out)
	}
}

// Identity field names and policies are schema metadata too. Compile only the
// relevant fields, preserving the exact keys used by persisted mean statistics.
func compileAggregationIdentity(typ reflect.Type) func(reflect.Value) (string, bool) {
	pointer := typ.Kind() == reflect.Pointer
	if pointer {
		typ = typ.Elem()
	}
	type identityField struct {
		index  int
		prefix string
	}
	var primary, fallback []identityField
	if typ.Kind() == reflect.Struct {
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			switch field.Tag.Get("agg") {
			case aggIdentity:
				primary = append(primary, identityField{index: i, prefix: field.Name + "="})
			case aggIdentityFallback:
				fallback = append(fallback, identityField{index: i, prefix: field.Name + "="})
			}
		}
	}
	key := func(value reflect.Value, fields []identityField) (string, bool) {
		var out strings.Builder
		nonZero := false
		for i, field := range fields {
			if i > 0 {
				out.WriteByte('\x1f')
			}
			part := value.Field(field.index)
			// Keep the builder out of fmt's interfaces so it stays on the stack,
			// including for the common string-only identity fields.
			var partText string
			if part.Kind() == reflect.String {
				partText = part.String()
			} else {
				partText = fmt.Sprint(part.Interface())
			}
			out.WriteString(field.prefix)
			out.WriteString(partText)
			nonZero = nonZero || !part.IsZero()
		}
		return out.String(), nonZero
	}
	return func(value reflect.Value) (string, bool) {
		if pointer {
			if value.IsNil() {
				return "", false
			}
			value = value.Elem()
		}
		if result, ok := key(value, primary); ok {
			return result, true
		}
		return key(value, fallback)
	}
}
