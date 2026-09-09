package main

import (
	"math"
	"reflect"
	"testing"
	"time"

	"kula/internal/collector"
	"kula/internal/config"
	"kula/internal/storage"
)

func TestGeneratorIsDeterministic(t *testing.T) {
	opts := generatorOptions{
		Seed:         12345,
		Interval:     5 * time.Second,
		TotalSamples: 500,
		Profile:      profileRealistic,
	}
	first, err := newGenerator(opts)
	if err != nil {
		t.Fatalf("newGenerator(first): %v", err)
	}
	second, err := newGenerator(opts)
	if err != nil {
		t.Fatalf("newGenerator(second): %v", err)
	}
	start := time.Date(2026, time.September, 7, 0, 0, 0, 0, time.UTC)
	for i := 0; i < opts.TotalSamples; i++ {
		ts := start.Add(time.Duration(i) * opts.Interval)
		left := first.next(ts, i)
		right := second.next(ts, i)
		if !reflect.DeepEqual(left, right) {
			t.Fatalf("same seed diverged at sample %d", i)
		}
	}
}

func TestGeneratorMaintainsMetricInvariants(t *testing.T) {
	opts := generatorOptions{
		Seed:         9876,
		Interval:     10 * time.Second,
		TotalSamples: 2_400,
		Profile:      profileRealistic,
	}
	gen, err := newGenerator(opts)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, time.September, 7, 0, 0, 0, 0, time.UTC)
	var previous *collector.Sample
	sawNetworkReset := false
	sawNginxReset := false

	for i := 0; i < opts.TotalSamples; i++ {
		sample := gen.next(start.Add(time.Duration(i)*opts.Interval), i)
		assertFiniteFloats(t, reflect.ValueOf(sample), "sample")
		assertSampleInvariants(t, sample, i)

		if previous != nil {
			currentBytes := sample.Network.Interfaces[0].RxBytes
			previousBytes := previous.Network.Interfaces[0].RxBytes
			if currentBytes < previousBytes {
				if i != gen.events.rebootAt {
					t.Fatalf("network counter reset outside reboot at sample %d", i)
				}
				sawNetworkReset = true
			}
			if sample.Apps.Nginx != nil && previous.Apps.Nginx != nil && sample.Apps.Nginx.Requests < previous.Apps.Nginx.Requests {
				sawNginxReset = true
			}
		}
		previous = sample
	}
	if !sawNetworkReset {
		t.Error("realistic profile did not reset network counters on reboot")
	}
	// Nginx is intentionally absent for part of recovery, so compare the last
	// pre-reboot value with the first visible post-reboot value separately.
	if !sawNginxReset {
		gen, _ = newGenerator(opts)
		var before, after uint64
		for i := 0; i < opts.TotalSamples; i++ {
			sample := gen.next(start.Add(time.Duration(i)*opts.Interval), i)
			if sample.Apps.Nginx == nil {
				continue
			}
			if i < gen.events.rebootAt {
				before = sample.Apps.Nginx.Requests
			} else if after == 0 {
				after = sample.Apps.Nginx.Requests
			}
		}
		sawNginxReset = before > 0 && after < before
	}
	if !sawNginxReset {
		t.Error("realistic profile did not expose an application counter reset")
	}
}

