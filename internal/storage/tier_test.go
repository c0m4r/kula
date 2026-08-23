package storage

import (
	"encoding/binary"
	"fmt"
	"os"
	"testing"
	"time"

	"kula/internal/collector"
)

// makeVarSample builds a sample whose ENCODED SIZE grows with nIfaces. Real
// systems do this naturally over their lifetime: a NIC/container/disk/GPU
// appears or disappears, an app's metrics toggle, mock data is replaced by
// real data, etc. The ring buffer must tolerate records of differing lengths
// across a wrap.
func makeVarSample(ts time.Time, nIfaces int) *collector.Sample {
	s := &collector.Sample{
		Timestamp: ts,
		CPU:       collector.CPUStats{Total: collector.CPUCoreStats{Usage: 42}},
		Memory:    collector.MemoryStats{Total: 1024, Used: 512},
		System:    collector.SystemStats{Hostname: "host"},
	}
	for i := 0; i < nIfaces; i++ {
		s.Network.Interfaces = append(s.Network.Interfaces, collector.NetInterface{
			Name:   fmt.Sprintf("eth%d", i),
			RxMbps: float64(i),
		})
	}
	return s
}

func varSample(ts time.Time, nIfaces int) *AggregatedSample {
	return &AggregatedSample{Timestamp: ts, Duration: time.Second, Data: makeVarSample(ts, nIfaces)}
}

// fullReadChecked returns ReadRange over the whole history up to newest and
// asserts the result is a single contiguous (1s-spaced) run ending at newest
// with its oldest matching OldestTimestamp(). It returns the record count so
// callers can detect history collapse.
func fullReadChecked(t *testing.T, tier *Tier, base, newest time.Time) int {
	t.Helper()
	got, err := tier.ReadRange(base, newest.Add(time.Second))
	if err != nil {
		t.Fatalf("ReadRange: %v", err)
	}
	if len(got) == 0 {
		t.Fatalf("ReadRange returned nothing (newest=%s)", newest.Format(time.RFC3339))
	}
	if !got[len(got)-1].Timestamp.Equal(newest) {
		t.Fatalf("ReadRange last=%s, want newest=%s",
			got[len(got)-1].Timestamp.Format(time.RFC3339), newest.Format(time.RFC3339))
	}
	for j := 1; j < len(got); j++ {
		if gap := got[j].Timestamp.Sub(got[j-1].Timestamp); gap != time.Second {
			t.Fatalf("history hole/disorder at %d: %s -> %s (gap %s)",
				j, got[j-1].Timestamp.Format(time.RFC3339),
				got[j].Timestamp.Format(time.RFC3339), gap)
		}
	}
	if o := tier.OldestTimestamp(); !o.Equal(got[0].Timestamp) {
		t.Fatalf("OldestTimestamp()=%s but oldest readable record=%s",
			o.Format(time.RFC3339), got[0].Timestamp.Format(time.RFC3339))
	}
	return len(got)
}

// downgradeToPreFix rewrites a tier header to look like one written by the OLD
// (pre-tail-tracking) binary: it clears the header-flags word and zeroes the
// oldestOff field, leaving the data region and writeOff/count untouched. This
// is exactly the on-disk shape a node carries when it upgrades from a release
// before the tail-tracking change.
func downgradeToPreFix(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		t.Fatalf("open for downgrade: %v", err)
	}
	defer func() { _ = f.Close() }()
	hdr := make([]byte, headerSize)
	if _, err := f.ReadAt(hdr, 0); err != nil {
		t.Fatalf("read header: %v", err)
	}
	binary.LittleEndian.PutUint32(hdr[4:8], 0)   // clear hasTail/wrapped flags
	binary.LittleEndian.PutUint64(hdr[56:64], 0) // clear oldestOff
	if _, err := f.WriteAt(hdr, 0); err != nil {
		t.Fatalf("write downgraded header: %v", err)
	}
}

