package storage

import (
	"encoding/binary"
	"hash/crc32"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"kula/internal/config"
)

// indexTestTier fills a 2 MiB tier through several wraps with record sizes
// that keep changing, and one clock step backwards, so seek points span both
// passes and a pass holds non-monotonic timestamps.
func indexTestTier(t *testing.T, path string) (*Tier, []time.Time) {
	t.Helper()
	tier, err := OpenTier(path, 2<<20)
	if err != nil {
		t.Fatalf("OpenTier: %v", err)
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var written []time.Time
	clock := base
	for i := 0; i < 9000; i++ {
		clock = clock.Add(time.Second)
		if i == 8500 {
			clock = clock.Add(-10 * time.Minute) // NTP stepped the clock back
		}
		if err := tier.Write(varSample(clock, 1+(i*7)%23)); err != nil {
			t.Fatalf("Write(%d): %v", i, err)
		}
		written = append(written, clock)
	}
	if !tier.wrapped || tier.writeCycle < 2 {
		t.Fatalf("precondition: tier should have wrapped repeatedly (cycle %d)", tier.writeCycle)
	}
	if len(tier.seek) < 20 {
		t.Fatalf("precondition: only %d seek points", len(tier.seek))
	}
	return tier, written
}

// unindexed runs fn as if the tier had no seek index.
func unindexed[T any](tier *Tier, fn func() T) T {
	tier.mu.Lock()
	ready := tier.seekReady
	tier.seekReady = false
	tier.mu.Unlock()
	defer func() {
		tier.mu.Lock()
		tier.seekReady = ready
		tier.mu.Unlock()
	}()
	return fn()
}

func timestamps(samples []*AggregatedSample) []int64 {
	out := make([]int64, len(samples))
	for i, sample := range samples {
		out[i] = sample.Timestamp.UnixNano()
	}
	return out
}

func assertSeekScansMatch(t *testing.T, tier *Tier, written []time.Time) {
	t.Helper()
	newest := written[len(written)-1]
	froms := []time.Time{written[0].Add(-time.Hour), tier.OldestTimestamp(), newest, newest.Add(time.Second)}
	// Around two dozen starts spread over both passes, the clock step included.
	for i := 0; i < len(written); i += len(written)/24 + 1 {
		froms = append(froms, written[i], written[i].Add(300*time.Millisecond))
	}
	for i, from := range froms {
		to := from.Add(5 * time.Minute)
		if i%4 == 0 {
			to = newest
		}
		got, err := tier.ReadRange(from, to)
		if err != nil {
			t.Fatalf("ReadRange(%s): %v", from, err)
		}
		want := unindexed(tier, func() []*AggregatedSample {
			samples, err := tier.ReadRange(from, to)
			if err != nil {
				t.Fatalf("unindexed ReadRange(%s): %v", from, err)
			}
			return samples
		})
		if !reflect.DeepEqual(timestamps(got), timestamps(want)) {
			t.Fatalf("seeked scan [%s, %s] returned %d records, full scan %d",
				from.Format(time.RFC3339), to.Format(time.RFC3339), len(got), len(want))
		}
	}
}

// TestSeekIndexScansMatchFullScans pins the index's one promise: a seeking scan
// returns exactly what a scan from the segment start returns — across wraps,
// for windows in either pass, and around a backwards clock step.
func TestSeekIndexScansMatchFullScans(t *testing.T) {
	tier, written := indexTestTier(t, filepath.Join(t.TempDir(), "tier_0.dat"))
	defer func() { _ = tier.Close() }()
	assertSeekScansMatch(t, tier, written)

	// Keep writing through another wrap; the maintained index must stay exact.
	clock := written[len(written)-1]
	for i := 0; i < 3000; i++ {
		clock = clock.Add(time.Second)
		if err := tier.Write(varSample(clock, 1+(i*5)%17)); err != nil {
			t.Fatal(err)
		}
		written = append(written, clock)
		if i%750 == 0 {
			assertSeekScansMatch(t, tier, written)
		}
	}
}

// TestSeekIndexBoundsScanReads checks the point of the index: a scan for a
// recent window starts within one spacing (plus a record) of its first record
// instead of at the segment start.
func TestSeekIndexBoundsScanReads(t *testing.T) {
	tier, written := indexTestTier(t, filepath.Join(t.TempDir(), "tier_0.dat"))
	defer func() { _ = tier.Close() }()

	tier.mu.RLock()
	defer tier.mu.RUnlock()
	segments := tier.ringSegments()
	current := segments[len(segments)-1]
	from := written[len(written)-200]
	first := int64(-1)
	tier.walkSegment(current, current.start, func(offset int64, _ uint32, ts int64, ok bool) bool {
		if ok && ts >= from.UnixNano() {
			first = offset
			return false
		}
		return true
	})
	start := tier.seekStart(current, from)
	if first < 0 || start > first {
		t.Fatalf("seek start %d is past the first matching record %d", start, first)
	}
	if start == current.start || first-start > seekIndexSpacing+64<<10 {
		t.Fatalf("seek start %d leaves %d bytes before the first matching record %d", start, first-start, first)
	}
}

func TestSeekIndexReadLatestMatchesFullWalk(t *testing.T) {
	tier, _ := indexTestTier(t, filepath.Join(t.TempDir(), "tier_0.dat"))
	defer func() { _ = tier.Close() }()
	for _, n := range []int{1, 2, 60, 500, int(tier.count) + 5} {
		got, err := tier.ReadLatest(n)
		if err != nil {
			t.Fatal(err)
		}
		want := unindexed(tier, func() []*AggregatedSample {
			samples, err := tier.ReadLatest(n)
			if err != nil {
				t.Fatal(err)
			}
			return samples
		})
		if !reflect.DeepEqual(timestamps(got), timestamps(want)) {
			t.Fatalf("ReadLatest(%d): indexed %d records, full walk %d", n, len(got), len(want))
		}
	}
}

func readSidecarPoints(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(seekSidecarPath(path))
	if err != nil {
		t.Fatalf("sidecar: %v", err)
	}
	return data
}

// TestSeekIndexSidecarLifecycle covers when a saved index is trusted: after a
// clean close it is adopted, and anything that may have changed the tier since
// — further writes without a clean close, corruption, a resize — rebuilds it.
func TestSeekIndexSidecarLifecycle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tier_0.dat")
	tier, written := indexTestTier(t, path)
	passes := func(tier *Tier) [4]int64 {
		return [4]int64{tier.passMax, tier.prevPassMax, tier.backstep, tier.prevBackstep}
	}
	before := passes(tier)
	if before[2] < 0 && before[3] < 0 {
		t.Fatalf("precondition: the clock step left no backstep in either pass: %v", before)
	}
	if err := tier.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "tier_0.idx")); err != nil {
		t.Fatalf("clean close did not save tier_0.idx: %v", err)
	}

	reopen := func(maxSize int64) *Tier {
		t.Helper()
		reopened, err := OpenTier(path, maxSize)
		if err != nil {
			t.Fatalf("OpenTier: %v", err)
		}
		return reopened
	}

	// A clean restart adopts the sidecar and seeks exactly as before.
	tier = reopen(2 << 20)
	if !tier.seekFromSidecar {
		t.Fatal("clean restart rebuilt the index instead of adopting the sidecar")
	}
	if got := passes(tier); got != before {
		t.Fatalf("the sidecar restored pass maxima and backsteps %v, want %v", got, before)
	}
	tier.mu.Lock()
	tier.buildSeekIndex()
	rebuilt := passes(tier)
	tier.mu.Unlock()
	if rebuilt != before {
		t.Fatalf("a rebuilt index has pass maxima and backsteps %v, the maintained one %v", rebuilt, before)
	}
	assertSeekScansMatch(t, tier, written)

	// Writes followed by a crash (no clean close): the sidecar on disk now
	// describes an older header and must not be trusted.
	clock := written[len(written)-1]
	for i := 0; i < 50; i++ {
		clock = clock.Add(time.Second)
		if err := tier.Write(varSample(clock, 3)); err != nil {
			t.Fatal(err)
		}
		written = append(written, clock)
	}
	_ = tier.file.Close()
	tier = reopen(2 << 20)
	if tier.seekFromSidecar {
		t.Fatal("adopted a sidecar saved before later writes")
	}
	assertSeekScansMatch(t, tier, written)
	if err := tier.Close(); err != nil {
		t.Fatal(err)
	}
	saved := readSidecarPoints(t, path)

	corruptions := map[string]func([]byte) []byte{
		"flipped byte": func(b []byte) []byte { b[len(b)/2] ^= 0xff; return b },
		"truncated":    func(b []byte) []byte { return b[:len(b)-7] },
		"empty":        func([]byte) []byte { return nil },
	}
	for name, corrupt := range corruptions {
		if err := os.WriteFile(seekSidecarPath(path), corrupt(append([]byte(nil), saved...)), 0o600); err != nil {
			t.Fatal(err)
		}
		tier = reopen(2 << 20)
		if tier.seekFromSidecar {
			t.Fatalf("%s sidecar was adopted", name)
		}
		assertSeekScansMatch(t, tier, written)
		if err := tier.Close(); err != nil {
			t.Fatal(err)
		}
		saved = readSidecarPoints(t, path)
	}

	// A well-formed sidecar whose points do not match the file (checksum
	// recomputed) is adopted, but its points fail confirmation, so scans fall
	// back to the segment start and stay correct.
	tampered := append([]byte(nil), saved...)
	firstPoint := seekSidecarFixed - 4
	binary.LittleEndian.PutUint64(tampered[firstPoint+8:], binary.LittleEndian.Uint64(tampered[firstPoint+8:])+1)
	for i := firstPoint + seekSidecarPoint; i+seekSidecarPoint <= len(tampered)-4; i += seekSidecarPoint {
		binary.LittleEndian.PutUint64(tampered[i+8:], binary.LittleEndian.Uint64(tampered[i+8:])+1)
	}
	binary.LittleEndian.PutUint32(tampered[len(tampered)-4:], crc32.ChecksumIEEE(tampered[:len(tampered)-4]))
	if err := os.WriteFile(seekSidecarPath(path), tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	tier = reopen(2 << 20)
	if !tier.seekFromSidecar {
		t.Fatal("precondition: a well-formed sidecar should be adopted")
	}
	assertSeekScansMatch(t, tier, written)
	if err := tier.Close(); err != nil {
		t.Fatal(err)
	}

	// Growing max_size rewrites the header: the saved geometry is stale.
	tier = reopen(3 << 20)
	if tier.seekFromSidecar {
		t.Fatal("adopted a sidecar saved for another ring size")
	}
	assertSeekScansMatch(t, tier, written)
	_ = tier.Close()
}

