package storage

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"testing"
	"time"

	"kula/internal/config"
)

// checksumRollup is a rollup record with Data, Min, Max and the MeanStats
// trailer, the shape that rejects trailing bytes and so cannot carry a
// checksum inside the codec.
func checksumRollup(ts time.Time) *AggregatedSample {
	a := makeSampleFull(ts)
	a.Data.CPU.Total.Usage = 37.5
	a.Min = makeSampleFull(ts).Data
	a.Max = makeSampleFull(ts).Data
	a.AggregationVersion = currentAggregationVersion
	a.MeanWeightsComplete = true
	a.MeanStats = map[string]meanAccumulator{"sample.cpu.total.usage": {Sum: 60, Weight: 60}}
	return a
}

// checksumSample is a raw sample whose values differ per record i, so bytes
// from one record copied into another change what it decodes to.
func checksumSample(ts time.Time, i int) *AggregatedSample {
	data := makeVarSample(ts, 2)
	data.CPU.Total.Usage = float64(10 + i)
	for j := range data.Network.Interfaces {
		data.Network.Interfaces[j].RxMbps = float64(100*i + j)
	}
	return &AggregatedSample{Timestamp: ts, Duration: time.Second, Data: data}
}

// writeUncheckedRecord appends a in the format versions before 0.22.0 wrote:
// recordKindBinary, no checksum.
func writeUncheckedRecord(t *testing.T, tier *Tier, a *AggregatedSample) {
	t.Helper()
	body, err := encodeSample(a)
	if err != nil {
		t.Fatal(err)
	}
	if err := tier.writePayload(append([]byte{recordKindBinary}, body...), a.Timestamp); err != nil {
		t.Fatal(err)
	}
}

func TestChecksummedRecordsRoundTrip(t *testing.T) {
	ts := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	tier := &Tier{}
	for name, a := range map[string]*AggregatedSample{
		"raw":    checksumSample(ts, 1),
		"rollup": checksumRollup(ts),
	} {
		body, err := encodeSample(a)
		if err != nil {
			t.Fatal(err)
		}
		want, err := decodeSample(body)
		if err != nil {
			t.Fatal(err)
		}
		disk, err := encodeSampleV(a)
		if err != nil {
			t.Fatal(err)
		}
		if disk[0] != recordKindChecksummed || !bytes.Equal(disk[1:len(disk)-recordChecksumSize], body) {
			t.Fatalf("%s: on-disk payload is not [0x03][encoded sample][crc32]", name)
		}
		got, err := tier.readRecord(disk)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: checksummed record decoded to %+v, %v", name, got, err)
		}
		// Records written before checksums still decode, unverified.
		old, err := tier.readRecord(append([]byte{recordKindBinary}, body...))
		if err != nil || !reflect.DeepEqual(old, want) {
			t.Fatalf("%s: 0x02 record decoded to %+v, %v", name, old, err)
		}
	}
}