// TestWrapMisalignment is the original repro: fill+wrap with small records,
// then write a few LARGER ones. Before the fix this dropped on-disk history
// (full read collapsed 187 -> 2 records) and reported a nonsense oldest
// timestamp. After the fix the tail offset is tracked, so neither happens.
func TestWrapMisalignment(t *testing.T) {
	dir := t.TempDir()
	tier, err := OpenTier(dir+"/tier_0.dat", 64*1024)
	if err != nil {
		t.Fatalf("OpenTier: %v", err)
	}
	defer func() { _ = tier.Close() }()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	i := 0
	for ; i < 5000 && !tier.wrapped; i++ {
		if err := tier.Write(varSample(base.Add(time.Duration(i)*time.Second), 1)); err != nil {
			t.Fatalf("Write small(%d): %v", i, err)
		}
	}
	if !tier.wrapped {
		t.Fatal("precondition: tier did not wrap during phase 1")
	}
	wrapAt := i
	baseN := fullReadChecked(t, tier, base, base.Add(time.Duration(i-1)*time.Second))
	t.Logf("baseline after wrap (small records): full read = %d contiguous records", baseN)

	for ; i < wrapAt+8; i++ {
		ts := base.Add(time.Duration(i) * time.Second)
		if err := tier.Write(varSample(ts, 8)); err != nil {
			t.Fatalf("Write large(%d): %v", i, err)
		}
		n := fullReadChecked(t, tier, base, ts)
		// A few larger writes drop a few old records, never most of them.
		if n < baseN-2*(i-wrapAt+1) {
			t.Errorf("history collapsed after %d larger writes: %d records (baseline %d)",
				i-wrapAt+1, n, baseN)
		}
	}
}

// TestWrapTailTrackingVariableSize hammers the ring through many wraps with a
// constantly changing record size and checks the read/oldest invariants after
// every write.
func TestWrapTailTrackingVariableSize(t *testing.T) {
	dir := t.TempDir()
	tier, err := OpenTier(dir+"/tier_0.dat", 64*1024)
	if err != nil {
		t.Fatalf("OpenTier: %v", err)
	}
	defer func() { _ = tier.Close() }()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const N = 5000
	minReturned := 1 << 30
	wrapped := false
	for i := 0; i < N; i++ {
		ts := base.Add(time.Duration(i) * time.Second)
		// Size churns between 1 and 10 interfaces; coprime stride avoids any
		// periodic re-alignment that could mask the bug.
		if err := tier.Write(varSample(ts, 1+(i*7)%10)); err != nil {
			t.Fatalf("Write(%d): %v", i, err)
		}
		if tier.wrapped {
			wrapped = true
		}
		if !wrapped {
			continue
		}
		n := fullReadChecked(t, tier, base, ts)
		if n < minReturned {
			minReturned = n
		}
	}
	if !wrapped {
		t.Fatal("precondition: tier never wrapped")
	}
	// With correct tail tracking the buffer always holds a healthy slice of
	// history; the bug collapsed it toward 1-2 records.
	if minReturned < 20 {
		t.Errorf("history collapsed across wraps: min full-read = %d records", minReturned)
	}
	t.Logf("survived %d writes / many wraps; min full-read = %d records", N, minReturned)
}

// TestWrapTailPersistsAcrossReopen verifies the tail offset + wrap state + oldest
// timestamp survive a close/reopen and that reads return the same data.
func TestWrapTailPersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/tier_0.dat"
	tier, err := OpenTier(path, 64*1024)
	if err != nil {
		t.Fatalf("OpenTier: %v", err)
	}

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const N = 3000
	for i := 0; i < N; i++ {
		if err := tier.Write(varSample(base.Add(time.Duration(i)*time.Second), 1+i%6)); err != nil {
			t.Fatalf("Write(%d): %v", i, err)
		}
	}
	if !tier.wrapped {
		t.Fatal("precondition: tier did not wrap")
	}
	newest := base.Add(time.Duration(N-1) * time.Second)
	wantOldest := tier.OldestTimestamp()
	wantOff := tier.oldestOff
	before := fullReadChecked(t, tier, base, newest)
	if err := tier.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	tier2, err := OpenTier(path, 64*1024)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = tier2.Close() }()

	if !tier2.wrapped {
		t.Error("wrapped state not persisted across reopen")
	}
	if tier2.oldestOff != wantOff {
		t.Errorf("oldestOff = %d after reopen, want %d", tier2.oldestOff, wantOff)
	}
	if !tier2.OldestTimestamp().Equal(wantOldest) {
		t.Errorf("OldestTimestamp() = %s after reopen, want %s",
			tier2.OldestTimestamp().Format(time.RFC3339), wantOldest.Format(time.RFC3339))
	}
	if after := fullReadChecked(t, tier2, base, newest); after != before {
		t.Errorf("full read returned %d records after reopen, want %d", after, before)
	}
}

