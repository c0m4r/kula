package storage

import (
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"kula/internal/collector"
)

func TestAggregationPoliciesCoverSampleSchema(t *testing.T) {
	if issues := aggregationSchemaIssues(); len(issues) > 0 {
		t.Fatalf("collector.Sample aggregation policy is incomplete:\n  %s", strings.Join(issues, "\n  "))
	}
}

func TestMatchingValuesDoesNotMutateCaller(t *testing.T) {
	values := []weightedValue{
		{value: reflect.ValueOf(int64(10)), weight: 1},
		{value: reflect.ValueOf("metadata"), weight: 2},
		{value: reflect.ValueOf(int64(30)), weight: 3},
	}

	matched := matchingValues(values, reflect.TypeOf(int64(0)))
	if len(matched) != 2 || matched[0].value.Int() != 10 || matched[1].value.Int() != 30 {
		t.Fatalf("matchingValues result = %+v, want the two int64 values", matched)
	}
	if got := values[1].value.String(); got != "metadata" {
		t.Fatalf("matchingValues mutated caller backing storage: values[1] = %q", got)
	}
}

func TestAggregationPoliciesReduceEveryNumericField(t *testing.T) {
	store := newTestStore(t)
	defer func() { _ = store.Close() }()

	first := aggregationFixture(time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC))
	last := aggregationFixture(first.Timestamp.Add(time.Second))
	setAggregationFixtureNumbers(reflect.ValueOf(first), 10)
	setAggregationFixtureNumbers(reflect.ValueOf(last), 30)

	agg := store.aggregateSamples([]*collector.Sample{first, last}, 2*time.Second)
	if agg == nil || agg.Data == nil || agg.Min == nil || agg.Max == nil {
		t.Fatal("policy reducer returned an incomplete envelope")
	}
	if agg.AggregationVersion != currentAggregationVersion {
		t.Fatalf("aggregation version = %d, want %d", agg.AggregationVersion, currentAggregationVersion)
	}
	assertAggregationFixtureNumbers(t, reflect.ValueOf(agg.Data), reflect.ValueOf(agg.Min), reflect.ValueOf(agg.Max), "sample")
}

func TestAggregationConcurrentResultOwnership(t *testing.T) {
	for worker := range 8 {
		t.Run(fmtRes(time.Duration(worker+1)*time.Second), func(t *testing.T) {
			t.Parallel()
			store := &Store{}
			base := time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)
			raw := make([]*AggregatedSample, 6)
			for i := range raw {
				sample := aggregationFixture(base.Add(time.Duration(i) * time.Second))
				setAggregationFixtureNumbers(reflect.ValueOf(sample), float64(worker*10+i))
				if i%2 == 0 {
					sample.Apps.Nginx = nil
					sample.Apps.Custom = nil
				}
				raw[i] = &AggregatedSample{Timestamp: sample.Timestamp, Duration: time.Second, Data: sample}
			}
			original := cloneHistoryResult(&HistoryResult{Samples: raw})
			expected := store.aggregateAggregated(raw, 0)
			for range 10 {
				result := store.aggregateAggregated(raw, 0)
				if !reflect.DeepEqual(result, expected) {
					t.Fatal("concurrent reduction changed the result")
				}
				for _, sample := range []*collector.Sample{result.Data, result.Min, result.Max} {
					setAggregationFixtureNumbers(reflect.ValueOf(sample), 999)
				}
			}
			if !reflect.DeepEqual(raw, original.Samples) {
				t.Fatal("reducing or mutating a result changed the input samples")
			}
		})
	}
}

