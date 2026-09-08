package storage

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"kula/internal/collector"
	"kula/internal/config"
)

func TestDiskIdentityCodecCompatibility(t *testing.T) {
	a := makeSampleFull(time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC))
	a.Data.Disks.Devices = []collector.DiskDevice{
		{ID: "wwid:eui.0011223344556677", Name: "sda", ReadBytesPS: 123},
		{ID: "serial:" + strings.Repeat("long", 100), Name: "sda", ReadBytesPS: 456},
		{Name: "sdc", ReadBytesPS: 789},
	}
	a.Min = cloneAggregatedSample(a).Data
	a.Max = cloneAggregatedSample(a).Data
	a.Min.Disks.Devices[0], a.Min.Disks.Devices[1] = a.Min.Disks.Devices[1], a.Min.Disks.Devices[0]
	a.MeanStats = map[string]meanAccumulator{`sample.disk.devices["ID=wwid:eui.0011223344556677"].read_bps`: {Sum: 123, Weight: 1}}
	a.MeanWeightsComplete = true
	payload, err := encodeSample(a)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeSample(payload)
	if err != nil {
		t.Fatal(err)
	}
	for i, block := range []*collector.Sample{decoded.Data, decoded.Min, decoded.Max} {
		want := []*collector.Sample{a.Data, a.Min, a.Max}[i]
		if !reflect.DeepEqual(block.Disks.Devices, want.Disks.Devices) {
			// The codec intentionally normalizes nil sensor slices to empty.
			for j := range block.Disks.Devices {
				if block.Disks.Devices[j].ID != want.Disks.Devices[j].ID || block.Disks.Devices[j].ReadBytesPS != want.Disks.Devices[j].ReadBytesPS {
					t.Fatalf("block %d disk identity alignment: %+v", i, block.Disks.Devices)
				}
			}
		}
	}
	if !reflect.DeepEqual(decoded.MeanStats, a.MeanStats) {
		t.Fatal("disk extension damaged contributing statistics")
	}

	// Construct the exact pre-extension multi-block format, including PSU and
	// the record-level contributing-statistics trailer.
	legacy := appendPreamble(nil, a)
	binary.LittleEndian.PutUint16(legacy[16:], binary.LittleEndian.Uint16(legacy[16:])&^flagHasDiskIDs)
	for _, block := range []*collector.Sample{a.Data, a.Min, a.Max} {
		legacy = appendFixed(legacy, block)
		variable, encodeErr := appendVariable(nil, block)
		if encodeErr != nil {
			t.Fatal(encodeErr)
		}
		extension, _ := appendDiskIDs(nil, block)
		legacy = append(legacy, variable[:len(variable)-len(extension)]...)
	}
	legacy, err = appendMeanStats(legacy, a)
	if err != nil {
		t.Fatal(err)
	}
	old, err := decodeSample(legacy)
	if err != nil {
		t.Fatal(err)
	}
	for _, block := range []*collector.Sample{old.Data, old.Min, old.Max} {
		for _, disk := range block.Disks.Devices {
			if disk.ID != "" || disk.Name == "" {
				t.Fatalf("legacy record attributed to physical drive: %+v", disk)
			}
		}
	}
	if !reflect.DeepEqual(old.MeanStats, a.MeanStats) {
		t.Fatal("legacy statistics changed")
	}

	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	for _, data := range [][]byte{payload, legacy} {
		cmd := exec.Command(python, "-c", `import importlib.util, json, sys
spec = importlib.util.spec_from_file_location("inspect_tier", "../../addons/inspect_tier.py")
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
decoded = module.decode_v2_record(sys.stdin.buffer.read())
print(json.dumps([[d.get("id", "") for d in decoded[b]["disks"]["devices"]] for b in ["data", "min", "max"]]))`)
		cmd.Stdin = bytes.NewReader(data)
		out, runErr := cmd.CombinedOutput()
		if runErr != nil {
			t.Fatalf("Python decoder: %v: %s", runErr, out)
		}
		var ids [][]string
		if err := json.Unmarshal(out, &ids); err != nil {
			t.Fatal(err)
		}
		goDecoded, _ := decodeSample(data)
		for i, block := range []*collector.Sample{goDecoded.Data, goDecoded.Min, goDecoded.Max} {
			for j, disk := range block.Disks.Devices {
				if ids[i][j] != disk.ID {
					t.Fatalf("Python/Go ID mismatch: %s", out)
				}
			}
		}
	}
}