func TestRealisticProfileExercisesOperationalIncidents(t *testing.T) {
	opts := generatorOptions{
		Seed:         42,
		Interval:     10 * time.Second,
		TotalSamples: 2_400,
		Profile:      profileRealistic,
	}
	gen, err := newGenerator(opts)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, time.September, 7, 0, 0, 0, 0, time.UTC)

	var maxCPU, maxDisk, maxSwap, maxRetrans, maxReplicationLag float64
	var lastUptime float64
	apiIDs := make(map[string]bool)
	sawAppsMissing := false
	sawCanary := false
	sawDischarge := false
	sawReboot := false
	for i := 0; i < opts.TotalSamples; i++ {
		sample := gen.next(start.Add(time.Duration(i)*opts.Interval), i)
		maxCPU = max(maxCPU, sample.CPU.Total.Usage)
		maxSwap = max(maxSwap, sample.Swap.UsedPercent)
		maxRetrans = max(maxRetrans, sample.Network.TCP.Retrans)
		for _, disk := range sample.Disks.Devices {
			maxDisk = max(maxDisk, disk.Utilization)
		}
		if sample.Apps.Nginx == nil {
			sawAppsMissing = true
		}
		if sample.Apps.Postgres != nil {
			maxReplicationLag = max(maxReplicationLag, sample.Apps.Postgres.ReplicationLagSeconds)
		}
		for _, container := range sample.Apps.Containers {
			if container.Name == "api-backend" {
				apiIDs[container.ID] = true
			}
			if container.Name == "api-backend-canary" {
				sawCanary = true
			}
		}
		for _, supply := range sample.PSU {
			if supply.Name == "ups0" && supply.Status == "Discharging" {
				sawDischarge = true
			}
		}
		if i > 0 && sample.System.Uptime < lastUptime {
			sawReboot = true
		}
		lastUptime = sample.System.Uptime
	}

	checks := map[string]bool{
		"CPU surge":             maxCPU > 65,
		"disk saturation":       maxDisk > 80,
		"swap pressure":         maxSwap > 5,
		"TCP retransmissions":   maxRetrans > 10,
		"database replica lag":  maxReplicationLag > 60,
		"application outage":    sawAppsMissing,
		"rolling canary":        sawCanary,
		"container replacement": len(apiIDs) == 2,
		"UPS discharge":         sawDischarge,
		"host reboot":           sawReboot,
	}
	for name, ok := range checks {
		if !ok {
			t.Errorf("profile did not exercise %s (cpu=%.2f disk=%.2f swap=%.2f retrans=%.2f lag=%.2f ids=%v)",
				name, maxCPU, maxDisk, maxSwap, maxRetrans, maxReplicationLag, apiIDs)
		}
	}
}

func TestSteadyProfileAvoidsFaultTransitions(t *testing.T) {
	opts := generatorOptions{Seed: 11, Interval: time.Minute, TotalSamples: 1_440, Profile: profileSteady}
	gen, err := newGenerator(opts)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, time.September, 7, 0, 0, 0, 0, time.UTC)
	lastUptime := 0.0
	for i := 0; i < opts.TotalSamples; i++ {
		sample := gen.next(start.Add(time.Duration(i)*opts.Interval), i)
		if sample.Apps.Nginx == nil {
			t.Fatalf("steady profile lost application metrics at sample %d", i)
		}
		if sample.System.Uptime <= lastUptime {
			t.Fatalf("steady profile uptime did not increase at sample %d", i)
		}
		if sample.Network.TCP.Retrans >= 10 {
			t.Fatalf("steady profile produced fault-level retransmissions at sample %d", i)
		}
		for _, supply := range sample.PSU {
			if supply.Status == "Discharging" || supply.Status == "Offline" {
				t.Fatalf("steady profile produced power failure at sample %d", i)
			}
		}
		lastUptime = sample.System.Uptime
	}
}

func TestWorkloadMetricsMoveTogether(t *testing.T) {
	opts := generatorOptions{Seed: 31415, Interval: time.Minute, TotalSamples: 1_440, Profile: profileSteady}
	gen, err := newGenerator(opts)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, time.September, 7, 0, 0, 0, 0, time.UTC)
	requests := make([]float64, 0, opts.TotalSamples)
	cpu := make([]float64, 0, opts.TotalSamples)
	network := make([]float64, 0, opts.TotalSamples)
	transactions := make([]float64, 0, opts.TotalSamples)
	for i := 0; i < opts.TotalSamples; i++ {
		ts := start.Add(time.Duration(i) * opts.Interval)
		sample := gen.next(ts, i)
		// Exclude the overnight backup window, where storage traffic is
		// intentionally independent of HTTP request volume.
		if ts.Hour() < 6 || ts.Hour() >= 21 {
			continue
		}
		requests = append(requests, sample.Apps.Nginx.RequestsPS)
		cpu = append(cpu, sample.CPU.Total.Usage)
		network = append(network, sample.Network.Interfaces[0].TxMbps)
		transactions = append(transactions, sample.Apps.Postgres.TxCommitPS)
	}
	if correlation(requests, cpu) < 0.75 {
		t.Fatalf("request rate and CPU are insufficiently correlated: %.3f", correlation(requests, cpu))
	}
	if correlation(requests, network) < 0.98 {
		t.Fatalf("request rate and network throughput are insufficiently correlated: %.3f", correlation(requests, network))
	}
	if correlation(requests, transactions) < 0.99 {
		t.Fatalf("request rate and database transactions are insufficiently correlated: %.3f", correlation(requests, transactions))
	}
}