func TestAggregationUnionsDynamicIdentities(t *testing.T) {
	store := newTestStore(t)
	defer func() { _ = store.Close() }()

	base := time.Date(2026, 9, 3, 11, 0, 0, 0, time.UTC)
	first := &collector.Sample{
		Timestamp: base,
		Network: collector.NetworkStats{Interfaces: []collector.NetInterface{
			{Name: "eth0", RxMbps: 10},
			{Name: "eth1", RxMbps: 50},
		}},
		Apps: collector.ApplicationsStats{
			Containers: []collector.ContainerStats{
				{ID: "a", Name: "api", CPUPct: 20},
				{ID: "old-b", Name: "worker", CPUPct: 40},
			},
			Custom: map[string][]collector.CustomMetricValue{
				"fans": {{Name: "front", Value: 1000}},
			},
		},
		PSU: []collector.PowerSupplyStats{{Name: "BAT0", Capacity: 60}},
	}
	second := &collector.Sample{
		Timestamp: base.Add(time.Second),
		Network: collector.NetworkStats{Interfaces: []collector.NetInterface{
			{Name: "eth1", RxMbps: 70}, // reordered
			{Name: "eth2", RxMbps: 30}, // appeared
		}},
		Apps: collector.ApplicationsStats{
			Containers: []collector.ContainerStats{
				{ID: "new-b", Name: "worker", CPUPct: 60}, // recreated, same stable series name
			},
			Custom: map[string][]collector.CustomMetricValue{
				"fans": {{Name: "rear", Value: 2000}},
			},
		},
		PSU: []collector.PowerSupplyStats{{Name: "AC", Capacity: 100}},
	}

	agg := store.aggregateSamples([]*collector.Sample{first, second}, 2*time.Second)
	if agg == nil || agg.Data == nil || agg.Min == nil || agg.Max == nil {
		t.Fatal("policy reducer returned an incomplete envelope")
	}

	assertInterface := func(name string, data, min, max float64) {
		t.Helper()
		gotData, ok := findInterface(agg.Data.Network.Interfaces, name)
		if !ok {
			t.Fatalf("data is missing interface %q: %+v", name, agg.Data.Network.Interfaces)
		}
		gotMin, ok := findInterface(agg.Min.Network.Interfaces, name)
		if !ok {
			t.Fatalf("min is missing interface %q: %+v", name, agg.Min.Network.Interfaces)
		}
		gotMax, ok := findInterface(agg.Max.Network.Interfaces, name)
		if !ok {
			t.Fatalf("max is missing interface %q: %+v", name, agg.Max.Network.Interfaces)
		}
		if gotData.RxMbps != data || gotMin.RxMbps != min || gotMax.RxMbps != max {
			t.Errorf("%s rx_mbps = data/min/max %.1f/%.1f/%.1f, want %.1f/%.1f/%.1f",
				name, gotData.RxMbps, gotMin.RxMbps, gotMax.RxMbps, data, min, max)
		}
	}
	assertInterface("eth0", 10, 10, 10)
	assertInterface("eth1", 60, 50, 70)
	assertInterface("eth2", 30, 30, 30)

	containers := make(map[string]collector.ContainerStats)
	for _, container := range agg.Data.Apps.Containers {
		containers[container.Name] = container
	}
	if len(containers) != 2 || containers["api"].CPUPct != 20 || containers["worker"].CPUPct != 50 {
		t.Errorf("container identity union = %+v, want api=20 and worker=50", containers)
	}
	if containers["worker"].ID != "new-b" {
		t.Errorf("worker metadata ID = %q, want latest ID new-b", containers["worker"].ID)
	}

	if got := aggregationCustomValue(agg.Data.Apps.Custom, "fans", "front"); got != 1000 {
		t.Errorf("disappeared custom metric = %v, want 1000", got)
	}
	if got := aggregationCustomValue(agg.Data.Apps.Custom, "fans", "rear"); got != 2000 {
		t.Errorf("appeared custom metric = %v, want 2000", got)
	}
	if len(agg.Data.PSU) != 2 {
		t.Errorf("PSU identity union has %d entries, want 2: %+v", len(agg.Data.PSU), agg.Data.PSU)
	}
}

