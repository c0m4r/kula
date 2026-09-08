package web

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"kula/internal/collector"
	"kula/internal/storage"
)

func historyPayloadFixture(appHeavy bool) *storage.HistoryResult {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	sample := &collector.Sample{
		Timestamp: base,
		CPU: collector.CPUStats{
			Total:    collector.CPUCoreStats{User: 20, System: 10, Usage: 30},
			NumCores: 8,
		},
		LoadAvg: collector.LoadAvg{Load1: 1.2, Load5: 1.1, Load15: 1},
		Memory:  collector.MemoryStats{Total: 32 << 30, Used: 16 << 30, UsedPercent: 50},
		Swap:    collector.SwapStats{Total: 4 << 30, Used: 1 << 30, UsedPercent: 25},
		System:  collector.SystemStats{Hostname: "benchmark-host", Uptime: 86400, ClockSync: true},
		Process: collector.ProcessStats{Total: 320, Running: 4, Sleeping: 310, Threads: 950},
		Self:    collector.SelfStats{CPUPercent: 0.5, MemRSS: 50 << 20, FDs: 20},
	}
	for i := 0; i < 8; i++ {
		sample.Network.Interfaces = append(sample.Network.Interfaces, collector.NetInterface{
			Name: fmt.Sprintf("eth%d", i), RxMbps: float64(i + 1), TxMbps: float64(i) / 2,
		})
	}

	if appHeavy {
		for i := 0; i < 12; i++ {
			sample.Disks.Devices = append(sample.Disks.Devices, collector.DiskDevice{
				Name: fmt.Sprintf("nvme%dn1", i), ReadBytesPS: float64(i+1) * 1e6, WriteBytesPS: float64(i+1) * 2e6,
			})
			sample.Disks.FileSystems = append(sample.Disks.FileSystems, collector.FileSystemInfo{
				Device: fmt.Sprintf("/dev/nvme%dn1p1", i), MountPoint: fmt.Sprintf("/srv/data-%02d", i),
				Total: 2 << 40, Used: 1 << 40, UsedPct: 50,
			})
		}
		for i := 0; i < 4; i++ {
			sample.GPU = append(sample.GPU, collector.GPUStats{
				Index: i, Name: fmt.Sprintf("GPU %d", i), Driver: "nvidia", LoadPct: 60, VRAMUsed: 8 << 30, VRAMTotal: 16 << 30,
			})
		}
		for i := 0; i < 40; i++ {
			sample.Apps.Containers = append(sample.Apps.Containers, collector.ContainerStats{
				ID: fmt.Sprintf("%064x", i), Name: fmt.Sprintf("service-%02d", i), CPUPct: 5,
				MemUsed: 256 << 20, MemLimit: 1 << 30, NetRxBPS: 1000, NetTxBPS: 2000,
			})
		}
		sample.Apps.Custom = make(map[string][]collector.CustomMetricValue)
		for group := 0; group < 10; group++ {
			for metric := 0; metric < 5; metric++ {
				name := fmt.Sprintf("metric_%02d", metric)
				sample.Apps.Custom[fmt.Sprintf("group_%02d", group)] = append(
					sample.Apps.Custom[fmt.Sprintf("group_%02d", group)],
					collector.CustomMetricValue{Name: name, Value: float64(group * metric)},
				)
			}
		}
	}

	samples := make([]*storage.AggregatedSample, 120)
	for i := range samples {
		ts := base.Add(time.Duration(i) * time.Minute)
		copySample := *sample
		copySample.Timestamp = ts
		samples[i] = &storage.AggregatedSample{
			Timestamp:   ts,
			Duration:    time.Minute,
			Data:        &copySample,
			Min:         &copySample,
			Max:         &copySample,
			BucketStart: ts.Add(-time.Minute),
			BucketEnd:   ts,
			SampleCount: 60,
			Coverage:    1,
		}
	}
	from, to := base, base.Add(2*time.Hour)
	return &storage.HistoryResult{
		Samples: samples, Tier: 1, Resolution: "1m", SourceResolution: "1m", RequestedFrom: from, RequestedTo: to,
		ActualFrom: &from, ActualTo: &to, Complete: true,
		ValidAggregations: []string{"data", "min", "max"},
	}
}

func countJSONNodes(value any) int {
	count := 1
	switch typed := value.(type) {
	case map[string]any:
		for _, child := range typed {
			count += countJSONNodes(child)
		}
	case []any:
		for _, child := range typed {
			count += countJSONNodes(child)
		}
	}
	return count
}

func encodedPayloadStats(t *testing.T, payload any) (bytes, nodes int) {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var parsed any
	if err := json.Unmarshal(encoded, &parsed); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	return len(encoded), countJSONNodes(parsed)
}

func TestHistorySectionPayloadReduction(t *testing.T) {
	sections, ordered, err := parseHistorySections("cpu,lavg,mem,swap,net,sys")
	if err != nil {
		t.Fatal(err)
	}

	for _, profile := range []struct {
		name     string
		appHeavy bool
	}{
		{name: "bare"},
		{name: "application-heavy", appHeavy: true},
	} {
		t.Run(profile.name, func(t *testing.T) {
			full := historyPayloadFixture(profile.appHeavy)
			selected := selectHistorySections(full, sections, ordered)
			fullBytes, fullNodes := encodedPayloadStats(t, full)
			selectedBytes, selectedNodes := encodedPayloadStats(t, selected)
			if selectedBytes >= fullBytes || selectedNodes >= fullNodes {
				t.Fatalf("section selection did not reduce payload: bytes %d/%d nodes %d/%d",
					selectedBytes, fullBytes, selectedNodes, fullNodes)
			}
			t.Logf("full=%dB/%d JSON nodes selected=%dB/%d nodes reduction=%.1fx/%.1fx",
				fullBytes, fullNodes, selectedBytes, selectedNodes,
				float64(fullBytes)/float64(selectedBytes), float64(fullNodes)/float64(selectedNodes))
		})
	}
}

func TestHistorySectionsPreserveResponseMetadata(t *testing.T) {
	full := historyPayloadFixture(false)
	full.Downsampled = true
	full.Resolution = "2m"
	full.ExactComplete = false
	sections, ordered, err := parseHistorySections("cpu")
	if err != nil {
		t.Fatal(err)
	}
	selected := selectHistorySections(full, sections, ordered)
	decodeMetadata := func(value any) map[string]any {
		t.Helper()
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var metadata map[string]any
		if err := json.Unmarshal(encoded, &metadata); err != nil {
			t.Fatal(err)
		}
		delete(metadata, "samples")
		delete(metadata, "sections")
		return metadata
	}
	if !reflect.DeepEqual(decodeMetadata(full), decodeMetadata(selected)) {
		t.Fatal("section selection changed history response metadata")
	}
}

func BenchmarkHistoryPayloadEncoding(b *testing.B) {
	sections, ordered, err := parseHistorySections("cpu,lavg,mem,swap,net,sys")
	if err != nil {
		b.Fatal(err)
	}
	full := historyPayloadFixture(true)
	selected := selectHistorySections(full, sections, ordered)

	for _, benchmark := range []struct {
		name    string
		payload any
	}{
		{name: "full-application-heavy", payload: full},
		{name: "selected-application-heavy", payload: selected},
	} {
		b.Run(benchmark.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := json.Marshal(benchmark.payload); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