// TestUpgradePreFixUniformWrappedKeepsAllData is the regression test for the
// "100% → 5%, data lost on upgrade" report. A stable node writes UNIFORM-size
// records, fills and wraps the ring, then upgrades to the tail-tracking binary.
// Opening that pre-fix file must keep it wrapped and return byte-for-byte the
// same history — never silently abandon the [writeOff, maxData) segment.
func TestUpgradePreFixUniformWrappedKeepsAllData(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/tier_0.dat"
	tier, err := OpenTier(path, 64*1024)
	if err != nil {
		t.Fatalf("OpenTier: %v", err)
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const N = 4000
	for i := 0; i < N; i++ {
		// Constant interface count → uniform record size (a stable real node).
		if err := tier.Write(varSample(base.Add(time.Duration(i)*time.Second), 2)); err != nil {
			t.Fatalf("Write(%d): %v", i, err)
		}
	}
	if !tier.wrapped {
		t.Fatal("precondition: tier did not wrap")
	}
	newest := base.Add(time.Duration(N-1) * time.Second)
	wantN := fullReadChecked(t, tier, base, newest)
	wantOldest := tier.OldestTimestamp()
	wantWriteOff := tier.writeOff
	if err := tier.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	t.Logf("baseline: %d records, oldest %s, writeOff %d (~%.0f%% of buffer)",
		wantN, wantOldest.Format(time.RFC3339), wantWriteOff, float64(wantWriteOff)/float64(tier.maxData)*100)

	downgradeToPreFix(t, path)

	tier2, err := OpenTier(path, 64*1024)
	if err != nil {
		t.Fatalf("reopen pre-fix file: %v", err)
	}
	defer func() { _ = tier2.Close() }()

	if !tier2.wrapped {
		t.Fatalf("DATA LOSS: a full wrapped pre-fix tier opened as NOT wrapped "+
			"(writeOff=%d) — the %d-record old segment was abandoned",
			tier2.writeOff, wantN)
	}
	gotN := fullReadChecked(t, tier2, base, newest)
	if gotN != wantN {
		t.Errorf("DATA LOSS: full read returned %d records after upgrade, want %d", gotN, wantN)
	}
	if !tier2.OldestTimestamp().Equal(wantOldest) {
		t.Errorf("oldest changed after upgrade: got %s want %s",
			tier2.OldestTimestamp().Format(time.RFC3339), wantOldest.Format(time.RFC3339))
	}
}

// TestUpgradePreFixWrappedNeverWipes covers the harder variable-size case: even
// when the old "oldest == writeOff" assumption is imperfect, the upgrade must
// not throw the ring away. It must stay wrapped (preserving the on-disk old
// segment) and must keep collecting correctly afterwards.
func TestUpgradePreFixWrappedNeverWipes(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/tier_0.dat"
	tier, err := OpenTier(path, 64*1024)
	if err != nil {
		t.Fatalf("OpenTier: %v", err)
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Write until the ring is actually in the wrapped state. With variable-size
	// records the tail periodically catches the head, leaving the ring
	// contiguous (wrapped=false); which write count lands in which state
	// depends on the record layout, so keep going instead of pinning a magic
	// count that any codec change would silently invalidate.
	const minWrites, maxWrites = 3000, 12000
	n := 0
	for n < maxWrites {
		if err := tier.Write(varSample(base.Add(time.Duration(n)*time.Second), 1+n%5)); err != nil {
			t.Fatalf("Write(%d): %v", n, err)
		}
		n++
		if n >= minWrites && tier.wrapped {
			break
		}
	}
	if !tier.wrapped {
		t.Fatalf("precondition: tier did not wrap after %d writes", n)
	}
	wantWriteOff, wantCount := tier.writeOff, tier.count
	if err := tier.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	downgradeToPreFix(t, path)

	tier2, err := OpenTier(path, 64*1024)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = tier2.Close() }()

	if !tier2.wrapped {
		t.Errorf("variable-size pre-fix wrapped tier was wiped to non-wrapped on upgrade")
	}
	if tier2.writeOff != wantWriteOff || tier2.count != wantCount {
		t.Errorf("writeOff/count changed on upgrade: got (%d,%d) want (%d,%d)",
			tier2.writeOff, tier2.count, wantWriteOff, wantCount)
	}
	// The head segment is always cleanly readable, and reads must keep working.
	now := base.Add(time.Duration(n) * time.Second)
	if err := tier2.Write(varSample(now, 4)); err != nil {
		t.Fatalf("post-upgrade write: %v", err)
	}
	if _, err := tier2.ReadRange(base, now.Add(time.Second)); err != nil {
		t.Fatalf("post-upgrade ReadRange: %v", err)
	}
}

// TestUpgradeCorruptNewFormatTailNeverWipes covers a new-format file whose
// persisted tail offset is corrupt (the symptom of the earlier runaway-tail
// bug). Validation must reject the bad offset, but the ring must still be kept
// by falling back to the old writeOff-based layout — never wiped.
func TestUpgradeCorruptNewFormatTailNeverWipes(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/tier_0.dat"
	tier, err := OpenTier(path, 64*1024)
	if err != nil {
		t.Fatalf("OpenTier: %v", err)
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const N = 4000
	for i := 0; i < N; i++ {
		if err := tier.Write(varSample(base.Add(time.Duration(i)*time.Second), 2)); err != nil {
			t.Fatalf("Write(%d): %v", i, err)
		}
	}
	if !tier.wrapped {
		t.Fatal("precondition: tier did not wrap")
	}
	newest := base.Add(time.Duration(N-1) * time.Second)
	wantN := fullReadChecked(t, tier, base, newest)
	maxData := tier.maxData
	if err := tier.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Keep the hasTail+wrapped flags but poison the tail offset out of range,
	// mimicking the runaway-tail corruption an earlier build could persist.
	f, err := os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		t.Fatalf("open for poisoning: %v", err)
	}
	hdr := make([]byte, headerSize)
	if _, err := f.ReadAt(hdr, 0); err != nil {
		t.Fatalf("read header: %v", err)
	}
	binary.LittleEndian.PutUint64(hdr[56:64], uint64(maxData)) // out-of-range oldestOff
	if _, err := f.WriteAt(hdr, 0); err != nil {
		t.Fatalf("write poisoned header: %v", err)
	}
	_ = f.Close()

	tier2, err := OpenTier(path, 64*1024)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = tier2.Close() }()

	if !tier2.wrapped {
		t.Fatalf("DATA LOSS: corrupt-tail file opened as NOT wrapped — old segment abandoned")
	}
	if gotN := fullReadChecked(t, tier2, base, newest); gotN != wantN {
		t.Errorf("corrupt-tail recovery changed record count: got %d want %d", gotN, wantN)
	}
}

