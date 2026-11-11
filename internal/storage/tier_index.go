package storage

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ---- Seek index ------------------------------------------------------------
//
// Records are length-prefixed and chronological within a ring pass, but carry
// no back links, so without help a scan reads every record from the oldest one
// on — the whole tier file — just to reach a query's `from`, and finding the
// newest records means walking the ring to its end.
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
// A scan stops at the first record past its window's end, which is only right
// while no later record of the pass was written with an earlier timestamp. So
// the index also keeps, per pass, the newest such "backstep" record and the
// pass's greatest timestamp: a scan stops early only at or past the backstep,
// and skips the previous pass only when its greatest timestamp precedes the
// window. Without an index, a scan reads every record of its segments.
//
// Write maintains the index. OpenTier adopts the sidecar file the last clean
// Close saved next to the tier (tier_N.idx) when it describes exactly the tier
// on disk; otherwise it rebuilds the index with one walk of the ring. The tier
// file format is unchanged: the sidecar is only a cache, and old versions
// ignore it.
//
// A read-only tier (the terminal UI reading what `kula serve` writes) builds
// its index with one walk on its first scan and, before each later scan,
// extends it over what the owner appended since. A refresh that shows the
// owner wrapped trims it as Write does; a replaced, resized or rewritten file
// drops it. The owner writes in ring order, so it overwrites the newest point
// before anything indexed after it: while that point still holds its record,
// the index is current, and when it does not the index is rebuilt. Points are
// confirmed against the file as always, and a scan the owner overtakes is
// caught by readOnlyScan.
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
	br := scanReader(io.NewSectionReader(t.file, headerSize+start, seg.size-bytesRead))
	defer releaseScanReader(br)
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
	t.passMax, t.prevPassMax = math.MinInt64, math.MinInt64
	t.backstep, t.prevBackstep = -1, -1
	t.seekWalked = 0
	for _, seg := range t.ringSegments() {
		passMax := int64(math.MinInt64)
		backstep := int64(-1)
		last := int64(-1)
		t.walkSegment(seg, seg.start, func(offset int64, dataLen uint32, ts int64, ok bool) bool {
			if seg.cycle == t.writeCycle {
				t.seekWalked = offset + 4 + int64(dataLen)
			}
			if !ok {
				// Unknown from here on: every later record may step back.
				passMax = math.MaxInt64
				backstep = offset
				return true
			}
			if last < 0 || offset-last >= seekIndexSpacing {
				t.seek = append(t.seek, seekPoint{offset: offset, ts: ts, maxBefore: passMax, cycle: seg.cycle})
				last = offset
			}
			if ts < passMax {
				backstep = offset
			}
			passMax = max(passMax, ts)
			return true
		})
		if seg.cycle == t.writeCycle {
			t.passMax, t.backstep = passMax, backstep
		} else {
			t.prevPassMax, t.prevBackstep = passMax, backstep
		}
	}
	t.seekReady = true
}

// mayStopAt reports whether a scan of seg that met a record past its window
// at offset can stop there: no later record of its pass was written with an
// earlier timestamp. Without an index it cannot know. Caller holds t.mu.
func (t *Tier) mayStopAt(seg ringSegment, offset int64) bool {
	if !t.seekReady {
		return false
	}
	backstep := t.backstep
	if seg.cycle != t.writeCycle {
		backstep = t.prevBackstep
	}
	return backstep <= offset
}

// dropSeekIndex forgets the index; the next read-only scan rebuilds it.
// Caller holds t.mu exclusively.
func (t *Tier) dropSeekIndex() {
	t.seek = nil
	t.seekReady = false
	t.passMax, t.prevPassMax = math.MinInt64, math.MinInt64
	t.backstep, t.prevBackstep = -1, -1
	t.seekWalked = 0
}

