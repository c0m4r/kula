package storage

import (
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"kula/internal/collector"
)

func TestTierMigration_JSONToBinary(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "test_tier.dat")
	maxSize := int64(64 * 1024)
	maxData := maxSize - headerSize

	// 1. A legacy JSON (v1) tier file
	ts1 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	ts2 := time.Date(2026, 1, 1, 10, 0, 1, 0, time.UTC)
	writeLegacyJSONTier(t, path, maxData,
		&AggregatedSample{
			Timestamp: ts1,
			Duration:  time.Second,
			Data:      &collector.Sample{Timestamp: ts1, System: collector.SystemStats{Hostname: "host1"}},
		},
		&AggregatedSample{
			Timestamp: ts2,
			Duration:  time.Second,
			Data:      &collector.Sample{Timestamp: ts2, System: collector.SystemStats{Hostname: "host2"}},
		})

	// 2. Open with OpenTier (should trigger migration)
	tier, err := OpenTier(path, maxSize)
	if err != nil {
		t.Fatalf("OpenTier failed: %v", err)
	}
	defer func() { _ = tier.Close() }()

	// 3. Verify migration
	if tier.codecVer != codecVersion2 {
		t.Errorf("Expected codec version %d, got %d", codecVersion2, tier.codecVer)
	}
	if tier.Count() != 2 {
		t.Errorf("Expected count 2, got %d", tier.Count())
	}

	samples, err := tier.ReadRange(ts1, ts2)
	if err != nil {
		t.Fatalf("ReadRange failed: %v", err)
	}
	if len(samples) != 2 {
		t.Errorf("Expected 2 samples, got %d", len(samples))
	} else {
		if samples[0].Data.System.Hostname != "host1" {
			t.Errorf("Sample 1 hostname = %q, want \"host1\"", samples[0].Data.System.Hostname)
		}
		if samples[1].Data.System.Hostname != "host2" {
			t.Errorf("Sample 2 hostname = %q, want \"host2\"", samples[1].Data.System.Hostname)
		}
	}

	// 4. Verify file content on disk is NOT JSON
	// Binary v2 records start with recordKindBinary (0x02)
	f2, _ := os.Open(path)
	defer func() { _ = f2.Close() }()
	peak := make([]byte, 5)
	if _, err := f2.ReadAt(peak, headerSize+4); err != nil {
		t.Fatalf("Failed to read back migrated data: %v", err)
	}
	if peak[0] != 0x02 {
		t.Errorf("Expected record kind %02x, got %02x (likely still JSON)", 0x02, peak[0])
	}
}

// writeLegacyJSONTier writes a codec v1 tier file holding samples as JSON
// records, the format of the earliest releases.
func writeLegacyJSONTier(t *testing.T, path string, maxData int64, samples ...*AggregatedSample) {
	t.Helper()
	header := make([]byte, headerSize)
	copy(header[0:4], magicString)
	binary.LittleEndian.PutUint64(header[8:16], 1) // codecVer = 1
	binary.LittleEndian.PutUint64(header[16:24], uint64(maxData))

	var records []byte
	for _, sample := range samples {
		data, err := json.Marshal(sample)
		if err != nil {
			t.Fatal(err)
		}
		records = binary.LittleEndian.AppendUint32(records, uint32(len(data)))
		records = append(records, data...)
	}
	binary.LittleEndian.PutUint64(header[24:32], uint64(len(records)))
	binary.LittleEndian.PutUint64(header[32:40], uint64(len(samples)))
	if len(samples) > 0 {
		binary.LittleEndian.PutUint64(header[40:48], uint64(samples[0].Timestamp.UnixNano()))
		binary.LittleEndian.PutUint64(header[48:56], uint64(samples[len(samples)-1].Timestamp.UnixNano()))
	}
	if err := os.WriteFile(path, append(header, records...), 0o600); err != nil {
		t.Fatalf("writing legacy tier: %v", err)
	}
}
