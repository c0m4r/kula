package storage

import "reflect"

// Compile the copy plan once from the schema. Scalar-only structs use a single
// assignment; only pointers, slices, and maps need recursive allocation. This
// preserves full precision and nil values without a codec round trip, and new
// metric fields automatically participate in ownership isolation.
type valueCloner func(dst, src reflect.Value)

func compileCloner(typ reflect.Type) valueCloner {
	if typ == timeValueType {
		// time.Time's location is immutable and safe to share.
		return nil
	}
	switch typ.Kind() {
	case reflect.Struct:
		type fieldCopy struct {
			index int
			clone valueCloner
		}
		var fields []fieldCopy
		for i := 0; i < typ.NumField(); i++ {
			if clone := compileCloner(typ.Field(i).Type); clone != nil {
				fields = append(fields, fieldCopy{i, clone})
			}
		}
		if len(fields) == 0 {
			return nil
		}
		return func(dst, src reflect.Value) {
			for _, field := range fields {
				field.clone(dst.Field(field.index), src.Field(field.index))
			}
		}
	case reflect.Pointer:
		child := compileCloner(typ.Elem())
		return func(dst, src reflect.Value) {
			if src.IsNil() {
				return
			}
			cloned := reflect.New(typ.Elem())
			cloned.Elem().Set(src.Elem())
			if child != nil {
				child(cloned.Elem(), src.Elem())
			}
			dst.Set(cloned)
		}
	case reflect.Slice:
		child := compileCloner(typ.Elem())
		return func(dst, src reflect.Value) {
			if src.IsNil() {
				return
			}
			cloned := reflect.MakeSlice(typ, src.Len(), src.Len())
			reflect.Copy(cloned, src)
			if child != nil {
				for i := 0; i < src.Len(); i++ {
					child(cloned.Index(i), src.Index(i))
				}
			}
			dst.Set(cloned)
		}
	case reflect.Map:
		child := compileCloner(typ.Elem())
		return func(dst, src reflect.Value) {
			if src.IsNil() {
				return
			}
			clonedMap := reflect.MakeMapWithSize(typ, src.Len())
			iter := src.MapRange()
			for iter.Next() {
				value := iter.Value()
				if child != nil {
					cloned := reflect.New(typ.Elem()).Elem()
					cloned.Set(value)
					child(cloned, value)
					value = cloned
				}
				clonedMap.SetMapIndex(iter.Key(), value)
			}
			dst.Set(clonedMap)
		}
	}
	return nil
}

var aggregatedSampleCloner = compileCloner(reflect.TypeFor[AggregatedSample]())

func cloneAggregatedSample(sample *AggregatedSample) *AggregatedSample {
	if sample == nil {
		return nil
	}
	cloned := *sample
	aggregatedSampleCloner(reflect.ValueOf(&cloned).Elem(), reflect.ValueOf(sample).Elem())
	return &cloned
}