func TestRatesMatchCumulativeCountersAtConfiguredInterval(t *testing.T) {
	opts := generatorOptions{Seed: 2718, Interval: 30 * time.Second, TotalSamples: 240, Profile: profileSteady}
	gen, err := newGenerator(opts)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, time.September, 7, 8, 0, 0, 0, time.UTC)
	previous := gen.next(start, 0)
	seconds := opts.Interval.Seconds()
	for i := 1; i < opts.TotalSamples; i++ {
		sample := gen.next(start.Add(time.Duration(i)*opts.Interval), i)
		for ifaceIndex := range sample.Network.Interfaces {
			currentIface := sample.Network.Interfaces[ifaceIndex]
			previousIface := previous.Network.Interfaces[ifaceIndex]
			actualMbps := float64(currentIface.RxBytes-previousIface.RxBytes) * 8 / seconds / 1_000_000
			if math.Abs(actualMbps-currentIface.RxMbps) > 0.011 {
				t.Fatalf("sample %d interface %s Rx rate %.3f does not match counter delta %.3f", i, currentIface.Name, currentIface.RxMbps, actualMbps)
			}
		}
		actualRequests := float64(sample.Apps.Nginx.Requests-previous.Apps.Nginx.Requests) / seconds
		if math.Abs(actualRequests-sample.Apps.Nginx.RequestsPS) > 0.04 {
			t.Fatalf("sample %d nginx rate %.3f does not match counter delta %.3f", i, sample.Apps.Nginx.RequestsPS, actualRequests)
		}
		previous = sample
	}
}

func TestConfiguredCustomMetricSchemaIsMirrored(t *testing.T) {
	opts := generatorOptions{
		Seed:         99,
		Interval:     time.Second,
		TotalSamples: 10,
		Profile:      profileSteady,
		CustomMetrics: map[string][]config.CustomMetricConfig{
			"hardware": {
				{Name: "cpu_temperature", Unit: "C", Max: 100},
				{Name: "case_fan_rpm", Unit: "rpm", Max: 5_000},
			},
			"service": {
				{Name: "cache_hit_ratio", Max: 1},
				{Name: "queue_backlog", Max: 40},
			},
		},
	}
	gen, err := newGenerator(opts)
	if err != nil {
		t.Fatal(err)
	}
	sample := gen.next(time.Date(2026, time.September, 7, 12, 0, 0, 0, time.UTC), 0)
	if len(sample.Apps.Custom) != len(opts.CustomMetrics) {
		t.Fatalf("custom groups = %v, want configured groups %v", sample.Apps.Custom, opts.CustomMetrics)
	}
	for group, definitions := range opts.CustomMetrics {
		metrics := sample.Apps.Custom[group]
		if len(metrics) != len(definitions) {
			t.Fatalf("group %q has %d metrics, want %d", group, len(metrics), len(definitions))
		}
		for i, metric := range metrics {
			if metric.Name != definitions[i].Name {
				t.Errorf("group %q metric %d name = %q, want %q", group, i, metric.Name, definitions[i].Name)
			}
			if metric.Value < 0 || metric.Value > definitions[i].Max {
				t.Errorf("group %q metric %q value %.2f exceeds configured range [0, %.2f]", group, metric.Name, metric.Value, definitions[i].Max)
			}
		}
	}
	if _, exists := sample.Apps.Custom["request_pipeline"]; exists {
		t.Error("built-in custom fixture was emitted despite an explicit custom schema")
	}
}