// prepareReadOnlySeek brings a read-only tier's index up to the snapshot the
// last refresh adopted: built with one walk on first use, then extended over
// the records the owner appended to the current pass since. Caller holds no
// lock on t.
func (t *Tier) prepareReadOnlySeek() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.file == nil || t.count == 0 || t.codecVer < codecVersion2 {
		return
	}
	if t.seekReady {
		newest := -1
		for i := len(t.seek) - 1; i >= 0 && newest < 0; i-- {
			if t.seek[i].cycle == t.writeCycle {
				newest = i
			}
		}
		switch {
		case t.seekWalked > t.writeOff:
			t.seekReady = false
		case newest >= 0 && !t.seekPointConfirmed(t.seek[newest]):
			// The owner lapped the ring past it unseen between refreshes.
			t.seekReady = false
		case newest < 0 && t.seekWalked > 0:
			t.seekReady = false
		}
	}
	if !t.seekReady {
		t.buildSeekIndex()
		return
	}
	segments := t.ringSegments()
	current := segments[len(segments)-1]
	last := int64(-1)
	if n := len(t.seek); n > 0 && t.seek[n-1].cycle == t.writeCycle {
		last = t.seek[n-1].offset
	}
	t.walkSegment(current, t.seekWalked, func(offset int64, dataLen uint32, ts int64, ok bool) bool {
		t.seekWalked = offset + 4 + int64(dataLen)
		if !ok {
			t.passMax = math.MaxInt64
			t.backstep = offset
			return true
		}
		if last < 0 || offset-last >= seekIndexSpacing {
			t.seek = append(t.seek, seekPoint{offset: offset, ts: ts, maxBefore: t.passMax, cycle: t.writeCycle})
			last = offset
		}
		if ts < t.passMax {
			t.backstep = offset
		}
		t.passMax = max(t.passMax, ts)
		return true
	})
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
	if ts < t.passMax {
		t.backstep = offset
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

type recordLoc struct {
	offset int64 // data-region offset of the length prefix
	length uint32
}

// latestRecords returns the locations of the n newest records, oldest first.
// It walks forward from ever older seek points until n records follow one,
// and from the start of the ring when none does. Caller holds t.mu.
func (t *Tier) latestRecords(n int) []recordLoc {
	segments := t.ringSegments()
	if t.seekReady {
		for back := 0; back < len(t.seek); back = 2*back + 1 {
			p := t.seek[len(t.seek)-1-back]
			first := -1
			for i, seg := range segments {
				if seg.cycle == p.cycle && p.offset >= seg.start && p.offset < seg.start+seg.size {
					first = i
				}
			}
			if first < 0 || !t.seekPointConfirmed(p) {
				break
			}
			if locs, walked := t.collectLatest(segments, first, p.offset, n); walked >= n {
				return locs
			}
		}
	}
	if len(segments) == 0 {
		return nil
	}
	locs, _ := t.collectLatest(segments, 0, segments[0].start, n)
	return locs
}

// collectLatest walks segments[first] from start and every later segment, and
// returns the last n records seen and how many it saw.
func (t *Tier) collectLatest(segments []ringSegment, first int, start int64, n int) ([]recordLoc, int) {
	locs := make([]recordLoc, 0, n)
	walked := 0
	for i := first; i < len(segments); i++ {
		from := segments[i].start
		if i == first {
			from = start
		}
		t.walkSegment(segments[i], from, func(offset int64, dataLen uint32, _ int64, _ bool) bool {
			walked++
			loc := recordLoc{offset: offset, length: dataLen}
			if len(locs) < n {
				locs = append(locs, loc)
			} else {
				copy(locs, locs[1:])
				locs[n-1] = loc
			}
			return true
		})
	}
	return locs, walked
}

// ---- Sidecar ----------------------------------------------------------------
//
// Layout, little-endian:
//
//	[0:8]   magic "KULAIDX2"
//	fingerprint of the tier as Close left it:
//	        maxData u64, writeOff u64, count u64, oldestOff u64, codecVer u64,
//	        newestTS i64, fileSize u64, wrapped u8
//	        passMax i64, prevPassMax i64, backstep i64, prevBackstep i64
//	        count u32, then per point: offset u64, ts i64, maxBefore i64,
//	        pass u8 (0 = current pass, 1 = previous pass)
//	        crc32 (IEEE) of every preceding byte
//
// The sidecar is adopted only when the fingerprint equals the tier just opened,
// so a crash (the header has moved on), an external rewrite, a restore from
// backup or a resize all rebuild the index instead. A KULAIDX1 sidecar (0.22.0
// development builds, without the pass maxima and backsteps) is rebuilt too.

const (
	seekSidecarMagic = "KULAIDX2"
	// magic, fingerprint, pass maxima and backsteps, point count and the
	// trailing checksum.
	seekSidecarFixed  = 8 + 7*8 + 1 + 4*8 + 4 + 4
	seekSidecarPoint  = 3*8 + 1
	seekSidecarMaxLen = 64 << 20
)

// seekSidecarPath names the sidecar of a tier file: tier_0.dat → tier_0.idx.
func seekSidecarPath(tierPath string) string {
	return strings.TrimSuffix(tierPath, filepath.Ext(tierPath)) + ".idx"
}

func (t *Tier) seekFingerprint() ([]byte, error) {
	info, err := t.file.Stat()
	if err != nil {
		return nil, err
	}
	b := make([]byte, 0, 8*7+1)
	for _, v := range []uint64{uint64(t.maxData), uint64(t.writeOff), t.count, uint64(t.oldestOff), t.codecVer} {
		b = binary.LittleEndian.AppendUint64(b, v)
	}
	var newest int64
	if !t.newestTS.IsZero() {
		newest = t.newestTS.UnixNano()
	}
	b = binary.LittleEndian.AppendUint64(b, uint64(newest))
	b = binary.LittleEndian.AppendUint64(b, uint64(info.Size()))
	if t.wrapped {
		return append(b, 1), nil
	}
	return append(b, 0), nil
}

// saveSeekIndex writes the sidecar atomically. Caller holds t.mu exclusively,
// after the header has been written and synced.
func (t *Tier) saveSeekIndex() error {
	fingerprint, err := t.seekFingerprint()
	if err != nil {
		return err
	}
	buf := make([]byte, 0, seekSidecarFixed+len(t.seek)*seekSidecarPoint)
	buf = append(buf, seekSidecarMagic...)
	buf = append(buf, fingerprint...)
	for _, v := range []int64{t.passMax, t.prevPassMax, t.backstep, t.prevBackstep} {
		buf = binary.LittleEndian.AppendUint64(buf, uint64(v))
	}
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(t.seek)))
	for _, p := range t.seek {
		buf = binary.LittleEndian.AppendUint64(buf, uint64(p.offset))
		buf = binary.LittleEndian.AppendUint64(buf, uint64(p.ts))
		buf = binary.LittleEndian.AppendUint64(buf, uint64(p.maxBefore))
		if p.cycle == t.writeCycle {
			buf = append(buf, 0)
		} else {
			buf = append(buf, 1)
		}
	}
	buf = binary.LittleEndian.AppendUint32(buf, crc32.ChecksumIEEE(buf))

	path := seekSidecarPath(t.path)
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(buf)
	if syncErr := tmp.Sync(); err == nil {
		err = syncErr
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
	}
	return err
}

