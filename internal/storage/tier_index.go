package storage

import (
	"bufio"
	"encoding/binary"
	"io"
	"math"
	"sort"
	"time"
)

// ---- Seek index ------------------------------------------------------------
//
// Records are length-prefixed and chronological within a ring pass, but carry
// no back links, so without help a scan reads every record from the oldest one
// on — the whole tier file — just to reach a query's `from`.
//
// The seek index keeps, about every seekIndexSpacing bytes of a pass, a record
// boundary together with that record's timestamp and the greatest timestamp
// written before it in the same pass. A scan for records from `from` on starts
// at the last point whose predecessors all precede `from`: it skips exactly the
// records a scan from the segment start would have skipped, so results are
// identical even if the clock once stepped backwards. Every point is checked
// against the file (a valid length prefix and its exact timestamp) before it is
// used; a scan that cannot confirm a point starts at the segment start.
//
// OpenTier builds the index with one walk of the ring and Write maintains it.
// It lives in memory only; the tier file format is unchanged. Read-only tiers
// keep no index and scan from the segment start.
// ----------------------------------------------------------------------------

// seekIndexSpacing is the byte distance between seek points in a pass, and so
// about the most a seeking scan reads before it reaches `from`.
const seekIndexSpacing = 64 << 10

type seekPoint struct {
	offset    int64 // data-region offset of the record's length prefix
	ts        int64 // the record's timestamp (Unix ns), checked before use
	maxBefore int64 // greatest timestamp of the records before it in its pass
	cycle     int64 // ring pass (Tier.writeCycle) the record was written in
}

// ringSegment is one chronological run of records in the data region.
type ringSegment struct {
	start, size int64
	cycle       int64
	// resync is true for the active head segment ([0, writeOff)). There a
	// zero/garbage record is a hole left by an unclean shutdown, so a walk
	// skips it and continues. It is false for the old wrapped segment, where a
	// zero length is the legitimate end-of-segment sentinel.
	resync bool
}

// ringSegments returns the ring's segments, oldest first: the previous pass
// [oldestOff, maxData) while wrapped, then the current pass [0, writeOff). The
// header's geometry is not trusted past the end of the file. Caller holds t.mu.
func (t *Tier) ringSegments() []ringSegment {
	var segments []ringSegment
	if t.wrapped {
		segments = append(segments, ringSegment{start: t.oldestOff, size: t.maxData - t.oldestOff, cycle: t.writeCycle - 1})
	}
	segments = append(segments, ringSegment{start: 0, size: t.writeOff, cycle: t.writeCycle, resync: true})
	if info, err := t.file.Stat(); err == nil {
		dataSize := info.Size() - headerSize
		for i := range segments {
			segments[i].size = max(0, min(segments[i].size, dataSize-segments[i].start))
		}
	}
	return segments
}

// walkSegment visits the records of seg from start, a record boundary in seg,
// handling holes and the sentinel as scanRange does. visit receives each
// record's data-region offset and payload length, and its timestamp when the
// payload has a binary timestamp header (ok false otherwise, e.g. legacy JSON);
// it returns false to stop. Caller holds t.mu.
func (t *Tier) walkSegment(seg ringSegment, start int64, visit func(offset int64, dataLen uint32, ts int64, ok bool) bool) {
	bytesRead := start - seg.start
	if bytesRead < 0 || bytesRead > seg.size {
		return
	}
	sr := io.NewSectionReader(t.file, headerSize+start, seg.size-bytesRead)
	br := bufio.NewReaderSize(sr, 1024*1024)
	for bytesRead < seg.size {
		if seg.size-bytesRead < 4 {
			return
		}
		hdr, _ := br.Peek(int(min(13, seg.size-bytesRead)))
		if len(hdr) < 4 {
			return
		}
		dataLen := binary.LittleEndian.Uint32(hdr[0:4])
		if !t.validRecordLen(dataLen) || bytesRead+4+int64(dataLen) > seg.size {
			if !seg.resync || !t.resyncToNextRecord(br, &bytesRead, seg.size) {
				return
			}
			continue
		}
		var ts int64
		ok := false
		if len(hdr) >= 13 && dataLen >= 9 && hdr[4] != '{' {
			if stamp, err := extractTimestamp(hdr[4:]); err == nil {
				ts, ok = stamp.UnixNano(), true
			}
		}
		if !visit(seg.start+bytesRead, dataLen, ts, ok) {
			return
		}
		recordLen := int64(4 + dataLen)
		if _, err := br.Discard(int(recordLen)); err != nil {
			return
		}
		bytesRead += recordLen
	}
}