// TestOpenTierRemovesStaleSidecarTemps covers a crash between writing a
// sidecar's temporary file and renaming it: the next open removes the
// leftover and keeps everything else.
func TestOpenTierRemovesStaleSidecarTemps(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "odd [dir]")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "tier_0.dat")
	stale := seekSidecarPath(path) + ".tmp-123456"
	keep := filepath.Join(dir, "tier_1.idx.tmp-123456")
	for _, p := range []string{stale, keep} {
		if err := os.WriteFile(p, []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tier, err := OpenTier(path, 256<<10)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tier.Close() }()
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale temporary sidecar survived the open: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("another tier's file was removed: %v", err)
	}
}

// TestStoreRestartRestoresTailsFromSidecars checks a clean restart reads only
// tails: every tier adopts its saved index, and the latest sample and pending
// rollups come back as they were.
func TestStoreRestartRestoresTailsFromSidecars(t *testing.T) {
	dir := t.TempDir()
	cfg := config.StorageConfig{
		Directory: dir,
		Tiers: []config.TierConfig{
			{Resolution: time.Second, MaxSize: "1MB", MaxBytes: 1 << 20},
			{Resolution: time.Minute, MaxSize: "1MB", MaxBytes: 1 << 20},
			{Resolution: 5 * time.Minute, MaxSize: "1MB", MaxBytes: 1 << 20},
		},
	}
	store, err := NewStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	for i := 1; i <= 60*13+17; i++ {
		if err := store.WriteSample(makeSampleWithCPU(base.Add(time.Duration(i)*time.Second), float64(i%50))); err != nil {
			t.Fatal(err)
		}
	}
	wantLatest := store.latestCache.Timestamp
	wantTier1 := timestamps(store.tier1Buf)
	wantTier2 := timestamps(store.tier2Buf)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = NewStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	for i, tier := range store.tiers {
		if !tier.seekFromSidecar {
			t.Errorf("tier %d rebuilt its index after a clean shutdown", i)
		}
	}
	if !store.latestCache.Timestamp.Equal(wantLatest) {
		t.Errorf("latest sample after restart = %s, want %s", store.latestCache.Timestamp, wantLatest)
	}
	if got := timestamps(store.tier1Buf); !reflect.DeepEqual(got, wantTier1) {
		t.Errorf("pending tier-1 samples after restart = %d, want %d", len(got), len(wantTier1))
	}
	if got := timestamps(store.tier2Buf); !reflect.DeepEqual(got, wantTier2) {
		t.Errorf("pending tier-2 rollups after restart = %d, want %d", len(got), len(wantTier2))
	}
}