func TestAggregationWeightedCascadeMatchesDirectReduction(t *testing.T) {
	store := newTestStore(t)
	defer func() { _ = store.Close() }()

	base := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	values := []float64{10, 20, 30, 40}
	durations := []time.Duration{time.Second, time.Second, 2 * time.Second, 2 * time.Second}
	raw := make([]*AggregatedSample, 0, len(values))
	for i, value := range values {
		sample := &collector.Sample{
			Timestamp: base.Add(time.Duration(i) * time.Second),
			CPU:       collector.CPUStats{Total: collector.CPUCoreStats{Usage: value}},
			Network: collector.NetworkStats{Interfaces: []collector.NetInterface{{
				Name: "eth0", RxBytes: uint64([]int{100, 150, 10, 30}[i]),
			}}},
		}
		raw = append(raw, &AggregatedSample{
			Timestamp:          sample.Timestamp,
			Duration:           durations[i],
			Data:               sample,
			AggregationVersion: currentAggregationVersion,
		})
	}

	direct := store.aggregateAggregated(raw, 6*time.Second)
	firstBucket := store.aggregateAggregated(raw[:2], 2*time.Second)
	secondBucket := store.aggregateAggregated(raw[2:], 4*time.Second)
	cascade := store.aggregateAggregated([]*AggregatedSample{firstBucket, secondBucket}, 6*time.Second)

	if direct == nil || cascade == nil {
		t.Fatal("direct or cascading reduction returned nil")
	}
	if math.Abs(direct.Data.CPU.Total.Usage-cascade.Data.CPU.Total.Usage) > 1e-9 {
		t.Errorf("weighted cascade data = %v, direct = %v", cascade.Data.CPU.Total.Usage, direct.Data.CPU.Total.Usage)
	}
	if cascade.Min.CPU.Total.Usage != 10 || cascade.Max.CPU.Total.Usage != 40 {
		t.Errorf("cascade CPU extrema = %v/%v, want 10/40", cascade.Min.CPU.Total.Usage, cascade.Max.CPU.Total.Usage)
	}
	iface := cascade.Data.Network.Interfaces[0]
	if iface.RxBytes != 30 {
		t.Errorf("counter after reset = %d, want last value 30", iface.RxBytes)
	}
	if cascade.Min.Network.Interfaces[0].RxBytes != 10 || cascade.Max.Network.Interfaces[0].RxBytes != 150 {
		t.Errorf("counter extrema after reset = %d/%d, want 10/150",
			cascade.Min.Network.Interfaces[0].RxBytes, cascade.Max.Network.Interfaces[0].RxBytes)
	}
}

func TestAggregationValidityRejectsLegacyEnvelopes(t *testing.T) {
	base := time.Date(2026, 9, 3, 13, 0, 0, 0, time.UTC)
	data := makeSample(base)
	legacy := &AggregatedSample{
		Timestamp: base,
		Duration:  time.Minute,
		Data:      data,
		Min:       data,
		Max:       data,
	}
	current := *legacy
	current.AggregationVersion = currentAggregationVersion

	if got := validHistoryAggregations([]*AggregatedSample{legacy}); !reflect.DeepEqual(got, []string{"data"}) {
		t.Fatalf("legacy validity = %v, want [data]", got)
	}
	if got := validHistoryAggregations([]*AggregatedSample{&current}); !reflect.DeepEqual(got, []string{"data", "min", "max"}) {
		t.Fatalf("current validity = %v, want [data min max]", got)
	}
}

func TestQueryAdvertisesExtremaAfterTrustedReduction(t *testing.T) {
	store := newTestStore(t)
	defer func() { _ = store.Close() }()

	base := time.Date(2026, 9, 3, 14, 0, 0, 0, time.UTC)
	for i := 0; i < 6; i++ {
		if err := store.WriteSample(makeSampleWithCPU(base.Add(time.Duration(i)*time.Second), float64(i))); err != nil {
			t.Fatalf("WriteSample(%d): %v", i, err)
		}
	}
	result, err := store.QueryRangeWithMeta(base, base.Add(6*time.Second), 2)
	if err != nil {
		t.Fatalf("QueryRangeWithMeta: %v", err)
	}
	if got, want := result.ValidAggregations, []string{"data", "min", "max"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("valid aggregations = %v, want %v", got, want)
	}
	for i, sample := range result.Samples {
		if sample.AggregationVersion != currentAggregationVersion || sample.Min == nil || sample.Max == nil {
			t.Fatalf("sample %d has an untrusted envelope: %+v", i, sample)
		}
	}
}

