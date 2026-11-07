package storage

import (
	"bufio"
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
// Write maintains the index. OpenTier adopts the sidecar file the last clean
// Close saved next to the tier (tier_N.idx) when it describes exactly the tier
// on disk; otherwise it rebuilds the index with one walk of the ring. The tier
// file format is unchanged: the sidecar is only a cache, and old versions
// ignore it. Read-only tiers keep no index and scan from the segment start.
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
//	[0:8]   magic "KULAIDX1"
//	fingerprint of the tier as Close left it:
//	        maxData u64, writeOff u64, count u64, oldestOff u64, codecVer u64,
//	        newestTS i64, fileSize u64, wrapped u8
//	        passMax i64
//	        count u32, then per point: offset u64, ts i64, maxBefore i64,
//	        pass u8 (0 = current pass, 1 = previous pass)
//	        crc32 (IEEE) of every preceding byte
//
// The sidecar is adopted only when the fingerprint equals the tier just opened,
// so a crash (the header has moved on), an external rewrite, a restore from
// backup or a resize all rebuild the index instead.

const (
	seekSidecarMagic = "KULAIDX1"
	// magic, fingerprint, passMax, point count and the trailing checksum.
	seekSidecarFixed  = 8 + 7*8 + 1 + 8 + 4 + 4
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
	buf = binary.LittleEndian.AppendUint64(buf, uint64(t.passMax))
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
	points, passMax, err := t.parseSeekSidecar(data)
	if err != nil {
		return err
	}
	t.seek, t.passMax, t.seekReady = points, passMax, true
	return nil
}

// parseSeekSidecar validates a sidecar against the open tier and returns its
// points with their passes mapped onto the in-memory ring passes.
func (t *Tier) parseSeekSidecar(data []byte) ([]seekPoint, int64, error) {
	if len(data) < seekSidecarFixed || string(data[:8]) != seekSidecarMagic {
		return nil, 0, fmt.Errorf("not a seek index sidecar")
	}
	body, sum := data[:len(data)-4], binary.LittleEndian.Uint32(data[len(data)-4:])
	if crc32.ChecksumIEEE(body) != sum {
		return nil, 0, fmt.Errorf("seek index sidecar checksum mismatch")
	}
	fingerprint, err := t.seekFingerprint()
	if err != nil {
		return nil, 0, err
	}
	off := 8
	if string(body[off:off+len(fingerprint)]) != string(fingerprint) {
		return nil, 0, errSeekSidecarStale
	}
	off += len(fingerprint)
	passMax := int64(binary.LittleEndian.Uint64(body[off:]))
	count := int(binary.LittleEndian.Uint32(body[off+8:]))
	off += 12
	if count < 0 || (len(body)-off)%seekSidecarPoint != 0 || (len(body)-off)/seekSidecarPoint != count {
		return nil, 0, fmt.Errorf("seek index sidecar length does not match its %d points", count)
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
			return nil, 0, fmt.Errorf("seek index sidecar point %d is outside its pass", i)
		}
		// Ring order: the previous pass first, offsets and prefix maxima
		// ascending within a pass.
		if n := len(points); n > 0 {
			prev := points[n-1]
			if p.cycle < prev.cycle || (p.cycle == prev.cycle && (p.offset <= prev.offset || p.maxBefore < prev.maxBefore)) {
				return nil, 0, fmt.Errorf("seek index sidecar point %d is out of order", i)
			}
		}
		points = append(points, p)
	}
	return points, passMax, nil
}

// openSeekIndex adopts the sidecar or rebuilds the index. Caller holds t.mu
// exclusively, or the tier is not shared yet.
func (t *Tier) openSeekIndex() {
	if err := t.loadSeekIndex(); err == nil {
		t.seekFromSidecar = true
		return
	}
	t.seekFromSidecar = false
	t.buildSeekIndex()
}
