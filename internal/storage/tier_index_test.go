package storage

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"
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