func TestQueryDoesNotPromoteLegacyCoarseDataOnlyRecords(t *testing.T) {
	store := newMultiTierStore(t)
	defer func() { _ = store.Close() }()

	base := time.Date(2026, 9, 3, 14, 0, 0, 0, time.UTC)
	for i := 1; i <= 3; i++ {
		ts := base.Add(time.Duration(i) * time.Minute)
		if err := store.tiers[1].Write(&AggregatedSample{
			Timestamp: ts,
			Duration:  time.Minute,
			Data:      makeSampleWithCPU(ts, float64(i)),
		}); err != nil {
			t.Fatalf("write legacy tier-1 record: %v", err)
		}
	}

	result, err := store.QueryRangeWithMeta(base, base.Add(3*time.Minute), 1)
	if err != nil {
		t.Fatalf("QueryRangeWithMeta: %v", err)
	}
	if result.Tier != 1 || len(result.Samples) != 1 {
		t.Fatalf("tier/samples = %d/%d, want 1/1", result.Tier, len(result.Samples))
	}
	if result.Samples[0].AggregationVersion != 0 {
		t.Fatalf("legacy coarse reduction was promoted to version %d", result.Samples[0].AggregationVersion)
	}
	if got, want := result.ValidAggregations, []string{"data"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("valid aggregations = %v, want %v", got, want)
	}
}

func TestWriteSampleDoesNotTreatCollectionGapAsObservedDuration(t *testing.T) {
	store := newTestStore(t)
	defer func() { _ = store.Close() }()

	base := time.Date(2026, 9, 3, 15, 0, 0, 0, time.UTC)
	if err := store.WriteSample(makeSampleWithCPU(base, 10)); err != nil {
		t.Fatalf("first WriteSample: %v", err)
	}
	if err := store.WriteSample(makeSampleWithCPU(base.Add(time.Hour), 90)); err != nil {
		t.Fatalf("second WriteSample: %v", err)
	}

	records, err := store.tiers[0].ReadRange(base, base.Add(time.Hour))
	if err != nil {
		t.Fatalf("ReadRange: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("raw records = %d, want 2", len(records))
	}
	if records[1].Duration != time.Second {
		t.Fatalf("post-gap duration = %s, want the 1s collection interval", records[1].Duration)
	}
}

func aggregationFixture(ts time.Time) *collector.Sample {
	return &collector.Sample{
		Timestamp: ts,
		CPU: collector.CPUStats{
			Sensors: []collector.CPUTempSensor{{Name: "package"}},
		},
		Network: collector.NetworkStats{
			Interfaces: []collector.NetInterface{{Name: "eth0"}},
		},
		Disks: collector.DiskStats{
			Devices: []collector.DiskDevice{{
				Name:    "nvme0n1",
				Sensors: []collector.DiskTempSensor{{Name: "composite"}},
			}},
			FileSystems: []collector.FileSystemInfo{{MountPoint: "/"}},
		},
		GPU: []collector.GPUStats{{Index: 1, Name: "gpu0"}},
		PSU: []collector.PowerSupplyStats{{Name: "BAT0"}},
		Apps: collector.ApplicationsStats{
			Nginx:      &collector.NginxStats{},
			Apache2:    &collector.Apache2Stats{},
			Containers: []collector.ContainerStats{{ID: "container-1"}},
			Postgres:   &collector.PostgresStats{},
			Mysql:      &collector.MysqlStats{},
			Custom: map[string][]collector.CustomMetricValue{
				"group": {{Name: "metric"}},
			},
		},
	}
}

func setAggregationFixtureNumbers(value reflect.Value, number float64) {
	setAggregationFixtureValue(value, "", number)
}

func setAggregationFixtureValue(value reflect.Value, policy string, number float64) {
	if !value.IsValid() {
		return
	}
	if value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return
		}
		setAggregationFixtureValue(value.Elem(), policy, number)
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
				setAggregationFixtureValue(value.Field(i), typ.Field(i).Tag.Get("agg"), number)
			}
		}
	case reflect.Slice:
		for i := 0; i < value.Len(); i++ {
			setAggregationFixtureValue(value.Index(i), "", number)
		}
	case reflect.Map:
		iter := value.MapRange()
		for iter.Next() {
			copyValue := reflect.New(value.Type().Elem()).Elem()
			copyValue.Set(iter.Value())
			setAggregationFixtureValue(copyValue, "", number)
			value.SetMapIndex(iter.Key(), copyValue)
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if policy == aggIdentity {
			value.SetInt(1)
		} else if policy != "" {
			value.SetInt(int64(number))
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if policy == aggIdentity {
			value.SetUint(1)
		} else if policy != "" {
			value.SetUint(uint64(number))
		}
	case reflect.Float32, reflect.Float64:
		if policy == aggIdentity {
			value.SetFloat(1)
		} else if policy != "" {
			value.SetFloat(number)
		}
	}
}

