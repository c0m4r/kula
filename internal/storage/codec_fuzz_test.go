package storage

import (
	"encoding/binary"
	"testing"
	"time"

	"kula/internal/collector"
)

// Fuzz coverage for the on-disk binary codec.
//
// decodeSample/decodeFixed/decodeVariable/extractTimestamp parse length- and
// count-prefixed binary records read straight off disk. A corrupted or
// truncated tier file (partial write on power loss, bit rot, a record from a
// future/older format) must never panic the 24/7 agent — it should return an
// error and move on. These targets assert exactly that: arbitrary bytes in,
// no panic / no out-of-bounds / no runaway allocation.

// seedRecords returns a handful of well-formed and adversarial encoded records
// used to seed every codec fuzz target.
func seedRecords(tb testing.TB) [][]byte {
	tb.Helper()
	ts := time.Unix(1_700_000_000, 123456789)

	var seeds [][]byte
	add := func(a *AggregatedSample) {
		b, err := encodeSample(a)
		if err != nil {
			tb.Fatalf("seed encode: %v", err)
		}
		seeds = append(seeds, b)
	}

	// Fully-populated record (Data only).
	add(makeSampleFull(ts))

	// Aggregated record carrying Data + Min + Max blocks.
	full := makeSampleFull(ts)
	full.Min = makeSampleFull(ts).Data
	full.Max = makeSampleFull(ts).Data
	add(full)

	// The disk-ID extension must also be exercised with populated ID strings.
	disks := makeSampleFull(ts)
	disks.Data.Disks.Devices = []collector.DiskDevice{
		{ID: "wwid:eui.0011223344556677", Name: "sda"},
		{ID: "serial:||A%3Apart%3A1", Name: "sdb"},
		{Name: "vda"},
	}
	disks.Min = disks.Data
	disks.Max = disks.Data
	add(disks)

	// Minimal record: valid preamble, no sample blocks.
	add(&AggregatedSample{Timestamp: ts, Duration: time.Second})

	// Hand-crafted adversarial headers.
	seeds = append(seeds,
		[]byte{},                             // empty
		make([]byte, 17),                     // one byte short of the 18-byte preamble
		make([]byte, 18),                     // bare preamble, all flags clear
		craftHeader(flagHasData),             // claims a fixed block that isn't there
		craftHeader(flagHasData|flagHasApps), // claims data + variable section
		craftHeader(0xFFFF),                  // every flag bit set
		craftHugeCounts(),                    // valid fixed block, then huge iface count
	)
	return seeds
}

// craftHeader builds an 18-byte preamble with the given flags and no payload.
func craftHeader(flags uint16) []byte {
	b := make([]byte, 18)
	binary.LittleEndian.PutUint16(b[16:], flags)
	return b
}

// craftHugeCounts builds a record whose variable block claims the maximum
// interface count (uint16) but supplies no interface bytes, exercising the
// make()-with-attacker-count + bounds-check path in decodeVariable.
func craftHugeCounts() []byte {
	b := craftHeader(flagHasData | flagHasApps)
	b = append(b, make([]byte, fixedBlockSize)...) // empty fixed block
	b = append(b, 0xFF, 0xFF)                      // numIfaces = 65535, then EOF
	return b
}

// FuzzDecodeSample throws arbitrary bytes at the top-level record decoder.
func FuzzDecodeSample(f *testing.F) {
	for _, s := range seedRecords(f) {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		a, err := decodeSample(data)
		if err != nil {
			return // malformed input: rejected cleanly, nothing more to check
		}
		if a == nil {
			t.Fatal("decodeSample returned nil sample with nil error")
		}
		// Invariant: anything the decoder accepts, the encoder must be able to
		// reproduce. A decode/encode asymmetry (e.g. a field decode accepts a
		// value encode cannot represent) would be a latent corruption bug.
		if _, err := encodeSample(a); err != nil {
			t.Fatalf("decoded record is not re-encodable: %v", err)
		}
	})
}

// FuzzExtractTimestamp targets the lightweight header reader used to index
// records without decoding the whole payload.
func FuzzExtractTimestamp(f *testing.F) {
	for _, s := range seedRecords(f) {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = extractTimestamp(data) // must never panic
	})
}

// FuzzEncodeDecodeRoundTrip builds a deterministic, map-free sample from typed
// fuzz inputs, then asserts encode→decode→encode is stable and that the
// length-prefixed interface name survives intact. This exercises the encoder
// and the string/scalar codecs with adversarial values (control bytes, long
// names, NaN/Inf-producing bit patterns) that the unit tests don't cover.
func FuzzEncodeDecodeRoundTrip(f *testing.F) {
	f.Add("eth0", uint64(16<<30), float64(1.5), uint16(8), 42)
	f.Add("", uint64(0), float64(0), uint16(0), 0)
	f.Add("a name with spaces & \x00 control", uint64(1), float64(-1), uint16(1), 1)

	f.Fuzz(func(t *testing.T, ifname string, memTotal uint64, rxMbps float64, numCores uint16, currEstab int) {
		orig := &AggregatedSample{
			Timestamp: time.Unix(0, 1234567890),
			Duration:  time.Second,
			Data: &collector.Sample{
				CPU:    collector.CPUStats{NumCores: int(numCores)},
				Memory: collector.MemoryStats{Total: memTotal},
				Network: collector.NetworkStats{
					Interfaces: []collector.NetInterface{{Name: ifname, RxMbps: rxMbps}},
					TCP:        collector.TCPStats{CurrEstab: uint64(currEstab)},
				},
			},
		}

		enc1, err := encodeSample(orig)
		if err != nil {
			// The only documented encode error is a string exceeding the
			// 255-byte on-disk limit; anything else is a bug.
			if len(ifname) <= 255 {
				t.Fatalf("encode failed for representable sample (ifname %d bytes): %v", len(ifname), err)
			}
			return
		}

		dec, err := decodeSample(enc1)
		if err != nil {
			t.Fatalf("decode of freshly-encoded record failed: %v", err)
		}

		// Re-encode must be byte-identical: the sample is map-free, so encoding
		// is fully deterministic.
		enc2, err := encodeSample(dec)
		if err != nil {
			t.Fatalf("re-encode failed: %v", err)
		}
		if string(enc1) != string(enc2) {
			t.Fatalf("round-trip not stable: %d vs %d bytes", len(enc1), len(enc2))
		}

		// Spot-check that the length-prefixed interface name survived exactly.
		if dec.Data == nil || len(dec.Data.Network.Interfaces) != 1 {
			t.Fatalf("interface list not preserved: %+v", dec.Data)
		}
		if got := dec.Data.Network.Interfaces[0].Name; got != ifname {
			t.Fatalf("iface name = %q, want %q", got, ifname)
		}
		if dec.Data.CPU.NumCores != int(numCores) {
			t.Fatalf("num cores = %d, want %d", dec.Data.CPU.NumCores, numCores)
		}
		if dec.Data.Memory.Total != memTotal {
			t.Fatalf("mem total = %d, want %d", dec.Data.Memory.Total, memTotal)
		}
	})
}
