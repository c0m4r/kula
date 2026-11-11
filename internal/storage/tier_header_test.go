package storage

import (
	"encoding/binary"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tierHeader is the 64-byte header of a tier file, field by field.
type tierHeader struct {
	flags                  uint32
	version, maxData       uint64
	writeOff, count        uint64
	oldestNano, newestNano int64
	oldestOff              uint64
}

func (h tierHeader) bytes() []byte {
	buf := make([]byte, headerSize)
	copy(buf, magicString)
	binary.LittleEndian.PutUint32(buf[4:], h.flags)
	binary.LittleEndian.PutUint64(buf[8:], h.version)
	binary.LittleEndian.PutUint64(buf[16:], h.maxData)
	binary.LittleEndian.PutUint64(buf[24:], h.writeOff)
	binary.LittleEndian.PutUint64(buf[32:], h.count)
	binary.LittleEndian.PutUint64(buf[40:], uint64(h.oldestNano))
	binary.LittleEndian.PutUint64(buf[48:], uint64(h.newestNano))
	binary.LittleEndian.PutUint64(buf[56:], h.oldestOff)
	return buf
}

func parseTierHeader(buf []byte) tierHeader {
	return tierHeader{
		flags:      binary.LittleEndian.Uint32(buf[4:]),
		version:    binary.LittleEndian.Uint64(buf[8:]),
		maxData:    binary.LittleEndian.Uint64(buf[16:]),
		writeOff:   binary.LittleEndian.Uint64(buf[24:]),
		count:      binary.LittleEndian.Uint64(buf[32:]),
		oldestNano: int64(binary.LittleEndian.Uint64(buf[40:])),
		newestNano: int64(binary.LittleEndian.Uint64(buf[48:])),
		oldestOff:  binary.LittleEndian.Uint64(buf[56:]),
	}
}

// TestOpenTierRefusesOffsetsOutsideTheRing: a header whose geometry the ring
// arithmetic cannot hold is corrupt, for the owner and for a read-only view.
// The first case is the fuzz crasher behind L2(c): a write offset near 2^63
// overflowed Write's wrap check, so the record went to that offset, and the
// next scan walked the 9.2 EB sparse file that made, holding the tier lock.
func TestOpenTierRefusesOffsetsOutsideTheRing(t *testing.T) {
	for name, h := range map[string]tierHeader{
		"write offset near 2^63": {
			flags: flagHeaderHasTail | flagHeaderWrapped, version: codecVersion2,
			maxData: 1 << 62, writeOff: math.MaxInt64 - 72, count: 1,
			oldestNano: 13, newestNano: 1 << 62, oldestOff: math.MaxInt64 - 51,
		},
		"write offset past the ring": {
			flags: flagHeaderHasTail, version: codecVersion2, maxData: 8 << 10, writeOff: 8<<10 + 1, count: 1,
		},
		"negative ring size": {
			flags: flagHeaderHasTail, version: codecVersion2, maxData: 1 << 63, count: 1,
		},
		"ring size past every offset": {
			flags: flagHeaderHasTail, version: codecVersion2, maxData: maxTierData + 1, count: 1,
		},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "tier_0.dat")
			if err := os.WriteFile(path, append(h.bytes(), '0'), 0o600); err != nil {
				t.Fatal(err)
			}
			if tier, err := OpenTier(path, 8<<10+headerSize); err == nil {
				_ = tier.Close()
				t.Fatalf("OpenTier accepted a ring of %d bytes written at %d", h.maxData, h.writeOff)
			}
			view := &Tier{path: path, readOnly: true}
			defer func() { _ = view.Close() }()
			if err := view.reloadReadOnly(); err == nil {
				t.Fatalf("a read-only view accepted a ring of %d bytes written at %d", h.maxData, h.writeOff)
			}
			// addons/inspect_tier.py refuses it too.
			python, err := exec.LookPath("python3")
			if err != nil {
				return
			}
			out, err := exec.Command(python, "-I", "../../addons/inspect_tier.py", path).CombinedOutput()
			if err == nil || !strings.Contains(string(out), "invalid header") {
				t.Fatalf("the inspector read a ring of %d bytes written at %d: %v\n%s", h.maxData, h.writeOff, err, out)
			}
		})
	}
	if _, err := OpenTier(filepath.Join(t.TempDir(), "tier_0.dat"), maxTierData+headerSize+1); err == nil {
		t.Fatal("OpenTier created a ring larger than maxTierData")
	}
}

// TestWriteResumesAtTheEndOfTheData: a header whose write offset lies past the
// end of the file (a crash wrote it but not the records it counts) is
// accepted, but writing resumes where the data ends. Written at the offset, a
// record would leave a hole every scan resyncs across, as long as a crafted
// offset makes it: here half a terabyte.
func TestWriteResumesAtTheEndOfTheData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tier_0.dat")
	base := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	h := tierHeader{
		flags: flagHeaderHasTail, version: codecVersion2, maxData: 1 << 40, writeOff: 1 << 39,
		count: 1, oldestNano: base.UnixNano(), newestNano: base.UnixNano(),
	}
	if err := os.WriteFile(path, h.bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	tier, err := OpenTier(path, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tier.Close() }()
	if err := tier.Write(varSample(base.Add(time.Second), 2)); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > 64<<10 {
		t.Fatalf("one record grew the file to %d bytes: it was written past a hole", info.Size())
	}
	got, err := tier.ReadRange(base, base.Add(time.Hour))
	if err != nil || len(got) != 1 {
		t.Fatalf("ReadRange returned %d records, err %v; want the one written", len(got), err)
	}
}