// FuzzSeekSidecar feeds arbitrary bytes to the sidecar parser. A corrupt or
// hostile tier_N.idx must never panic the agent, and anything it accepts must
// describe points inside the ring, in ring order.
func FuzzSeekSidecar(f *testing.F) {
	dir := f.TempDir()
	path := filepath.Join(dir, "tier_0.dat")
	tier, err := OpenTier(path, 256<<10)
	if err != nil {
		f.Fatal(err)
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 900; i++ {
		if err := tier.Write(varSample(base.Add(time.Duration(i)*time.Second), 1+i%13)); err != nil {
			f.Fatal(err)
		}
	}
	if err := tier.Close(); err != nil {
		f.Fatal(err)
	}
	valid, err := os.ReadFile(seekSidecarPath(path))
	if err != nil {
		f.Fatal(err)
	}
	f.Add(valid)
	f.Add(valid[:len(valid)-1])
	f.Add([]byte(seekSidecarMagic))
	f.Add([]byte{})

	tier, err = OpenTier(path, 256<<10)
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { _ = tier.Close() })
	f.Fuzz(func(t *testing.T, data []byte) {
		points, _, err := tier.parseSeekSidecar(data)
		if err != nil {
			return
		}
		for i, p := range points {
			if !tier.seekPointConfirmed(p) && (p.offset < 0 || p.offset >= tier.maxData) {
				t.Fatalf("accepted point %d outside the ring: %+v", i, p)
			}
			if i > 0 && (p.cycle < points[i-1].cycle || (p.cycle == points[i-1].cycle && p.offset <= points[i-1].offset)) {
				t.Fatalf("accepted points out of ring order at %d", i)
			}
		}
	})
}