func TestDiskIdentityCodecRejectsCorruptExtension(t *testing.T) {
	sample := &collector.Sample{Disks: collector.DiskStats{Devices: []collector.DiskDevice{{Name: "sda", ID: "wwid:test"}}}}
	extension, err := appendDiskIDs(nil, sample)
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n < len(extension); n++ {
		if _, err := decodeDiskIDs(extension[:n], sample); err == nil {
			t.Fatalf("accepted truncation at %d", n)
		}
	}
	for _, corrupt := range []func([]byte){
		func(b []byte) { b[0] = 99 },
		func(b []byte) { b[1], b[2] = 0xff, 0xff },
		func(b []byte) { b[3], b[4] = 0xff, 0xff },
	} {
		bad := bytes.Clone(extension)
		corrupt(bad)
		if _, err := decodeDiskIDs(bad, sample); err == nil {
			t.Fatal("accepted corrupt extension")
		}
	}
	sample.Disks.Devices[0].ID = strings.Repeat("x", 65536)
	if _, err := appendDiskIDs(nil, sample); err == nil {
		t.Fatal("silently truncated an oversized identity")
	}
}

func TestDiskIdentityAggregationAndRestart(t *testing.T) {
	ts := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	samples := []*collector.Sample{
		{Timestamp: ts, Disks: collector.DiskStats{Devices: []collector.DiskDevice{{ID: "serial:A", Name: "sda", ReadBytesPS: 10}, {ID: "serial:B", Name: "sdb", ReadBytesPS: 100}}}},
		{Timestamp: ts.Add(time.Second), Disks: collector.DiskStats{Devices: []collector.DiskDevice{{ID: "serial:A", Name: "sdb", ReadBytesPS: 30}, {ID: "serial:B", Name: "sda", ReadBytesPS: 200}}}},
		{Timestamp: ts.Add(2 * time.Second), Disks: collector.DiskStats{Devices: []collector.DiskDevice{{ID: "serial:B", Name: "sda", ReadBytesPS: 300}}}},
	}
	s := &Store{}
	direct := s.aggregateSamples(samples, 3*time.Second)
	first := s.aggregateSamples(samples[:2], 2*time.Second)
	second := s.aggregateSamples(samples[2:], time.Second)
	encoded, err := encodeSample(first)
	if err != nil {
		t.Fatal(err)
	}
	first, err = decodeSample(encoded)
	if err != nil {
		t.Fatal(err)
	}
	cascaded := s.aggregateAggregated([]*AggregatedSample{first, second}, 3*time.Second)
	for _, agg := range []*AggregatedSample{direct, cascaded} {
		if len(agg.Data.Disks.Devices) != 2 {
			t.Fatalf("renamed disks split or merged: %+v", agg.Data.Disks.Devices)
		}
		a, b := agg.Data.Disks.Devices[0], agg.Data.Disks.Devices[1]
		if a.ID != "serial:A" || a.Name != "sdb" || a.ReadBytesPS != 20 || b.ID != "serial:B" || b.ReadBytesPS != 200 {
			t.Fatalf("wrong identities or means after cascade: %+v", agg.Data.Disks.Devices)
		}
		if agg.Min.Disks.Devices[0].ReadBytesPS != 10 || agg.Max.Disks.Devices[0].ReadBytesPS != 30 {
			t.Fatal("envelopes mixed identities")
		}
	}
	legacy := &collector.Sample{Disks: collector.DiskStats{Devices: []collector.DiskDevice{{Name: "sda", ReadBytesPS: 999}}}}
	mixed := s.aggregateSamples([]*collector.Sample{legacy, samples[0]}, 2*time.Second)
	if len(mixed.Data.Disks.Devices) != 3 {
		t.Fatal("legacy name history merged with a stable identity")
	}
	cfg := config.StorageConfig{Directory: t.TempDir(), Tiers: []config.TierConfig{{Resolution: time.Second, MaxBytes: 1 << 20}}}
	store, err := NewStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteSample(samples[0]); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	store, err = NewStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	latest, err := store.QueryLatest()
	if err != nil || latest == nil || latest.Data.Disks.Devices[0].ID != "serial:A" {
		t.Fatalf("restart lost identity: %+v", latest)
	}
}