func TestChecksumRejectsCorruptRecords(t *testing.T) {
	disk, err := encodeSampleV(checksumRollup(time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatal(err)
	}
	tier := &Tier{}
	// The kind byte selects the format, so every byte after it is covered.
	for i := 1; i < len(disk); i++ {
		for _, bit := range []byte{0x01, 0x80} {
			bad := bytes.Clone(disk)
			bad[i] ^= bit
			if _, err := tier.readRecord(bad); !errors.Is(err, errRecordChecksum) {
				t.Fatalf("byte %d ^ %#x: readRecord error %v, want a checksum mismatch", i, bit, err)
			}
		}
	}
	// A record read with the wrong extent fails too: the prefix is covered.
	if _, err := tier.readRecord(disk[:len(disk)-1]); err == nil {
		t.Fatal("accepted a record one byte short")
	}
	if _, err := tier.readRecord(append(bytes.Clone(disk), 0)); err == nil {
		t.Fatal("accepted a record one byte long")
	}
	if _, err := tier.readRecord([]byte{recordKindChecksummed, 0, 0, 0}); err == nil {
		t.Fatal("accepted a record too short for its trailer")
	}
}

// TestTierReadsRecordsWrittenBeforeChecksums covers an upgraded tier: the
// records an older version wrote are followed by checksummed ones, and every
// reader, the seek-index walk included, returns both.
func TestTierReadsRecordsWrittenBeforeChecksums(t *testing.T) {
	path := t.TempDir() + "/tier_0.dat"
	tier, err := OpenTier(path, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	const n = 10
	for i := 0; i < n; i++ {
		a := checksumSample(base.Add(time.Duration(i)*time.Second), i)
		if i < n/2 {
			writeUncheckedRecord(t, tier, a)
		} else if err := tier.Write(a); err != nil {
			t.Fatal(err)
		}
	}
	if err := tier.Close(); err != nil {
		t.Fatal(err)
	}
	// Reopen without the saved seek index, so the index is walked.
	if err := os.Remove(seekSidecarPath(path)); err != nil {
		t.Fatal(err)
	}
	tier, err = OpenTier(path, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tier.Close() }()
	if tier.seekFromSidecar {
		t.Fatal("the seek index came from the sidecar; the walk is not exercised")
	}

	got, err := tier.ReadRange(base, base.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	latest, err := tier.ReadLatest(n)
	if err != nil {
		t.Fatal(err)
	}
	for name, samples := range map[string][]*AggregatedSample{"ReadRange": got, "ReadLatest": latest} {
		if len(samples) != n {
			t.Fatalf("%s returned %d records, want %d", name, len(samples), n)
		}
		for i, s := range samples {
			if !s.Timestamp.Equal(base.Add(time.Duration(i)*time.Second)) || s.Data.CPU.Total.Usage != float64(10+i) {
				t.Fatalf("%s record %d: %s CPU %v", name, i, s.Timestamp.Format(time.RFC3339), s.Data.CPU.Total.Usage)
			}
		}
	}
}

// damageFixedBlock overwrites the fixed block of the record at data-region
// offset off, as when the page holding it never reached the disk: zeros, or
// the bytes an older record left there.
func damageFixedBlock(t *testing.T, tier *Tier, off int64, with []byte) {
	t.Helper()
	if with == nil {
		with = make([]byte, fixedBlockSize)
	}
	if _, err := tier.file.WriteAt(with[:fixedBlockSize], headerSize+off+4+1+18); err != nil {
		t.Fatal(err)
	}
}

func readFixedBlock(t *testing.T, tier *Tier, off int64) []byte {
	t.Helper()
	block := make([]byte, fixedBlockSize)
	if _, err := tier.file.ReadAt(block, headerSize+off+4+1+18); err != nil {
		t.Fatal(err)
	}
	return block
}

// TestTornRecordsAreSkipped damages two records the way an unclean shutdown
// can: one fixed block left zeroed, one holding an older record's bytes. Both
// still decode without the checksum, into wrong values; with it every reader
// skips them.
func TestTornRecordsAreSkipped(t *testing.T) {
	tier, err := OpenTier(t.TempDir()+"/tier_0.dat", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tier.Close() }()
	base := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	const n = 8
	offsets := make([]int64, n)
	for i := 0; i < n; i++ {
		offsets[i] = tier.writeOff
		if err := tier.Write(checksumSample(base.Add(time.Duration(i)*time.Second), i)); err != nil {
			t.Fatal(err)
		}
	}
	damageFixedBlock(t, tier, offsets[3], nil)
	damageFixedBlock(t, tier, offsets[6], readFixedBlock(t, tier, offsets[1]))

	// Without the checksum both decode "successfully": the damage is silent.
	for i, want := range map[int]float64{3: 0, 6: 11} {
		payload := make([]byte, offsets[i+1]-offsets[i]-4)
		if _, err := tier.file.ReadAt(payload, headerSize+offsets[i]+4); err != nil {
			t.Fatal(err)
		}
		decoded, err := decodeSample(payload[1 : len(payload)-recordChecksumSize])
		if err != nil || decoded.Data.CPU.Total.Usage != want {
			t.Fatalf("record %d without its checksum: %v, %v; the damage should decode", i, decoded, err)
		}
	}

	check := func(name string, got []*AggregatedSample) {
		t.Helper()
		var seen []int
		for _, s := range got {
			i := int(s.Timestamp.Sub(base) / time.Second)
			if s.Data.CPU.Total.Usage != float64(10+i) {
				t.Fatalf("%s: record %d decoded CPU %v, want %v", name, i, s.Data.CPU.Total.Usage, 10+i)
			}
			seen = append(seen, i)
		}
		if want := []int{0, 1, 2, 4, 5, 7}; !reflect.DeepEqual(seen, want) {
			t.Fatalf("%s returned records %v, want %v", name, seen, want)
		}
	}
	got, err := tier.ReadRange(base, base.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	check("ReadRange", got)
	latest, err := tier.ReadLatest(n)
	if err != nil {
		t.Fatal(err)
	}
	check("ReadLatest", latest)
}

// TestRestartSkipsATornRecord covers the records an unclean shutdown can tear:
// the newest ones, which restart recovery reads back into the latest-sample
// cache and replays into the tier-1 window. A torn one must reach neither,
// nor a read-only store.
func TestRestartSkipsATornRecord(t *testing.T) {
	store := newMultiTierStore(t)
	base := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	writeOffsetSamples(t, store, base, 0, 29, 0, func(int) float64 { return 7 })
	off := store.tiers[0].writeOff
	writeOffsetSamples(t, store, base, 30, 30, 0, func(int) float64 { return 7 })
	damageFixedBlock(t, store.tiers[0], off, nil) // CPU usage reads back 0
	cfg, dir := store.configs, store.dir
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store = reopenStore(t, dir, cfg)
	defer func() { _ = store.Close() }()
	if want := base.Add(29 * time.Second); store.latestCache == nil || !store.latestCache.Timestamp.Equal(want) {
		t.Fatalf("latest sample after restart = %v, want the last intact record at %s", store.latestCache, want.Format(time.RFC3339))
	}
	if len(store.tier1Buf) == 0 {
		t.Fatal("restart replayed nothing into the tier-1 window")
	}
	for _, pending := range store.tier1Buf {
		if pending.Timestamp.After(base.Add(29*time.Second)) || pending.Data.CPU.Total.Usage != 7 {
			t.Fatalf("tier-1 window holds %s with CPU %v; the torn record was replayed",
				pending.Timestamp.Format(time.RFC3339), pending.Data.CPU.Total.Usage)
		}
	}

	reader, err := OpenReadOnly(config.StorageConfig{Directory: dir, Tiers: cfg})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	for _, s := range queryAll(t, reader, base, base.Add(time.Minute)) {
		if s.Data.CPU.Total.Usage != 7 {
			t.Fatalf("read-only store returned %s with CPU %v", s.Timestamp.Format(time.RFC3339), s.Data.CPU.Total.Usage)
		}
	}
}

// TestResyncVerifiesChecksummedRecords plants, in a hole an unclean shutdown
// left, the header of a rollup an older ring pass wrote there, its body since
// overwritten. Rollups outgrow the resync window, so without the checksum
// resync accepts that header alone, and the scan then skips by its stale
// length over intact records written after the hole.
func TestResyncVerifiesChecksummedRecords(t *testing.T) {
	tier, err := OpenTier(t.TempDir()+"/tier_1.dat", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tier.Close() }()
	base := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	const n = 6
	offsets := make([]int64, n+1)
	for i := 0; i < n; i++ {
		offsets[i] = tier.writeOff
		if err := tier.Write(varSample(base.Add(time.Duration(i)*time.Minute), 120)); err != nil {
			t.Fatal(err)
		}
	}
	offsets[n] = tier.writeOff
	if size := offsets[1] - offsets[0]; size <= 4096 {
		t.Fatalf("records are %d bytes; the case needs records larger than the 4 KiB resync window", size)
	}

	// Record 2 is a hole; 64 bytes into it lies the stale header, whose length
	// ends halfway through record 4.
	if _, err := tier.file.WriteAt(make([]byte, offsets[3]-offsets[2]), headerSize+offsets[2]); err != nil {
		t.Fatal(err)
	}
	stale := offsets[2] + 64
	staleLen := (offsets[4]+offsets[5])/2 - stale - 4
	header := make([]byte, 13)
	binary.LittleEndian.PutUint32(header[0:], uint32(staleLen))
	header[4] = recordKindChecksummed
	binary.LittleEndian.PutUint64(header[5:], uint64(base.Add(-time.Hour).UnixNano()))
	if _, err := tier.file.WriteAt(header, headerSize+stale); err != nil {
		t.Fatal(err)
	}

	got, err := tier.ReadRange(base.Add(-2*time.Hour), base.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var seen []int
	for _, s := range got {
		seen = append(seen, int(s.Timestamp.Sub(base)/time.Minute))
	}
	if want := []int{0, 1, 3, 4, 5}; !reflect.DeepEqual(seen, want) {
		t.Fatalf("ReadRange returned records %v, want %v (only the hole lost)", seen, want)
	}
}

// TestResyncBoundsChecksumWork plants the active pass of a crafted tier file:
// [00 00 0F 00 03] repeated reads, at every fifth byte, as a sane checksummed
// header claiming 960 KiB, the bytes after its kind byte a timestamp in 2004,
// whose trailer never matches. Resync verified every one of them, about a
// megabyte of checksum per five bytes it passed: 2 MiB of it took 14 s to
// walk, 8 MiB 90 s, holding the tier lock. A walk must stay linear and still
// find the records written after the pattern.
func TestResyncBoundsChecksumWork(t *testing.T) {
	tier, err := OpenTier(t.TempDir()+"/tier_0.dat", 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tier.Close() }()
	// An impossible first length starts the resync.
	bomb := append([]byte{0xFF, 0xFF, 0xFF, 0xFF}, bytes.Repeat([]byte{0x00, 0x00, 0x0F, 0x00, 0x03}, (2<<20)/5)...)
	if _, err := tier.file.WriteAt(bomb, headerSize); err != nil {
		t.Fatal(err)
	}
	tier.writeOff = int64(len(bomb))
	base := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	const n = 5
	for i := 0; i < n; i++ {
		if err := tier.Write(varSample(base.Add(time.Duration(i)*time.Second), 2)); err != nil {
			t.Fatal(err)
		}
	}

	type result struct {
		scanned, latest []*AggregatedSample
		err             error
	}
	done := make(chan result, 1)
	go func() {
		var r result
		tier.mu.Lock()
		tier.buildSeekIndex() // walks the whole pass
		tier.mu.Unlock()
		r.scanned, r.err = tier.ReadRange(base.Add(-time.Hour), base.Add(time.Hour))
		if r.err == nil {
			r.latest, r.err = tier.ReadLatest(n)
		}
		done <- r
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatal(r.err)
		}
		if len(r.scanned) != n || len(r.latest) != n || !r.latest[n-1].Timestamp.Equal(base.Add((n-1)*time.Second)) {
			t.Fatalf("scan found %d records and the tail read %d, want the %d written after the pattern", len(r.scanned), len(r.latest), n)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("walks still running after 10 s: resync verifies every crafted candidate")
	}
}

// TestInspectorVerifiesRecordChecksums keeps addons/inspect_tier.py in step:
// it decodes checksummed and older records as the Go reader does, and
// rejects a checksummed record that fails its trailer.
func TestInspectorVerifiesRecordChecksums(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	a := checksumRollup(time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC))
	checked, err := encodeSampleV(a)
	if err != nil {
		t.Fatal(err)
	}
	body, err := encodeSample(a)
	if err != nil {
		t.Fatal(err)
	}
	torn := bytes.Clone(checked)
	torn[len(torn)/2] ^= 0xFF

	for name, tc := range map[string]struct {
		payload []byte
		ok      bool
	}{
		"checksummed": {checked, true},
		"unchecked":   {append([]byte{recordKindBinary}, body...), true},
		"torn":        {torn, false},
	} {
		cmd := exec.Command(python, "-c", `import importlib.util, json, sys
spec = importlib.util.spec_from_file_location("inspect_tier", "../../addons/inspect_tier.py")
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
try:
    decoded = module.decode_v2_record(sys.stdin.buffer.read())
    print(json.dumps({"usage": decoded["data"]["cpu"]["usage"], "stats": len(decoded["mean_stats"])}))
except ValueError as exc:
    print(json.dumps({"error": str(exc)}))`)
		cmd.Stdin = bytes.NewReader(tc.payload)
		out, runErr := cmd.CombinedOutput()
		if runErr != nil {
			t.Fatalf("%s: Python decoder: %v: %s", name, runErr, out)
		}
		var result struct {
			Usage *float64 `json:"usage"`
			Stats int      `json:"stats"`
			Error string   `json:"error"`
		}
		if err := json.Unmarshal(out, &result); err != nil {
			t.Fatalf("%s: Python output %q: %v", name, out, err)
		}
		if !tc.ok {
			if result.Error == "" {
				t.Fatalf("%s: Python decoded a record that fails its checksum", name)
			}
			continue
		}
		want := a.Data.CPU.Total.Usage // 37.5: exact in float32 and at Python's 6 decimals
		if result.Error != "" || result.Usage == nil || *result.Usage != want || result.Stats != len(a.MeanStats) {
			t.Fatalf("%s: Python decoded %s, want CPU %v and %d statistics", name, out, want, len(a.MeanStats))
		}
	}
}