// TestReadOnlySeekIndexMatchesFullScans covers the terminal UI's index: a
// read-only view of a tier another writer keeps appending to builds its index
// on first use and extends it after each refresh, through wraps and through
// laps of the whole ring between two refreshes, and its seeking scans always
// return what a full scan of the owner's file does.
func TestReadOnlySeekIndexMatchesFullScans(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tier_0.dat")
	owner, err := OpenTier(path, 2<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	reader := &Tier{path: path, readOnly: true}
	defer func() { _ = reader.Close() }()

	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	written := []time.Time{}
	write := func(n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			clock = clock.Add(time.Second)
			if err := owner.Write(varSample(clock, 1+(len(written)*7)%23)); err != nil {
				t.Fatal(err)
			}
			written = append(written, clock)
		}
	}
	// Batches below, around and above one ring pass (about 4,500 records).
	for round, batch := range []int{300, 40, 2500, 4700, 9600, 1, 4400, 6100, 700} {
		write(batch)
		if err := reader.reloadReadOnly(); err != nil {
			t.Fatalf("round %d reload: %v", round, err)
		}
		newest := written[len(written)-1]
		oldest := owner.OldestTimestamp()
		froms := []time.Time{oldest.Add(-time.Hour), oldest, newest.Add(-10 * time.Second), newest}
		for i := 0; i < len(written); i += len(written)/16 + 1 {
			if !written[i].Before(oldest) {
				froms = append(froms, written[i], written[i].Add(300*time.Millisecond))
			}
		}
		for _, from := range froms {
			to := newest
			got, err := reader.ReadRange(from, to)
			if err != nil {
				t.Fatalf("round %d reader ReadRange(%s): %v", round, from, err)
			}
			want := unindexed(owner, func() []*AggregatedSample {
				samples, err := owner.ReadRange(from, to)
				if err != nil {
					t.Fatal(err)
				}
				return samples
			})
			if !reflect.DeepEqual(timestamps(got), timestamps(want)) {
				t.Fatalf("round %d: read-only seeking scan from %s returned %d records, full scan %d",
					round, from.Format(time.RFC3339), len(got), len(want))
			}
		}
		reader.mu.RLock()
		ready, points := reader.seekReady, len(reader.seek)
		segments := reader.ringSegments()
		start := reader.seekStart(segments[len(segments)-1], newest.Add(-10*time.Second))
		current := segments[len(segments)-1]
		reader.mu.RUnlock()
		if !ready || points == 0 {
			t.Fatalf("round %d: read-only tier has no seek index (ready %v, %d points)", round, ready, points)
		}
		if current.size > 2*seekIndexSpacing && start == current.start {
			t.Fatalf("round %d: a recent read-only scan starts at the segment start", round)
		}
	}
	if owner.writeCycle < 4 {
		t.Fatalf("precondition: the owner wrapped only %d times", owner.writeCycle)
	}
}