var errSeekSidecarStale = errors.New("seek index sidecar does not describe this tier")

// loadSeekIndex adopts the sidecar when it describes exactly the tier on disk.
// Caller holds t.mu exclusively.
func (t *Tier) loadSeekIndex() error {
	path := seekSidecarPath(t.path)
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Size() > seekSidecarMaxLen {
		return fmt.Errorf("seek index sidecar is %d bytes", info.Size())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	points, passes, err := t.parseSeekSidecar(data)
	if err != nil {
		return err
	}
	t.seek, t.seekReady = points, true
	t.passMax, t.prevPassMax, t.backstep, t.prevBackstep = passes.max, passes.prevMax, passes.backstep, passes.prevBackstep
	return nil
}

// sidecarPasses is what a sidecar records about the ring's two passes.
type sidecarPasses struct {
	max, prevMax, backstep, prevBackstep int64
}

// parseSeekSidecar validates a sidecar against the open tier and returns its
// points with their passes mapped onto the in-memory ring passes.
func (t *Tier) parseSeekSidecar(data []byte) ([]seekPoint, sidecarPasses, error) {
	var passes sidecarPasses
	if len(data) < seekSidecarFixed || string(data[:8]) != seekSidecarMagic {
		return nil, passes, fmt.Errorf("not a seek index sidecar")
	}
	body, sum := data[:len(data)-4], binary.LittleEndian.Uint32(data[len(data)-4:])
	if crc32.ChecksumIEEE(body) != sum {
		return nil, passes, fmt.Errorf("seek index sidecar checksum mismatch")
	}
	fingerprint, err := t.seekFingerprint()
	if err != nil {
		return nil, passes, err
	}
	off := 8
	if string(body[off:off+len(fingerprint)]) != string(fingerprint) {
		return nil, passes, errSeekSidecarStale
	}
	off += len(fingerprint)
	for _, v := range []*int64{&passes.max, &passes.prevMax, &passes.backstep, &passes.prevBackstep} {
		*v = int64(binary.LittleEndian.Uint64(body[off:]))
		off += 8
	}
	passMax := passes.max
	count := int(binary.LittleEndian.Uint32(body[off:]))
	off += 4
	if passes.backstep < -1 || passes.backstep >= max(t.writeOff, 1) ||
		passes.prevBackstep < -1 || passes.prevBackstep >= t.maxData ||
		(!t.wrapped && (passes.prevBackstep != -1 || passes.prevMax != math.MinInt64)) {
		return nil, passes, fmt.Errorf("seek index sidecar backsteps lie outside their passes")
	}
	if count < 0 || (len(body)-off)%seekSidecarPoint != 0 || (len(body)-off)/seekSidecarPoint != count {
		return nil, passes, fmt.Errorf("seek index sidecar length does not match its %d points", count)
	}

	points := make([]seekPoint, 0, count)
	for i := 0; i < count; i++ {
		p := seekPoint{
			offset:    int64(binary.LittleEndian.Uint64(body[off:])),
			ts:        int64(binary.LittleEndian.Uint64(body[off+8:])),
			maxBefore: int64(binary.LittleEndian.Uint64(body[off+16:])),
		}
		pass := body[off+24]
		off += seekSidecarPoint
		switch {
		case pass == 0 && p.offset >= 0 && p.offset < t.writeOff && p.ts <= passMax && p.maxBefore <= passMax:
			p.cycle = t.writeCycle
		case pass == 1 && t.wrapped && p.offset >= t.oldestOff && p.offset < t.maxData:
			p.cycle = t.writeCycle - 1
		default:
			return nil, passes, fmt.Errorf("seek index sidecar point %d is outside its pass", i)
		}
		// Ring order: the previous pass first, offsets and prefix maxima
		// ascending within a pass.
		if n := len(points); n > 0 {
			prev := points[n-1]
			if p.cycle < prev.cycle || (p.cycle == prev.cycle && (p.offset <= prev.offset || p.maxBefore < prev.maxBefore)) {
				return nil, passes, fmt.Errorf("seek index sidecar point %d is out of order", i)
			}
		}
		points = append(points, p)
	}
	return points, passes, nil
}

// removeStaleSeekTemps deletes temporary sidecars a crash left between
// saveSeekIndex's create and rename. Only the tier's owner calls it.
func (t *Tier) removeStaleSeekTemps() {
	matches, _ := filepath.Glob(globEscape(seekSidecarPath(t.path)) + ".tmp-*")
	for _, path := range matches {
		_ = os.Remove(path)
	}
}

// globEscape quotes the metacharacters of a literal path for filepath.Glob.
func globEscape(path string) string {
	var b strings.Builder
	for _, r := range path {
		switch r {
		case '*', '?', '[', ']', '\\':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// openSeekIndex adopts the sidecar or rebuilds the index. Caller holds t.mu
// exclusively, or the tier is not shared yet.
func (t *Tier) openSeekIndex() {
	if err := t.loadSeekIndex(); err == nil {
		t.seekFromSidecar = true
		return
	}
	t.seekFromSidecar = false
	// Without a sidecar from a clean shutdown the header may lag the data.
	t.verifyOldSegment()
	t.buildSeekIndex()
}