func TestGeneratedSampleSurvivesStorageRoundTrip(t *testing.T) {
	dir := t.TempDir()
	storageConfig := config.StorageConfig{
		Directory: dir,
		Tiers: []config.TierConfig{{
			Resolution: time.Second,
			MaxSize:    "2MB",
			MaxBytes:   2 * 1024 * 1024,
		}},
	}
	store, err := storage.NewStore(storageConfig)
	if err != nil {
		t.Fatal(err)
	}

	opts := generatorOptions{Seed: 77, Interval: time.Second, TotalSamples: 50, Profile: profileRealistic}
	gen, err := newGenerator(opts)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, time.September, 7, 12, 0, 0, 0, time.UTC)
	for i := 0; i < opts.TotalSamples; i++ {
		if err := store.WriteSample(gen.next(start.Add(time.Duration(i)*opts.Interval), i)); err != nil {
			t.Fatalf("WriteSample(%d): %v", i, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := storage.NewStore(storageConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	latest, err := reopened.QueryLatest()
	if err != nil {
		t.Fatal(err)
	}
	if latest == nil || latest.Data == nil {
		t.Fatal("reopened store returned no latest generated sample")
	}
	sample := latest.Data
	if sample.Disks.Devices[0].ID == "" {
		t.Error("disk identity was lost during storage round trip")
	}
	if len(sample.PSU) != 2 || sample.PSU[1].Name != "ups0" {
		t.Fatalf("power supplies were lost during storage round trip: %+v", sample.PSU)
	}
	if sample.Apps.Mysql == nil || sample.Apps.Mysql.IOState == "" {
		t.Fatalf("MySQL state was lost during storage round trip: %+v", sample.Apps.Mysql)
	}
	if len(sample.Apps.Custom) != 2 {
		t.Fatalf("custom metric groups were lost during storage round trip: %+v", sample.Apps.Custom)
	}
}

func TestGenerationHelpers(t *testing.T) {
	if _, err := generationDuration(0, 0); err == nil {
		t.Error("zero days should fail")
	}
	if got, err := generationDuration(99, 90*time.Minute); err != nil || got != 90*time.Minute {
		t.Fatalf("exact duration override = %v, %v", got, err)
	}
	if got, err := sampleCount(61*time.Second, 30*time.Second); err != nil || got != 3 {
		t.Fatalf("rounded sample count = %d, %v", got, err)
	}
	if _, err := parseProfile("unknown"); err == nil {
		t.Error("unknown profile should fail")
	}
	start, end, err := generationRange("2026-09-07T12:30:00+02:00", 3, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if got := end.Sub(start); got != 2*time.Minute {
		t.Fatalf("generated time range = %v, want 2m", got)
	}
}

func assertSampleInvariants(t *testing.T, sample *collector.Sample, index int) {
	t.Helper()
	components := sample.CPU.Total.User + sample.CPU.Total.System + sample.CPU.Total.IOWait +
		sample.CPU.Total.IRQ + sample.CPU.Total.SoftIRQ + sample.CPU.Total.Steal
	if math.Abs(components-sample.CPU.Total.Usage) > 0.011 || sample.CPU.Total.Usage < 0 || sample.CPU.Total.Usage > 100 {
		t.Errorf("sample %d has inconsistent CPU components: sum %.3f, usage %.3f", index, components, sample.CPU.Total.Usage)
	}
	if sample.Memory.Used+sample.Memory.Free+sample.Memory.Cached+sample.Memory.Buffers != sample.Memory.Total {
		t.Errorf("sample %d has inconsistent memory accounting: %+v", index, sample.Memory)
	}
	if sample.Memory.Available < sample.Memory.Free || sample.Memory.Available > sample.Memory.Total {
		t.Errorf("sample %d has invalid available memory: %+v", index, sample.Memory)
	}
	if math.Abs(sample.Memory.UsedPercent-percent(float64(sample.Memory.Used), float64(sample.Memory.Total))) > 0.011 {
		t.Errorf("sample %d has inconsistent memory percentage", index)
	}
	if sample.Swap.Used+sample.Swap.Free != sample.Swap.Total {
		t.Errorf("sample %d has inconsistent swap accounting: %+v", index, sample.Swap)
	}
	states := sample.Process.Running + sample.Process.Sleeping + sample.Process.Zombie + sample.Process.Blocked
	if states != sample.Process.Total {
		t.Errorf("sample %d process states sum to %d, total is %d", index, states, sample.Process.Total)
	}
	if sample.LoadAvg.Running != sample.Process.Running || sample.LoadAvg.Total != sample.Process.Total {
		t.Errorf("sample %d load process counts disagree with process metrics", index)
	}
	if len(sample.Network.Interfaces) != 2 {
		t.Errorf("sample %d has %d network interfaces", index, len(sample.Network.Interfaces))
	}
	for _, disk := range sample.Disks.Devices {
		if disk.ID == "" || disk.Utilization < 0 || disk.Utilization > 100 {
			t.Errorf("sample %d has invalid disk: %+v", index, disk)
		}
	}
	for _, fs := range sample.Disks.FileSystems {
		if fs.Used > fs.Total || fs.Available > fs.Total-fs.Used {
			t.Errorf("sample %d has invalid filesystem accounting: %+v", index, fs)
		}
	}
	for _, gpu := range sample.GPU {
		if gpu.LoadPct < 0 || gpu.LoadPct > 100 || gpu.VRAMUsed > gpu.VRAMTotal {
			t.Errorf("sample %d has invalid GPU values: %+v", index, gpu)
		}
	}
	for _, supply := range sample.PSU {
		if supply.Capacity < 0 || supply.Capacity > 100 {
			t.Errorf("sample %d has invalid PSU capacity: %+v", index, supply)
		}
	}
	for _, container := range sample.Apps.Containers {
		if container.MemUsed > container.MemLimit || container.MemPct < 0 || container.MemPct > 100 {
			t.Errorf("sample %d has invalid container memory: %+v", index, container)
		}
	}
	if nginx := sample.Apps.Nginx; nginx != nil && nginx.ActiveConnections != nginx.Reading+nginx.Writing+nginx.Waiting {
		t.Errorf("sample %d has inconsistent nginx connection states: %+v", index, nginx)
	}
}

func assertFiniteFloats(t *testing.T, value reflect.Value, path string) {
	t.Helper()
	if !value.IsValid() {
		return
	}
	switch value.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !value.IsNil() {
			assertFiniteFloats(t, value.Elem(), path)
		}
	case reflect.Struct:
		if value.Type() == reflect.TypeOf(time.Time{}) {
			return
		}
		for i := 0; i < value.NumField(); i++ {
			assertFiniteFloats(t, value.Field(i), path+"."+value.Type().Field(i).Name)
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < value.Len(); i++ {
			assertFiniteFloats(t, value.Index(i), path)
		}
	case reflect.Map:
		iter := value.MapRange()
		for iter.Next() {
			assertFiniteFloats(t, iter.Value(), path)
		}
	case reflect.Float32, reflect.Float64:
		v := value.Float()
		if math.IsNaN(v) || math.IsInf(v, 0) {
			t.Fatalf("%s contains non-finite value %v", path, v)
		}
	}
}

func correlation(left, right []float64) float64 {
	if len(left) != len(right) || len(left) == 0 {
		return 0
	}
	var leftMean, rightMean float64
	for i := range left {
		leftMean += left[i]
		rightMean += right[i]
	}
	leftMean /= float64(len(left))
	rightMean /= float64(len(right))

	var covariance, leftVariance, rightVariance float64
	for i := range left {
		leftDelta := left[i] - leftMean
		rightDelta := right[i] - rightMean
		covariance += leftDelta * rightDelta
		leftVariance += leftDelta * leftDelta
		rightVariance += rightDelta * rightDelta
	}
	if leftVariance == 0 || rightVariance == 0 {
		return 0
	}
	return covariance / math.Sqrt(leftVariance*rightVariance)
}