// TestReadOnlySeekIndexSurvivesAnUnseenLap covers the case a refresh cannot
// see: between two refreshes the owner laps the ring and stops past where it
// was, so the header shows no wrap. Extending the old index from where it
// ended would miss the new pass's first records, and with the clock stepped
// back among them, under-estimate what precedes later points and skip
// records. The index must notice and rebuild.
func TestReadOnlySeekIndexSurvivesAnUnseenLap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tier_0.dat")
	owner, err := OpenTier(path, 2<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	reader := &Tier{path: path, readOnly: true}
	defer func() { _ = reader.Close() }()

	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	n := 0
	write := func() {
		t.Helper()
		clock = clock.Add(time.Second)
		if err := owner.Write(varSample(clock, 1+(n*7)%23)); err != nil {
			t.Fatal(err)
		}
		n++
	}
	for i := 0; i < 600; i++ {
		write()
	}
	if err := reader.reloadReadOnly(); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ReadRange(clock.Add(-time.Minute), clock); err != nil {
		t.Fatal(err)
	}
	reader.mu.RLock()
	walked, readerCycle := reader.seekWalked, reader.writeCycle
	reader.mu.RUnlock()
	if walked == 0 || walked+8*seekIndexSpacing >= owner.maxData {
		t.Fatalf("precondition: index ends at %d of %d", walked, owner.maxData)
	}

	// Finish the pass, wrap, and step the clock back 15 minutes halfway to
	// where the reader's index ended; then write on past it. (The ring holds
	// about 25 minutes, and the step must leave its newest record newer than
	// its oldest, or the header's tail is rightly distrusted.)
	for cycle := owner.writeCycle; owner.writeCycle == cycle; {
		write()
	}
	for owner.writeOff < walked/2 {
		write()
	}
	peak := clock
	clock = clock.Add(-15 * time.Minute)
	for owner.writeOff <= walked+4*seekIndexSpacing {
		write()
	}
	if err := reader.reloadReadOnly(); err != nil {
		t.Fatal(err)
	}
	if reader.writeCycle != readerCycle {
		t.Fatal("precondition: the refresh saw the wrap; the lap is not unseen")
	}

	// The window must reach past the pre-step peak: a scan stops at the
	// first record newer than its end.
	to := peak.Add(time.Hour)
	for _, from := range []time.Time{peak.Add(-time.Minute), peak.Add(-5 * time.Minute), clock.Add(-time.Minute)} {
		got, err := reader.ReadRange(from, to)
		if err != nil {
			t.Fatal(err)
		}
		want := unindexed(owner, func() []*AggregatedSample {
			samples, err := owner.ReadRange(from, to)
			if err != nil {
				t.Fatal(err)
			}
			return samples
		})
		if len(want) == 0 {
			t.Fatalf("precondition: a full scan from %s found nothing", from.Format(time.RFC3339))
		}
		if !reflect.DeepEqual(timestamps(got), timestamps(want)) {
			t.Fatalf("after an unseen lap: scan from %s returned %d records, full scan %d",
				from.Format(time.RFC3339), len(got), len(want))
		}
	}
}

