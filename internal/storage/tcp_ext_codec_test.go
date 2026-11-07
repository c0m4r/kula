package storage

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"os/exec"
	"reflect"
	"testing"
	"time"

	"kula/internal/collector"
)

// tcpExtEncodedSize is the TCP extension's size in one variable block: its
// version byte plus the v1 block.
const tcpExtEncodedSize = 1 + tcpExtV1Size

// preTCPExtVariable returns block's variable section exactly as an encoder
// from before flagHasTCPExt wrote it: everything up to the disk identities.
func preTCPExtVariable(t *testing.T, block *collector.Sample) []byte {
	t.Helper()
	variable, err := appendVariable(nil, block)
	if err != nil {
		t.Fatal(err)
	}
	return variable[:len(variable)-tcpExtEncodedSize]
}

func TestTCPExtCodecCompatibility(t *testing.T) {
	a := makeSampleFull(time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC))
	a.Duration = time.Minute
	a.Min = cloneAggregatedSample(a).Data
	a.Max = cloneAggregatedSample(a).Data
	a.Data.Network.TCP.Retrans = 4.5
	a.Min.Network.TCP.Retrans = 1.25
	a.Max.Network.TCP.Retrans = 12
	a.MeanStats = map[string]meanAccumulator{"sample.net.tcp.curr_estab": {Sum: 42, Weight: 1}}
	a.MeanWeightsComplete = true
	a.AggregationVersion = currentAggregationVersion

	payload, err := encodeSample(a)
	if err != nil {
		t.Fatal(err)
	}
	current, err := decodeSample(payload)
	if err != nil {
		t.Fatal(err)
	}
	want := []float64{4.5, 1.25, 12}
	for i, block := range []*collector.Sample{current.Data, current.Min, current.Max} {
		if got := block.Network.TCP.Retrans; got != want[i] {
			t.Fatalf("block %d retrans_ps = %v, want %v", i, got, want[i])
		}
	}
	if !reflect.DeepEqual(current.MeanStats, a.MeanStats) {
		t.Fatal("TCP extension damaged contributing statistics")
	}

	// The exact pre-extension multi-block format: disk identities closing each
	// block, then the record-level contributing-statistics trailer.
	legacy := appendPreamble(nil, a)
	binary.LittleEndian.PutUint16(legacy[16:], binary.LittleEndian.Uint16(legacy[16:])&^flagHasTCPExt)
	for _, block := range []*collector.Sample{a.Data, a.Min, a.Max} {
		legacy = appendFixed(legacy, block)
		legacy = append(legacy, preTCPExtVariable(t, block)...)
	}
	if legacy, err = appendMeanStats(legacy, a); err != nil {
		t.Fatal(err)
	}
	old, err := decodeSample(legacy)
	if err != nil {
		t.Fatalf("pre-extension record: %v", err)
	}
	if !reflect.DeepEqual(old.MeanStats, a.MeanStats) {
		t.Fatal("pre-extension record: contributing statistics changed")
	}
	if old.ExtremaProfile != current.ExtremaProfile {
		t.Fatalf("pre-extension extrema profile = %q, want %q", old.ExtremaProfile, current.ExtremaProfile)
	}
	for i, block := range []*collector.Sample{old.Data, old.Min, old.Max} {
		if got := block.Network.TCP.Retrans; got != 0 {
			t.Fatalf("pre-extension block %d retrans_ps = %v, want 0", i, got)
		}
		// Apart from the retransmit rate, the old record must decode to the
		// same sample: the extension may not shift any other section.
		aligned := *[]*collector.Sample{current.Data, current.Min, current.Max}[i]
		aligned.Network.TCP.Retrans = 0
		if !reflect.DeepEqual(*block, aligned) {
			t.Fatalf("pre-extension block %d decoded differently:\n got %+v\nwant %+v", i, *block, aligned)
		}
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
print(json.dumps([decoded[b]["tcp"]["retrans_ps"] for b in ["data", "min", "max"]]))`)
		cmd.Stdin = bytes.NewReader(data)
		out, runErr := cmd.CombinedOutput()
		if runErr != nil {
			t.Fatalf("Python decoder: %v: %s", runErr, out)
		}
		var rates []float64
		if err := json.Unmarshal(out, &rates); err != nil {
			t.Fatalf("Python output %q: %v", out, err)
		}
		goDecoded, _ := decodeSample(data)
		for i, block := range []*collector.Sample{goDecoded.Data, goDecoded.Min, goDecoded.Max} {
			if rates[i] != block.Network.TCP.Retrans {
				t.Fatalf("Python/Go retrans_ps mismatch in block %d: python %v, go %v", i, rates[i], block.Network.TCP.Retrans)
			}
		}
	}
}

func TestTCPExtCodecRejectsCorruptExtension(t *testing.T) {
	sample := &collector.Sample{Network: collector.NetworkStats{TCP: collector.TCPStats{Retrans: 3}}}
	extension := appendTCPExt(nil, sample)
	if len(extension) != tcpExtEncodedSize {
		t.Fatalf("extension size = %d, want %d", len(extension), tcpExtEncodedSize)
	}
	for n := 0; n < len(extension); n++ {
		if _, err := decodeTCPExt(extension[:n], &collector.Sample{}); err == nil {
			t.Fatalf("accepted truncation at %d", n)
		}
	}
	bad := bytes.Clone(extension)
	bad[0] = tcpExtVersion + 1
	if _, err := decodeTCPExt(bad, &collector.Sample{}); err == nil {
		t.Fatal("accepted an unknown TCP extension version")
	}
	decoded := &collector.Sample{}
	if n, err := decodeTCPExt(extension, decoded); err != nil || n != tcpExtEncodedSize || decoded.Network.TCP.Retrans != 3 {
		t.Fatalf("decodeTCPExt = (%d, %v), retrans %v", n, err, decoded.Network.TCP.Retrans)
	}
}