// TestOpenTierCorruptHeaderFailsLoudWithoutWiping verifies that a corrupt header
// makes OpenTier fail loudly and leaves the file untouched, instead of
// reinitializing in place (which silently abandoned, then overwrote, intact data).
func TestOpenTierCorruptHeaderFailsLoudWithoutWiping(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/tier_0.dat"

	tier, err := OpenTier(path, 64*1024)
	if err != nil {
		t.Fatalf("OpenTier: %v", err)
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 50; i++ {
		if err := tier.Write(varSample(base.Add(time.Duration(i)*time.Second), 2)); err != nil {
			t.Fatalf("Write(%d): %v", i, err)
		}
	}
	if err := tier.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	orig, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read original: %v", err)
	}

	// Corrupt only the 4-byte magic.
	f, err := os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		t.Fatalf("open to corrupt: %v", err)
	}
	if _, err := f.WriteAt([]byte("XXXX"), 0); err != nil {
		t.Fatalf("corrupt magic: %v", err)
	}
	_ = f.Close()

	if _, err := OpenTier(path, 64*1024); err == nil {
		t.Fatal("OpenTier on a corrupt header returned nil; it must fail loudly, not reinitialize")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if len(after) != len(orig) {
		t.Fatalf("file size changed %d -> %d; OpenTier must not rewrite a corrupt file", len(orig), len(after))
	}
	// Everything past the 4 magic bytes we deliberately corrupted must be
	// byte-identical — proving OpenTier neither zeroed the header nor touched
	// the data region.
	for i := 4; i < len(orig); i++ {
		if orig[i] != after[i] {
			t.Fatalf("OpenTier mutated the file at offset %d — data was not preserved", i)
		}
	}
}