// buildSeekIndex indexes the ring with one walk. A record without a readable
// timestamp makes every later point of its pass unusable rather than wrong.
// Caller holds t.mu exclusively.
func (t *Tier) buildSeekIndex() {
	t.seek = nil
	t.passMax = math.MinInt64
	for _, seg := range t.ringSegments() {
		passMax := int64(math.MinInt64)
		last := int64(-1)
		t.walkSegment(seg, seg.start, func(offset int64, _ uint32, ts int64, ok bool) bool {
			if !ok {
				passMax = math.MaxInt64
				return true
			}
			if last < 0 || offset-last >= seekIndexSpacing {
				t.seek = append(t.seek, seekPoint{offset: offset, ts: ts, maxBefore: passMax, cycle: seg.cycle})
				last = offset
			}
			passMax = max(passMax, ts)
			return true
		})
		if seg.cycle == t.writeCycle {
			t.passMax = passMax
		}
	}
	t.seekReady = true
}

// seekTrim drops the points whose records a wrap or the write about to happen
// overwrites: earlier passes, and the previous pass before oldestOff. Call it
// once oldestOff has advanced. Caller holds t.mu exclusively.
func (t *Tier) seekTrim() {
	if !t.seekReady {
		return
	}
	drop := 0
	for ; drop < len(t.seek); drop++ {
		p := t.seek[drop]
		if p.cycle == t.writeCycle || (p.cycle == t.writeCycle-1 && t.wrapped && p.offset >= t.oldestOff) {
			break
		}
	}
	if drop > 0 {
		t.seek = append(t.seek[:0], t.seek[drop:]...)
	}
}

// seekAppend notes the record just written at offset: a point when it starts
// a pass or lies seekIndexSpacing past the newest one. Caller holds t.mu
// exclusively.
func (t *Tier) seekAppend(offset, ts int64) {
	if !t.seekReady {
		return
	}
	if n := len(t.seek); n == 0 || t.seek[n-1].cycle != t.writeCycle || offset-t.seek[n-1].offset >= seekIndexSpacing {
		t.seek = append(t.seek, seekPoint{offset: offset, ts: ts, maxBefore: t.passMax, cycle: t.writeCycle})
	}
	t.passMax = max(t.passMax, ts)
}

// seekPointConfirmed reports whether p still lies in its segment and the file
// holds a record with p's exact timestamp at its offset. Caller holds t.mu.
func (t *Tier) seekPointConfirmed(p seekPoint) bool {
	switch {
	case p.cycle == t.writeCycle:
		if p.offset < 0 || p.offset >= t.writeOff {
			return false
		}
	case p.cycle == t.writeCycle-1 && t.wrapped:
		if p.offset < t.oldestOff || p.offset >= t.maxData {
			return false
		}
	default:
		return false
	}
	ts, err := t.readTimestampAt(p.offset)
	return err == nil && ts.UnixNano() == p.ts
}

// seekStart returns where a scan of seg for records from `from` on can begin:
// the newest confirmed point of seg whose predecessors in its pass all precede
// from, or seg.start. Caller holds t.mu.
func (t *Tier) seekStart(seg ringSegment, from time.Time) int64 {
	// UnixNano is defined for years 1678–2262 only.
	if !t.seekReady || from.Year() < 1700 || from.Year() > 2200 {
		return seg.start
	}
	fromNs := from.UnixNano()
	lo := sort.Search(len(t.seek), func(i int) bool { return t.seek[i].cycle >= seg.cycle })
	hi := lo + sort.Search(len(t.seek)-lo, func(i int) bool { return t.seek[lo+i].cycle > seg.cycle })
	// maxBefore never decreases within a pass.
	k := lo + sort.Search(hi-lo, func(i int) bool { return t.seek[lo+i].maxBefore >= fromNs })
	if k == lo {
		return seg.start
	}
	p := t.seek[k-1]
	if p.offset <= seg.start || p.offset >= seg.start+seg.size || !t.seekPointConfirmed(p) {
		return seg.start
	}
	return p.offset
}

// openSeekIndex builds the index with one walk of the ring. Caller holds t.mu
// exclusively, or the tier is not shared yet.
func (t *Tier) openSeekIndex() {
	t.buildSeekIndex()
}
