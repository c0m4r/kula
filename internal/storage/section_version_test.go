package storage

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"

	"kula/internal/collector"
)

// TestDecodeRefusesUnknownSectionVersions requires a PostgreSQL, MySQL or
// Apache2 version byte newer than the decoder knows to fail the record, in Go
// and in the Python inspector alike. Read as another layout, it would
// misalign every section after it: Apache2 used to read nothing at all and
// decode the bytes that followed as custom metrics.
func TestDecodeRefusesUnknownSectionVersions(t *testing.T) {
	full := makeSampleFull(time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC))
	full.Data.Apps.Postgres = &collector.PostgresStats{ActiveConns: 1}
	full.Data.Apps.Mysql = &collector.MysqlStats{ThreadsConnected: 1}
	full.Data.Apps.Apache2 = &collector.Apache2Stats{BusyWorkers: 1}
	fullPayload, err := encodeSample(full)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeSample(fullPayload); err != nil {
		t.Fatalf("record with every section: %v", err)
	}

	sections := []struct {
		name    string
		drop    func(*collector.Sample)
		unknown byte
	}{
		{"postgres", func(s *collector.Sample) { s.Apps.Postgres = nil }, 4},
		{"mysql", func(s *collector.Sample) { s.Apps.Mysql = nil }, 4},
		{"apache2", func(s *collector.Sample) { s.Apps.Apache2 = nil }, 3},
	}
	var corrupted []string
	for _, section := range sections {
		data := *full.Data
		data.Apps = full.Data.Apps
		section.drop(&data)
		without := *full
		without.Data = &data
		payload, err := encodeSample(&without)
		if err != nil {
			t.Fatal(err)
		}
		// The first byte where the two records differ is the section's
		// presence byte: 0 here, its version in the full record.
		at := 0
		for at < len(payload) && payload[at] == fullPayload[at] {
			at++
		}
		if at == len(payload) || payload[at] != 0 {
			t.Fatalf("%s: presence byte not found", section.name)
		}
		bad := bytes.Clone(payload)
		bad[at] = section.unknown
		if _, err := decodeSample(bad); err == nil || !strings.Contains(err.Error(), "unsupported "+section.name) {
			t.Errorf("%s version %d: decode error = %v, want unsupported version", section.name, section.unknown, err)
		}
		corrupted = append(corrupted, hex.EncodeToString(bad))
	}

	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is unavailable")
	}
	input, _ := json.Marshal(corrupted)
	cmd := exec.Command(python, "-c", `import importlib.util, json, sys
spec = importlib.util.spec_from_file_location("inspect_tier", "../../addons/inspect_tier.py")
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
results = []
for payload in json.load(sys.stdin):
    try:
        module.decode_v2_record(bytes.fromhex(payload))
        results.append("decoded")
    except ValueError as err:
        results.append(str(err))
print(json.dumps(results))`)
	cmd.Stdin = bytes.NewReader(input)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Python decoder: %v: %s", err, out)
	}
	var results []string
	if err := json.Unmarshal(out, &results); err != nil {
		t.Fatalf("Python output %q: %v", out, err)
	}
	for i, section := range sections {
		if !strings.Contains(results[i], "unsupported "+section.name) {
			t.Errorf("Python %s version %d: %q, want unsupported version", section.name, section.unknown, results[i])
		}
	}
}