// TestReadRecoversAfterUncleanShutdownHole reproduces the field bug where an
// unclean reboot left a zero-filled hole in the middle of the active segment
// (the header's count/writeOff outran payloads that were never fsynced). The old
// reader treated the first zero-length record as end-of-data and `break`ed,
// hiding every sample written after the hole — so the whole raw tier looked
// empty for any recent window and queries silently fell through to a coarser
// tier. The reader must instead skip the hole and return the surviving records.
func TestReadRecoversAfterUncleanShutdownHole(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/tier_0.dat"
	tier, err := OpenTier(path, 1<<20)
	if err != nil {
		t.Fatalf("OpenTier: %v", err)
	}

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const N = 10
	for i := 0; i < N; i++ {
		// Uniform record size so the hole we punch is record-aligned, exactly
		// like the on-disk file from the incident.
		if err := tier.Write(varSample(base.Add(time.Duration(i)*time.Second), 2)); err != nil {
			t.Fatalf("Write(%d): %v", i, err)
		}
	}
	recordLen := tier.writeOff / N
	if recordLen*N != tier.writeOff {
		t.Fatalf("records not uniform: writeOff=%d not divisible by %d", tier.writeOff, N)
	}

	// Simulate the unflushed payloads: zero out records [3,4,5] in place while
	// leaving the header's writeOff/count pointing past them (the phantom-record
	// state an unclean shutdown leaves behind).
	hole := make([]byte, 3*recordLen)
	if _, err := tier.file.WriteAt(hole, headerSize+3*recordLen); err != nil {
		t.Fatalf("punching hole: %v", err)
	}

	newest := base.Add(time.Duration(N-1) * time.Second)
	got, err := tier.ReadRange(base, newest.Add(time.Second))
	if err != nil {
		t.Fatalf("ReadRange: %v", err)
	}
	// Records 0,1,2 (pre-hole) and 6,7,8,9 (post-hole) survive; 3,4,5 are gone.
	if len(got) != 7 {
		t.Fatalf("ReadRange returned %d records, want 7 (3 pre-hole + 4 post-hole)", len(got))
	}
	wantTS := []int{0, 1, 2, 6, 7, 8, 9}
	for i, w := range wantTS {
		want := base.Add(time.Duration(w) * time.Second)
		if !got[i].Timestamp.Equal(want) {
			t.Errorf("record %d ts=%s, want %s", i, got[i].Timestamp.Format(time.RFC3339), want.Format(time.RFC3339))
		}
	}

	// A query whose window starts entirely AFTER the hole must still find the
	// post-hole data — this is the exact symptom from the server (recent windows
	// returned nothing from the raw tier).
	postHole, err := tier.ReadRange(base.Add(6*time.Second), newest.Add(time.Second))
	if err != nil {
		t.Fatalf("ReadRange post-hole: %v", err)
	}
	if len(postHole) != 4 {
		t.Fatalf("post-hole window returned %d records, want 4", len(postHole))
	}

	// ReadLatest must see past the hole too (warmLatestCache relies on it).
	latest, err := tier.ReadLatest(1)
	if err != nil {
		t.Fatalf("ReadLatest: %v", err)
	}
	if len(latest) != 1 || !latest[0].Timestamp.Equal(newest) {
		t.Fatalf("ReadLatest(1) = %v, want newest %s", latest, newest.Format(time.RFC3339))
	}
	_ = tier.Close()
}

// TestRecordHeaderSaneRejectsMisalignedRead guards the off-by-one that an early
// version of the resync logic hit on the real incident file: a byte-scan can
// stop one byte before the true record boundary, where a stray length prefix
// plus a garbage timestamp happen to look plausible. resync would then land
// inside a record and read megabytes of garbage. The strict kind-byte check on
// v2 records is what prevents this.
func TestRecordHeaderSaneRejectsMisalignedRead(t *testing.T) {
	dir := t.TempDir()
	tier, err := OpenTier(dir+"/tier_0.dat", 1<<20)
	if err != nil {
		t.Fatalf("OpenTier: %v", err)
	}
	defer func() { _ = tier.Close() }()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const N = 5
	for i := 0; i < N; i++ {
		if err := tier.Write(varSample(base.Add(time.Duration(i)*time.Second), 2)); err != nil {
			t.Fatalf("Write(%d): %v", i, err)
		}
	}

	// Read the first record's on-disk bytes (4-byte length prefix + payload).
	recLen := tier.writeOff / N
	buf := make([]byte, recLen)
	if _, err := tier.file.ReadAt(buf, headerSize); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}

	if _, ok := tier.recordHeaderSane(buf); !ok {
		t.Fatal("recordHeaderSane rejected a real v2 record header (aligned)")
	}
	// One leading zero byte, exactly the trailing edge of a zero hole: the kind
	// byte no longer sits at offset 4, so a v2 header must be rejected.
	mis := append([]byte{0x00}, buf...)
	if _, ok := tier.recordHeaderSane(mis); ok {
		t.Fatal("recordHeaderSane accepted a 1-byte-misaligned read; resync would land inside a record")
	}
}

// TestTierWriteNilReturnsError verifies Write(nil) returns an error rather than
// panicking the collection loop.
func TestTierWriteNilReturnsError(t *testing.T) {
	dir := t.TempDir()
	tier, err := OpenTier(dir+"/tier_0.dat", 64*1024)
	if err != nil {
		t.Fatalf("OpenTier: %v", err)
	}
	defer func() { _ = tier.Close() }()
	if err := tier.Write(nil); err == nil {
		t.Error("Write(nil) returned nil; expected an error (and no panic)")
	}
}
