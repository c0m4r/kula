package storage

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"math"
	"os/exec"
	"testing"
	"time"

	"kula/internal/collector"
)

// An infinity or NaN in a stored record comes back in every history response
// that covers it, and encoding/json refuses to encode either. The float
// helpers keep both out: a value float32 cannot hold saturates, NaN is 0.
func TestCodecStoresOnlyFiniteFloats(t *testing.T) {
	var b [4]byte
	for _, tc := range []struct{ in, want float64 }{
		{42.5, 42.5},
		{-0.25, -0.25},
		{math.MaxFloat32, math.MaxFloat32},
		{1e39, math.MaxFloat32},
		{-1e39, -math.MaxFloat32},
		{math.MaxFloat64, math.MaxFloat32},
		{math.Inf(1), math.MaxFloat32},
		{math.Inf(-1), -math.MaxFloat32},
		{math.NaN(), 0},
	} {
		putF32(b[:], tc.in)
		if stored := math.Float32frombits(binary.LittleEndian.Uint32(b[:])); math.IsNaN(float64(stored)) || math.IsInf(float64(stored), 0) {
			t.Errorf("putF32(%v) stored %v", tc.in, stored)
		}
		if got := getF32(b[:]); got != tc.want {
			t.Errorf("putF32(%v) read back as %v, want %v", tc.in, got, tc.want)
		}
	}

	// What a version without the guard wrote.
	for _, tc := range []struct {
		bits uint32
		want float64
	}{
		{0x7f800000, math.MaxFloat32},  // +Inf
		{0xff800000, -math.MaxFloat32}, // -Inf
		{0x7fc00000, 0},                // NaN
	} {
		binary.LittleEndian.PutUint32(b[:], tc.bits)
		if got := getF32(b[:]); got != tc.want {
			t.Errorf("getF32(%#x) = %v, want %v", tc.bits, got, tc.want)
		}
	}

	for in, want := range map[float64]float64{1.5: 1.5, math.MaxFloat64: math.MaxFloat64, math.Inf(1): 0, math.Inf(-1): 0} {
		if got := finiteF64(in); got != want {
			t.Errorf("finiteF64(%v) = %v, want %v", in, got, want)
		}
	}
	if got := finiteF64(math.NaN()); got != 0 {
		t.Errorf("finiteF64(NaN) = %v, want 0", got)
	}
}

// One value beyond float32's range (a custom metric producer's "no reading"
// sentinel) used to be stored as +Inf, in the raw record and in the extrema of
// every rollup of its minute, and no history response covering it could be
// encoded until the records aged out of all three tiers.
func TestHistoryEncodesAfterAnOutOfRangeValue(t *testing.T) {
	store := newMultiTierStore(t)
	defer func() { _ = store.Close() }()

	base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	for i := 1; i <= 400; i++ {
		value := 42.0
		if i == 100 {
			value = 1e39
		}
		sample := &collector.Sample{Timestamp: base.Add(time.Duration(i) * time.Second)}
		sample.Apps.Custom = map[string][]collector.CustomMetricValue{"g": {{Name: "m", Value: value}}}
		if err := store.WriteSample(sample); err != nil {
			t.Fatalf("WriteSample %d: %v", i, err)
		}
	}

	for tier, window := range [][2]time.Duration{
		{95 * time.Second, 105 * time.Second},
		{0, 400 * time.Second},
		{0, 400 * time.Second},
	} {
		result, err := store.QueryRangeFromTier(base.Add(window[0]), base.Add(window[1]), 500, tier)
		if err != nil {
			t.Fatalf("tier %d: %v", tier, err)
		}
		if result.Tier != tier || len(result.Samples) == 0 {
			t.Fatalf("tier %d: answered from tier %d with %d samples", tier, result.Tier, len(result.Samples))
		}
		if _, err := json.Marshal(result); err != nil {
			t.Errorf("tier %d: history does not encode: %v", tier, err)
		}
		// The value is kept as the largest float32, in the raw sample and in
		// the maximum of each rollup that covers it.
		peak := 0.0
		for _, sample := range result.Samples {
			for _, block := range []*collector.Sample{sample.Data, sample.Max} {
				if block != nil && len(block.Apps.Custom["g"]) == 1 {
					peak = max(peak, block.Apps.Custom["g"][0].Value)
				}
			}
		}
		if peak != math.MaxFloat32 {
			t.Errorf("tier %d: peak = %v, want %v", tier, peak, float64(math.MaxFloat32))
		}
	}
}