func assertAggregationFixtureNumbers(t *testing.T, data, minValue, maxValue reflect.Value, path string) {
	t.Helper()
	if data.Kind() == reflect.Pointer {
		if data.IsNil() || minValue.IsNil() || maxValue.IsNil() {
			t.Fatalf("%s: reducer dropped a pointer", path)
		}
		assertAggregationFixtureNumbers(t, data.Elem(), minValue.Elem(), maxValue.Elem(), path)
		return
	}
	if data.Type() == timeValueType {
		return
	}

	switch data.Kind() {
	case reflect.Struct:
		typ := data.Type()
		for i := 0; i < data.NumField(); i++ {
			field := typ.Field(i)
			if !field.IsExported() {
				continue
			}
			childPath := path + "." + aggregationJSONName(field)
			policy := field.Tag.Get("agg")
			if isNumericKind(field.Type.Kind()) {
				wantData := 20.0
				switch policy {
				case aggLast:
					wantData = 30
				case aggIdentity:
					wantData = 1
				}
				wantMin, wantMax := 10.0, 30.0
				if policy == aggIdentity {
					wantMin, wantMax = 1, 1
				}
				assertNumericValue(t, data.Field(i), wantData, childPath+" data")
				assertNumericValue(t, minValue.Field(i), wantMin, childPath+" min")
				assertNumericValue(t, maxValue.Field(i), wantMax, childPath+" max")
				continue
			}
			assertAggregationFixtureNumbers(t, data.Field(i), minValue.Field(i), maxValue.Field(i), childPath)
		}
	case reflect.Slice:
		if data.Len() != minValue.Len() || data.Len() != maxValue.Len() {
			t.Fatalf("%s: envelope collection lengths differ: %d/%d/%d", path, data.Len(), minValue.Len(), maxValue.Len())
		}
		for i := 0; i < data.Len(); i++ {
			assertAggregationFixtureNumbers(t, data.Index(i), minValue.Index(i), maxValue.Index(i), path+"[]")
		}
	case reflect.Map:
		iter := data.MapRange()
		for iter.Next() {
			minEntry := minValue.MapIndex(iter.Key())
			maxEntry := maxValue.MapIndex(iter.Key())
			if !minEntry.IsValid() || !maxEntry.IsValid() {
				t.Fatalf("%s: envelope map is missing key %v", path, iter.Key())
			}
			assertAggregationFixtureNumbers(t, iter.Value(), minEntry, maxEntry, path+"{}")
		}
	}
}

func assertNumericValue(t *testing.T, value reflect.Value, want float64, label string) {
	t.Helper()
	got := numericAsFloat(value)
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s = %v, want %v", label, got, want)
	}
}

func findInterface(interfaces []collector.NetInterface, name string) (collector.NetInterface, bool) {
	for _, iface := range interfaces {
		if iface.Name == name {
			return iface, true
		}
	}
	return collector.NetInterface{}, false
}

func aggregationCustomValue(groups map[string][]collector.CustomMetricValue, group, name string) float64 {
	for _, metric := range groups[group] {
		if metric.Name == name {
			return metric.Value
		}
	}
	return math.NaN()
}
