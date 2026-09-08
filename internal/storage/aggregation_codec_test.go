package storage

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"math"
	"os/exec"
	"reflect"
	"testing"
	"time"
)

func TestMeanStatsCodecCompatibilityAndValidation(t *testing.T) {
	old := makeSampleFull(time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC))
	old.AggregationVersion = currentAggregationVersion
	legacy, err := encodeSample(old)
	if err != nil {
		t.Fatal(err)
	}
	current := *old
	current.MeanWeightsComplete = true
	current.MeanStats = map[string]meanAccumulator{
		`sample.apps.containers["Name=api"].cpu_pct`: {Sum: 100, Weight: 1},
		"sample.apps.mysql.replica_seconds_behind":   {Sum: 0, Weight: 0},
	}
	payload, err := encodeSample(&current)
	if err != nil {
		t.Fatal(err)
	}
	// The extension follows the full old payload; every metric byte is unchanged.
	if !bytes.Equal(payload[:16], legacy[:16]) || !bytes.Equal(payload[18:len(legacy)], legacy[18:]) {
		t.Fatal("mean statistics changed an existing metric block")
	}
	decoded, err := decodeSample(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !decoded.MeanWeightsComplete || !reflect.DeepEqual(decoded.MeanStats, current.MeanStats) {
		t.Fatalf("statistics round trip: %+v", decoded.MeanStats)
	}
	decoded, err = decodeSample(legacy)
	if err != nil || decoded.MeanStats != nil || decoded.MeanWeightsComplete {
		t.Fatalf("legacy record was promoted or rejected: %+v, %v", decoded, err)
	}
	for size := len(legacy); size < len(payload); size++ {
		if _, err := decodeSample(payload[:size]); err == nil {
			t.Fatalf("accepted truncated mean statistics at byte %d", size)
		}
	}
	for _, mutate := range []func([]byte){
		func(b []byte) { b[len(legacy)] = 99 }, // unknown version
		func(b []byte) { binary.LittleEndian.PutUint32(b[len(legacy)+2:], math.MaxUint32) },
		func(b []byte) { binary.LittleEndian.PutUint64(b[len(b)-8:], math.Float64bits(-1)) },
		func(b []byte) { binary.LittleEndian.PutUint64(b[len(b)-16:], math.Float64bits(math.NaN())) },
	} {
		bad := bytes.Clone(payload)
		mutate(bad)
		if _, err := decodeSample(bad); err == nil {
			t.Fatal("accepted invalid mean statistics")
		}
	}

	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is unavailable")
	}
	cmd := exec.Command(python, "-c", `import importlib.util, json, sys
spec = importlib.util.spec_from_file_location("inspect_tier", "../../addons/inspect_tier.py")
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
decoded = module.decode_v2_record(sys.stdin.buffer.read())
print(json.dumps({"complete": decoded["mean_weights_complete"], "stats": decoded["mean_stats"]}))`)
	cmd.Stdin = bytes.NewReader(payload)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Python decoder: %v: %s", err, output)
	}
	var result struct {
		Complete bool
		Stats    map[string]meanAccumulator
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatal(err)
	}
	if !result.Complete || !reflect.DeepEqual(result.Stats, current.MeanStats) {
		t.Fatalf("Python/Go statistics disagree: %s", output)
	}
}