// A record written before values were saturated can hold an infinity or a
// NaN. The Go reader and the Python inspector both read it as finite, and the
// same.
func TestDecodersReadStoredInfinitiesAsFinite(t *testing.T) {
	sample := &collector.Sample{Timestamp: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)}
	sample.System.Uptime = math.MaxFloat64
	sample.Apps.Custom = map[string][]collector.CustomMetricValue{"g": {
		{Name: "inf", Value: math.MaxFloat32},
		{Name: "nan", Value: -math.MaxFloat32},
	}}
	payload, err := encodeSample(&AggregatedSample{Timestamp: sample.Timestamp, Duration: time.Second, Data: sample})
	if err != nil {
		t.Fatal(err)
	}
	// Swap each marker value for the bits an unguarded encoder left behind.
	poison := func(marker, bits []byte) {
		t.Helper()
		if n := bytes.Count(payload, marker); n != 1 {
			t.Fatalf("marker %x occurs %d times in the record, want once", marker, n)
		}
		payload = bytes.Replace(payload, marker, bits, 1)
	}
	f32 := func(bits uint32) []byte { return binary.LittleEndian.AppendUint32(nil, bits) }
	f64 := func(bits uint64) []byte { return binary.LittleEndian.AppendUint64(nil, bits) }
	poison(f32(math.Float32bits(math.MaxFloat32)), f32(0x7f800000))                    // +Inf
	poison(f32(math.Float32bits(-math.MaxFloat32)), f32(0x7fc00000))                   // NaN
	poison(f64(math.Float64bits(math.MaxFloat64)), f64(math.Float64bits(math.Inf(1)))) // uptime

	decoded, err := decodeSample(payload)
	if err != nil {
		t.Fatal(err)
	}
	custom := decoded.Data.Apps.Custom["g"]
	if len(custom) != 2 {
		t.Fatalf("decoded custom metrics: %+v", custom)
	}
	got := []float64{custom[0].Value, custom[1].Value, decoded.Data.System.Uptime}
	want := []float64{math.MaxFloat32, 0, 0}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Go value %d = %v, want %v", i, got[i], want[i])
		}
	}
	if _, err := json.Marshal(decoded); err != nil {
		t.Errorf("decoded record does not encode: %v", err)
	}

	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	cmd := exec.Command(python, "-c", `import importlib.util, json, sys
spec = importlib.util.spec_from_file_location("inspect_tier", "../../addons/inspect_tier.py")
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
data = module.decode_v2_record(sys.stdin.buffer.read())["data"]
print(json.dumps([m["value"] for m in data["apps"]["custom"]["g"]] + [data["system"]["uptime_sec"]], allow_nan=False))`)
	cmd.Stdin = bytes.NewReader(append([]byte{recordKindBinary}, payload...))
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("Python decoder: %v: %s", runErr, out)
	}
	var fromPython []float64
	if err := json.Unmarshal(out, &fromPython); err != nil {
		t.Fatalf("Python output %q: %v", out, err)
	}
	if len(fromPython) != len(got) {
		t.Fatalf("Python decoded %v, Go %v", fromPython, got)
	}
	for i := range got {
		if fromPython[i] != got[i] {
			t.Errorf("Python/Go mismatch at %d: python %v, go %v", i, fromPython[i], got[i])
		}
	}
}