func fuzzSeedTier(f *testing.F, writes int) []byte {
	path := filepath.Join(f.TempDir(), "tier_0.dat")
	tier, err := OpenTier(path, fuzzTierSize)
	if err != nil {
		f.Fatal(err)
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < writes; i++ {
		ts := base.Add(time.Duration(i) * time.Second)
		if i%7 == 3 {
			ts = ts.Add(-30 * time.Second) // the clock stepped back
		}
		if err := tier.Write(varSample(ts, 1+i%3)); err != nil {
			f.Fatal(err)
		}
	}
	if err := tier.Close(); err != nil {
		f.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		f.Fatal(err)
	}
	return b
}

// fuzzTierSize is the ring FuzzOpenTier opens its files with: small enough
// that its seeds wrap.
const fuzzTierSize = 8<<10 + headerSize

// memWriterAt is an in-memory snapshot destination.
type memWriterAt struct{ b []byte }

func (m *memWriterAt) WriteAt(p []byte, off int64) (int, error) {
	if need := int(off) + len(p); need > len(m.b) {
		m.b = append(m.b, make([]byte, need-len(m.b))...)
	}
	copy(m.b[off:], p)
	return len(p), nil
}

// FuzzOpenTier opens arbitrary tier files through every path that reads one:
// the header inspectors, a read-only view and the owner, which then writes,
// snapshots, closes and reopens the file. A corrupt or crafted file may be
// refused, but must not panic, hang, or grow the file far past its data. Its
// corpus holds the crasher behind L2(c), a write offset near 2^63.
func FuzzOpenTier(f *testing.F) {
	add := func(file []byte) {
		h := parseTierHeader(file)
		f.Add(h.flags, h.version, h.maxData, h.writeOff, h.count, h.oldestNano, h.newestNano, h.oldestOff, file[headerSize:])
	}
	add(fuzzSeedTier(f, 5))
	add(fuzzSeedTier(f, 40))  // wrapped
	add(fuzzSeedTier(f, 101)) // wrapped more than once
	// A legacy JSON tier, and a write offset past the data in a huge ring.
	f.Add(uint32(0), uint64(1), uint64(4096), uint64(20), uint64(2), int64(0), int64(0), uint64(0), []byte(`{"ts":"2026-01-01T00:00:00Z"}`))
	f.Add(flagHeaderHasTail, uint64(codecVersion2), uint64(1<<40), uint64(1<<39), uint64(1), int64(0), int64(0), uint64(0), []byte{})

	earliest, far := time.Unix(0, -1<<62), time.Unix(0, 1<<62)
	f.Fuzz(func(t *testing.T, flags uint32, version, maxData, writeOff, count uint64, oldest, newest int64, oldestOff uint64, data []byte) {
		if len(data) > 64<<10 {
			return
		}
		h := tierHeader{flags, version, maxData, writeOff, count, oldest, newest, oldestOff}
		path := filepath.Join(t.TempDir(), "tier_0.dat")
		if err := os.WriteFile(path, append(h.bytes(), data...), 0o600); err != nil {
			t.Fatal(err)
		}
		recent := time.Unix(0, newest)

		_, _ = InspectTierFile(path)
		_, _ = InspectLatestTierSample(path)

		view := &Tier{path: path, readOnly: true}
		if err := view.reloadReadOnly(); err == nil {
			_, _ = view.ReadRange(earliest, far)
			_, _ = view.scanRange(recent.Add(-5*time.Second), recent, 2, func([]*AggregatedSample) error { return nil })
			_, _ = view.ReadLatest(3)
			_ = view.reloadReadOnly()
			_, _ = view.ReadRange(earliest, far)
		}
		_ = view.Close()

		tier, err := OpenTier(path, fuzzTierSize)
		if err != nil {
			return
		}
		_, _ = tier.ReadRange(earliest, far)
		_, _ = tier.ReadLatest(5)
		_ = tier.OldestDuration()
		_, _ = tier.scanRange(recent.Add(-5*time.Second), recent, 2, func([]*AggregatedSample) error { return nil })
		for i := 0; i < 3; i++ {
			_ = tier.Write(varSample(recent.Add(time.Duration(i+1)*time.Second), 2))
		}
		if info, err := os.Stat(path); err == nil && info.Size() > headerSize+int64(len(data))+64<<10 {
			t.Fatalf("three records grew a %d-byte file to %d bytes", headerSize+len(data), info.Size())
		}
		_, _ = tier.ReadRange(earliest, far)
		_, _ = tier.SnapshotTo(&memWriterAt{})
		_ = tier.Close()

		// Reopen: adopts, or rejects, the sidecar Close just saved.
		tier, err = OpenTier(path, fuzzTierSize)
		if err != nil {
			return
		}
		_, _ = tier.ReadRange(earliest, far)
		_, _ = tier.ReadLatest(5)
		_ = tier.Close()
	})
}