// bruteForceRange lists the records of tier with from ≤ ts ≤ to in ring
// order, from a scan that never stops early (its window holds every record).
func bruteForceRange(t *testing.T, tier *Tier, from, to time.Time) []int64 {
	t.Helper()
	all, err := tier.ReadRange(time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	var want []int64
	for _, ts := range timestamps(all) {
		if ts >= from.UnixNano() && ts <= to.UnixNano() {
			want = append(want, ts)
		}
	}
	return want
}

// assertScansFindEveryRecord checks windows against bruteForceRange on the
// owning tier and on a read-only view of its file.
func assertScansFindEveryRecord(t *testing.T, owner *Tier, windows [][2]time.Time) {
	t.Helper()
	reader := &Tier{path: owner.path, readOnly: true}
	defer func() { _ = reader.Close() }()
	if err := reader.reloadReadOnly(); err != nil {
		t.Fatal(err)
	}
	for _, w := range windows {
		want := bruteForceRange(t, owner, w[0], w[1])
		if len(want) == 0 {
			t.Fatalf("precondition: no record in [%s, %s]", w[0].Format(time.RFC3339), w[1].Format(time.RFC3339))
		}
		for name, tier := range map[string]*Tier{"owner": owner, "read-only": reader} {
			got, err := tier.ReadRange(w[0], w[1])
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(timestamps(got), want) {
				t.Errorf("%s scan of [%s, %s] returned %d records, the tier holds %d",
					name, w[0].Format(time.RFC3339), w[1].Format(time.RFC3339), len(got), len(want))
			}
		}
	}
}

// TestScansFindRecordsWrittenAfterAClockStep: after the clock stepped back,
// a window that ends before the pre-step peak holds records on both sides of
// the first record past its end. A scan used to stop there and miss those
// written after the step.
func TestScansFindRecordsWrittenAfterAClockStep(t *testing.T) {
	tier, written := indexTestTier(t, filepath.Join(t.TempDir(), "tier_0.dat"))
	defer func() { _ = tier.Close() }()
	peak := written[8499] // the clock steps back ten minutes after it
	assertScansFindEveryRecord(t, tier, [][2]time.Time{
		{peak.Add(-8 * time.Minute), peak.Add(-6 * time.Minute)},
		{peak.Add(-90 * time.Second), peak.Add(-30 * time.Second)},
		{peak.Add(-10 * time.Minute), peak.Add(-1 * time.Second)},
	})
	if bruteForce := bruteForceRange(t, tier, peak.Add(-8*time.Minute), peak.Add(-6*time.Minute)); len(bruteForce) < 200 {
		t.Fatalf("precondition: the window holds %d records, want both timelines (about 240)", len(bruteForce))
	}
}

// TestScansReadThePreviousPassAcrossAClockStep: the clock stepped back just
// before the ring wrapped, so the current pass begins earlier than the
// previous one ends. A window between the two used to skip the previous
// pass, judged by the current pass's first record alone.
func TestScansReadThePreviousPassAcrossAClockStep(t *testing.T) {
	tier, err := OpenTier(filepath.Join(t.TempDir(), "tier_0.dat"), 2<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tier.Close() }()
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	n := 0
	write := func() {
		t.Helper()
		clock = clock.Add(time.Second)
		if err := tier.Write(varSample(clock, 1+n%13)); err != nil {
			t.Fatal(err)
		}
		n++
	}
	// Fill most of the first pass, step the clock back half an hour, and
	// finish the pass well inside that half hour.
	for tier.writeOff <= tier.maxData*9/10 {
		write()
	}
	if tier.wrapped {
		t.Fatal("precondition: wrapped before the clock step")
	}
	peak := clock
	clock = clock.Add(-30 * time.Minute)
	for !tier.wrapped {
		write()
	}
	for range 50 {
		write()
	}
	tier.mu.RLock()
	first, err := tier.readTimestampAt(0)
	tier.mu.RUnlock()
	if err != nil || !first.Before(peak) {
		t.Fatalf("precondition: the current pass begins at %s, not before the previous pass's peak %s", first, peak)
	}
	assertScansFindEveryRecord(t, tier, [][2]time.Time{
		{first.Add(time.Second), peak},
		{first.Add(time.Second), clock},
	})
}
