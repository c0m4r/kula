package storage

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// Tier implements a single ring-buffer storage tier.
// File format:
//   Header (64 bytes):
//     [0:4]   magic "KULA"
//     [4:8]   reserved
//     [8:16]  version (uint64)
//     [16:24] max data size (uint64)
//     [24:32] write offset within data region (uint64)
//     [32:40] total records written (uint64)
//     [40:48] oldest timestamp (int64, unix nano)
//     [48:56] newest timestamp (int64, unix nano)
//     [56:64] reserved
//   Data region:
//     Sequence of: [length uint32][data []byte]
//     When write wraps around, it overwrites from the beginning.

const (
	headerSize    = 64
	magicString   = "KULA"
	version       = 1
	codecVersion2 = 2
)

// Header flags packed into bytes [4:8] (uint32 LE). Files written before this
// field existed leave it zeroed, which is exactly how we detect a pre-fix file
// that has no recorded tail offset.
const (
	flagHeaderHasTail uint32 = 1 << 0 // header carries a valid oldestOff (tail)
	flagHeaderWrapped uint32 = 1 << 1 // ring has wrapped; an old segment exists
)

type Tier struct {
	mu       sync.RWMutex
	file     *os.File
	path     string
	maxData  int64
	writeOff int64
	count    uint64
	oldestTS time.Time
	newestTS time.Time
	wrapped  bool
	// oldestOff is the data-region byte offset of the oldest surviving record.
	// It is meaningful only while wrapped and is always kept on a real record
	// boundary, so it stays correct even when record sizes vary across a wrap.
	oldestOff int64
	codecVer  uint64 // 1 = legacy JSON, 2 = binary
}

func OpenTier(path string, maxSize int64) (*Tier, error) {
	maxData := maxSize - headerSize
	if maxData < 1024 {
		return nil, fmt.Errorf("max_size too small for tier: %d", maxSize)
	}

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, fmt.Errorf("opening tier file: %w", err)
	}

	t := &Tier{
		file:    f,
		path:    path,
		maxData: maxData,
	}

	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}

	if info.Size() >= headerSize {
		if err := t.readHeader(); err != nil {
			// A header that won't parse means the file is corrupt or not a Kula
			// tier. Refuse to open it. The previous behavior reinitialized in
			// place — it overwrote the header with a zeroed one, silently
			// abandoning (and then overwriting) any still-intact data. Failing
			// loudly instead lets the operator inspect or move the file aside.
			_ = f.Close()
			return nil, fmt.Errorf("tier %s: corrupt or unreadable header: %w; "+
				"refusing to open so existing data is not destroyed — move the file aside to start fresh", path, err)
		}
	} else {
		t.writeOff = 0
		t.count = 0
		t.codecVer = codecVersion2 // new files start binary
		if err := t.writeHeader(); err != nil {
			_ = f.Close()
			return nil, err
		}
	}

	if t.codecVer < codecVersion2 && t.count > 0 {
		fmt.Printf("Storage migration: legacy JSON detected in %s, starting conversion to binary v2...\n", path)
		if err := t.migrateToBinary(); err != nil {
			_ = t.file.Close()
			return nil, fmt.Errorf("migration failed for %s: %w", path, err)
		}
		fmt.Printf("Storage migration: %s converted to binary v2 successfully.\n", path)
	}

	return t, nil
}

func (t *Tier) readHeader() error {
	buf := make([]byte, headerSize)
	if _, err := t.file.ReadAt(buf, 0); err != nil {
		return err
	}

	magic := string(buf[0:4])
	if magic != magicString {
		return fmt.Errorf("invalid magic: %q", magic)
	}

	v := binary.LittleEndian.Uint64(buf[8:16])
	if v == 0 {
		v = 1
	}
	t.codecVer = v
	t.maxData = int64(binary.LittleEndian.Uint64(buf[16:24]))
	if t.maxData == 0 {
		return fmt.Errorf("invalid header: maxData is zero")
	}
	t.writeOff = int64(binary.LittleEndian.Uint64(buf[24:32]))
	t.count = binary.LittleEndian.Uint64(buf[32:40])

	oldestNano := int64(binary.LittleEndian.Uint64(buf[40:48]))
	newestNano := int64(binary.LittleEndian.Uint64(buf[48:56]))
	if oldestNano > 0 {
		t.oldestTS = time.Unix(0, oldestNano)
	}
	if newestNano > 0 {
		t.newestTS = time.Unix(0, newestNano)
	}

	headerFlags := binary.LittleEndian.Uint32(buf[4:8])
	tailTrusted := false
	if headerFlags&flagHeaderHasTail != 0 {
		// Written by tail-tracking code.
		t.wrapped = headerFlags&flagHeaderWrapped != 0
		t.oldestOff = int64(binary.LittleEndian.Uint64(buf[56:64]))
		switch {
		case !t.wrapped:
			tailTrusted = true // legitimate non-wrapped state; nothing to validate
		case t.oldestOff >= t.writeOff && t.oldestOff < t.maxData:
			// Validate the persisted tail: an offset left behind by an earlier
			// buggy build can point at garbage (e.g. a far-future "2119"
			// timestamp). Only trust a tail that decodes to a sane oldest.
			if ts, err := t.readTimestampAt(t.oldestOff); err == nil &&
				!ts.IsZero() && (t.newestTS.IsZero() || !ts.After(t.newestTS)) {
				t.oldestTS = ts
				tailTrusted = true
			}
		}
	}

	if !tailTrusted {
		// Reached for (a) pre-fix files with no tail metadata and (b)
		// new-format files whose persisted wrapped tail failed validation
		// (corrupt offset from an earlier buggy build). In BOTH cases reproduce
		// the PREVIOUS binary's layout instead of dropping data: if the file is
		// physically larger than [0, writeOff) it had wrapped, so keep the whole
		// ring by assuming the oldest record begins at writeOff — exactly what
		// the old code read (ReadRange becomes byte-for-byte identical). Tail
		// tracking then refines oldestOff on the next writes.
		//
		// Dropping the [writeOff, maxData) segment here would wipe up to a whole
		// buffer of history on upgrade — a stable node has uniform records and a
		// perfectly valid old segment. Self-heal must never self-annihilate.
		t.wrapped = false
		t.oldestOff = 0
		if t.count > 0 {
			fileInfo, _ := t.file.Stat()
			if fileInfo != nil && fileInfo.Size() > headerSize+t.writeOff {
				t.wrapped = true
				t.oldestOff = t.writeOff
				// Refresh oldestTS from the assumed tail when it decodes sanely;
				// otherwise keep the header value (display-only; it self-corrects
				// as the ring cycles). This never affects what data is kept.
				if ts, err := t.readTimestampAt(t.writeOff); err == nil &&
					!ts.IsZero() && (t.newestTS.IsZero() || !ts.After(t.newestTS)) {
					t.oldestTS = ts
				}
			}
		}
	}

	// Non-wrapped: the oldest record sits at offset 0. Re-derive oldestTS from
	// there so it can never be the classic 1970/2119 garbage a buggy header may
	// carry.
	if !t.wrapped {
		t.oldestOff = 0
		if t.count > 0 && t.writeOff > 0 {
			if ts, err := t.readTimestampAt(0); err == nil {
				t.oldestTS = ts
			}
		}
	}

	return nil
}

func (t *Tier) writeHeader() error {
	buf := make([]byte, headerSize)
	copy(buf[0:4], magicString)
	binary.LittleEndian.PutUint64(buf[8:16], t.codecVer)
	binary.LittleEndian.PutUint64(buf[16:24], uint64(t.maxData))
	binary.LittleEndian.PutUint64(buf[24:32], uint64(t.writeOff))
	binary.LittleEndian.PutUint64(buf[32:40], t.count)

	headerFlags := flagHeaderHasTail
	if t.wrapped {
		headerFlags |= flagHeaderWrapped
	}
	binary.LittleEndian.PutUint32(buf[4:8], headerFlags)
	binary.LittleEndian.PutUint64(buf[56:64], uint64(t.oldestOff))

	if !t.oldestTS.IsZero() {
		binary.LittleEndian.PutUint64(buf[40:48], uint64(t.oldestTS.UnixNano()))
	}
	if !t.newestTS.IsZero() {
		binary.LittleEndian.PutUint64(buf[48:56], uint64(t.newestTS.UnixNano()))
	}

	_, err := t.file.WriteAt(buf, 0)
	return err
}

// Write stores a sample in the ring buffer.
func (t *Tier) Write(s *AggregatedSample) error {
	// Defensive: a nil sample would panic in the encoder. Callers never pass
	// nil today, but turn a latent panic in the collection loop into a plain
	// error so a single bad aggregate can't take the whole process down.
	if s == nil {
		return fmt.Errorf("tier %s: refusing to write nil sample", t.path)
	}
	// encodeSampleV returns [kind][preamble][fixed][variable...] — the full
	// on-disk payload including the recordKindBinary byte at [0].
	data, err := encodeSampleV(s)
	if err != nil {
		return err
	}

	recordLen := 4 + len(data) // length prefix + data
	if int64(recordLen) > t.maxData {
		return fmt.Errorf("sample too large: %d > %d", recordLen, t.maxData)
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	// Check if we need to wrap
	if t.writeOff+int64(recordLen) > t.maxData {
		// Write a zero sentinel to mark end-of-segment so ReadRange
		// knows there are no more records in this tail region.
		if t.writeOff+4 <= t.maxData {
			var sentinel [4]byte // zero-value sentinel (dataLen == 0)
			_, _ = t.file.WriteAt(sentinel[:], headerSize+t.writeOff)
		}
		t.writeOff = 0
		if !t.wrapped {
			// First wrap: every record from the just-finished pass becomes the
			// "old" segment and the oldest one sits at offset 0.
			t.wrapped = true
			t.oldestOff = 0
		}
	}

	// While wrapped, the record about to be written at [writeOff,
	// writeOff+recordLen) may overlap one or more surviving old records.
	// Advance the tail (oldestOff) past every old record we are about to
	// clobber, reading each old length BEFORE it is overwritten. This keeps
	// oldestOff on a real record boundary no matter how record sizes vary
	// across the wrap — the root cause of the bogus "oldest timestamp" and the
	// dropped history was assuming that boundary was always at writeOff.
	if t.wrapped {
		newHead := t.writeOff + int64(recordLen)
		for t.oldestOff < newHead {
			oldLen, ok := t.recordLenAt(t.oldestOff)
			if !ok {
				// Sentinel or end of the old region: the tail has caught the
				// head, so the ring is contiguous [0, newHead) again.
				t.wrapped = false
				t.oldestOff = 0
				break
			}
			t.oldestOff += 4 + oldLen
			if t.oldestOff >= t.maxData {
				t.wrapped = false
				t.oldestOff = 0
				break
			}
		}
		// The advance can land exactly on the sentinel that marks the end of
		// the old region (the last old record was just consumed). That isn't a
		// real record, so the old segment is now empty: the ring is contiguous
		// [0, newHead) and the oldest record is at offset 0.
		if t.wrapped {
			if _, ok := t.recordLenAt(t.oldestOff); !ok {
				t.wrapped = false
				t.oldestOff = 0
			}
		}
	}

	var lenBuf [4]byte
	binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(data)))

	fileOff := headerSize + t.writeOff
	if _, err := t.file.WriteAt(lenBuf[:], fileOff); err != nil {
		return err
	}
	if _, err := t.file.WriteAt(data, fileOff+4); err != nil {
		return err
	}

	t.writeOff += int64(recordLen)
	t.count++
	t.newestTS = s.Timestamp

	// Keep oldestTS pinned to the actual oldest surviving record.
	if t.wrapped {
		// oldestOff points at a valid record boundary (maintained above).
		if ts, err := t.readTimestampAt(t.oldestOff); err == nil {
			t.oldestTS = ts
		}
	} else if ts, err := t.readTimestampAt(0); err == nil {
		// Not wrapped: records were only appended, so the oldest is at offset 0.
		t.oldestTS = ts
	} else if t.oldestTS.IsZero() {
		t.oldestTS = s.Timestamp
	}

	// Bump codec version to binary on first write to a legacy JSON file.
	if t.codecVer < codecVersion2 {
		t.codecVer = codecVersion2
		_ = t.writeHeader()
	}

	return t.writeHeader()
}

// ReadRange returns all samples within [from, to].
func (t *Tier) ReadRange(from, to time.Time) ([]*AggregatedSample, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if t.count == 0 {
		return nil, nil
	}

	var samples []*AggregatedSample

	// Build list of (start, size) segments to scan in chronological order.
	//
	// When wrapped two segments exist:
	//   1. writeOff..maxData  — older records from the previous ring pass
	//   2. 0..writeOff        — newer records written after the wrap
	// When not wrapped: one segment 0..writeOff.
	//
	// For v2 binary files and wrapped tiers, we check whether 'from' is past
	// the start of segment 2; if so we skip segment 1 entirely (safe because
	// each segment always starts on a record boundary).
	type segment struct{ start, size int64 }
	var segments []segment

	if t.wrapped {
		seg1 := segment{t.oldestOff, t.maxData - t.oldestOff}
		seg2 := segment{0, t.writeOff}

		if t.codecVer >= codecVersion2 && t.writeOff > 0 {
			// Peek at the oldest record in segment 2 (data region offset 0).
			// If 'from' is at or after that timestamp we can skip segment 1 entirely.
			seg2Oldest, err := t.readTimestampAt(0)
			if err == nil && !from.Before(seg2Oldest) {
				segments = []segment{seg2}
			} else {
				segments = []segment{seg1, seg2}
			}
		} else {
			segments = []segment{seg1, seg2}
		}
	} else {
		segments = []segment{{0, t.writeOff}}
	}

	for _, seg := range segments {
		bytesRead := int64(0)

		// Use buffered reader for drastic performance improvement over thousands of reads.
		sr := io.NewSectionReader(t.file, headerSize+seg.start, seg.size)
		br := bufio.NewReaderSize(sr, 1024*1024)

		for bytesRead < seg.size {
			if seg.size-bytesRead < 4 {
				break
			}

			lenBuf := make([]byte, 4)
			if _, err := io.ReadFull(br, lenBuf); err != nil {
				break
			}
			dataLen := binary.LittleEndian.Uint32(lenBuf)

			if dataLen == 0 || int64(dataLen) > t.maxData {
				break
			}

			recordLen := int64(4 + dataLen)
			if bytesRead+recordLen > seg.size {
				break
			}

			data := make([]byte, dataLen)
			if _, err := io.ReadFull(br, data); err != nil {
				break
			}

			ts, err := extractTimestamp(data)
			if err != nil {
				// Fallback: full decode for JSON records or unreadable binary.
				sample, decErr := t.readRecord(data)
				if decErr == nil {
					if sample.Timestamp.After(to) {
						break
					}
					if !sample.Timestamp.Before(from) {
						samples = append(samples, sample)
					}
				}
				bytesRead += recordLen
				continue
			}

			// Records are chronological within a segment: past the window → done.
			if ts.After(to) {
				break
			}

			if ts.Before(from) {
				bytesRead += recordLen
				continue
			}

			sample, err := t.readRecord(data)
			if err != nil {
				bytesRead += recordLen
				continue
			}

			samples = append(samples, sample)
			bytesRead += recordLen
		}
	}

	return samples, nil
}

// ReadLatest returns the n most recent samples.
func (t *Tier) ReadLatest(n int) ([]*AggregatedSample, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if t.count == 0 {
		return nil, nil
	}

	type segment struct{ start, size int64 }
	var segments []segment

	if t.wrapped {
		segments = append(segments, segment{t.oldestOff, t.maxData - t.oldestOff})
		segments = append(segments, segment{0, t.writeOff})
	} else {
		segments = append(segments, segment{0, t.writeOff})
	}

	// First pass: find the offsets of all records
	type recordLoc struct {
		offset int64
		length uint32
	}
	locs := make([]recordLoc, 0, n)

	for _, seg := range segments {
		bytesRead := int64(0)
		sr := io.NewSectionReader(t.file, headerSize+seg.start, seg.size)
		br := bufio.NewReaderSize(sr, 1024*1024)

		for bytesRead < seg.size {
			if seg.size-bytesRead < 4 {
				break
			}
			lenBuf := make([]byte, 4)
			if _, err := io.ReadFull(br, lenBuf); err != nil {
				break
			}
			dataLen := binary.LittleEndian.Uint32(lenBuf)
			if dataLen == 0 || int64(dataLen) > t.maxData {
				break
			}

			recordLen := int64(4 + dataLen)
			if bytesRead+recordLen > seg.size {
				break
			}

			loc := recordLoc{
				offset: headerSize + seg.start + bytesRead,
				length: dataLen,
			}
			if len(locs) < n {
				locs = append(locs, loc)
			} else {
				copy(locs, locs[1:])
				locs[len(locs)-1] = loc
			}

			if _, err := br.Discard(int(dataLen)); err != nil {
				break
			}
			bytesRead += recordLen
		}
	}

	var samples []*AggregatedSample
	for _, loc := range locs {
		data := make([]byte, loc.length)
		if _, err := t.file.ReadAt(data, loc.offset+4); err != nil {
			continue
		}
		sample, err := t.readRecord(data)
		if err == nil {
			samples = append(samples, sample)
		}
	}

	return samples, nil
}

func (t *Tier) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.writeHeader(); err != nil {
		return err
	}
	// Flush header + data to stable storage so a clean shutdown or upgrade is
	// durable. We deliberately do NOT fsync on every Write (too costly for the
	// per-sample hot path); syncing here covers the common restart/upgrade
	// case. A crash mid-operation can still lose the last few un-synced records.
	if err := t.file.Sync(); err != nil {
		_ = t.file.Close()
		return fmt.Errorf("syncing tier %s on close: %w", t.path, err)
	}
	return t.file.Close()
}

func (t *Tier) Flush() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.writeHeader()
}

// SnapshotTo writes a byte-for-byte copy of the tier file to w. The copy is
// taken under the tier read lock, so it never races a Write (which updates the
// data region and header atomically under the write lock). The result is a
// self-contained, consistent tier file that can be reopened with OpenTier.
func (t *Tier) SnapshotTo(w io.Writer) (int64, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	info, err := t.file.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat tier %s: %w", t.path, err)
	}
	return io.Copy(w, io.NewSectionReader(t.file, 0, info.Size()))
}

// readRecord decodes a payload using per-record format detection.
//
//   - recordKindBinary (0x02): kind-tagged binary written by current code
//   - '{' (0x7B):              legacy JSON record
//   - anything else:           legacy binary record (no kind byte)
func (t *Tier) readRecord(data []byte) (*AggregatedSample, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty record")
	}
	switch data[0] {
	case recordKindBinary:
		return decodeSample(data[1:])
	case '{':
		return decodeSampleJSON(data)
	default:
		// Legacy binary record written before the kind-byte format.
		return decodeSample(data)
	}
}

// recordLenAt reads just the 4-byte length prefix of the record at the given
// data-region offset. It returns the payload length (excluding the prefix) and
// whether it looks like a real record. A zero or out-of-range length means a
// sentinel or the end of the old region. Must be called under at least a read
// lock (Write holds the write lock, which is sufficient).
func (t *Tier) recordLenAt(dataOffset int64) (int64, bool) {
	var buf [4]byte
	if _, err := t.file.ReadAt(buf[:], headerSize+dataOffset); err != nil {
		return 0, false
	}
	dataLen := binary.LittleEndian.Uint32(buf[:])
	if dataLen == 0 || int64(dataLen) > t.maxData {
		return 0, false
	}
	return int64(dataLen), true
}

// readTimestampAt reads the timestamp of the record at the given data-region
// offset. Returns an error if the record is invalid. Must be called under at
// least a read lock (Write holds the write lock, which is sufficient).
//
// Format detection uses the explicit kind byte for new binary records and falls
// back to content sniffing for legacy formats (JSON: '{'; binary: no kind byte).
func (t *Tier) readTimestampAt(dataOffset int64) (time.Time, error) {
	// 4-byte length prefix + 1 kind byte + 8-byte timestamp = 13 bytes covers all
	// three formats in a single ReadAt call.
	var buf [13]byte
	if _, err := t.file.ReadAt(buf[:], headerSize+dataOffset); err != nil {
		return time.Time{}, err
	}
	dataLen := binary.LittleEndian.Uint32(buf[0:4])
	if dataLen == 0 || int64(dataLen) > t.maxData {
		return time.Time{}, fmt.Errorf("invalid record length %d at offset %d", dataLen, dataOffset)
	}

	switch buf[4] {
	case recordKindBinary:
		// Kind-tagged binary record: timestamp follows the kind byte.
		if dataLen < 9 {
			return time.Time{}, fmt.Errorf("binary record too short for timestamp: %d bytes", dataLen)
		}
		return time.Unix(0, int64(binary.LittleEndian.Uint64(buf[5:13]))), nil
	case '{':
		// Legacy JSON record: must fully decode to get timestamp.
		data := make([]byte, dataLen)
		if _, err := t.file.ReadAt(data, headerSize+dataOffset+4); err != nil {
			return time.Time{}, err
		}
		s, err := decodeSampleJSON(data)
		if err != nil {
			return time.Time{}, err
		}
		return s.Timestamp, nil
	default:
		// Legacy binary record (no kind byte): timestamp at start of payload.
		if dataLen < 8 {
			return time.Time{}, fmt.Errorf("legacy binary record too short: %d bytes", dataLen)
		}
		return time.Unix(0, int64(binary.LittleEndian.Uint64(buf[4:12]))), nil
	}
}

// migrateToBinary converts a legacy (v1) tier file to binary (v2) in-place.
// It reads all records in chronological order and rewrites them to a new v2 file.
func (t *Tier) migrateToBinary() error {
	// 1. Pre-check: Disk space.
	// We need enough space for a full copy of the tier file.
	var stat syscall.Statfs_t
	if err := syscall.Statfs(filepath.Dir(t.path), &stat); err != nil {
		return fmt.Errorf("statfs failed: %w", err)
	}
	available := int64(stat.Bavail) * int64(stat.Bsize)
	required := headerSize + t.maxData
	if available < required {
		return fmt.Errorf("insufficient disk space for migration: need %d MB, have %d MB", required/1e6, available/1e6)
	}

	tmpPath := t.path + ".migration"
	// Ensure cleanup if we fail
	defer func() { _ = os.Remove(tmpPath) }()

	// Open original for reading (already open in t.file) and tmp for writing.
	tmpFile, err := os.OpenFile(tmpPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = tmpFile.Close() }()

	// Initialize tmp tier structure to use its Write logic.
	tmpTier := &Tier{
		file:     tmpFile,
		path:     tmpPath,
		maxData:  t.maxData,
		codecVer: codecVersion2,
	}
	if err := tmpTier.writeHeader(); err != nil {
		return err
	}

	// Read all records from the current tier in chronological order.
	type segment struct{ start, size int64 }
	var segments []segment
	if t.wrapped {
		segments = []segment{
			{t.oldestOff, t.maxData - t.oldestOff},
			{0, t.writeOff},
		}
	} else {
		segments = []segment{{0, t.writeOff}}
	}

	processed := 0
	for _, seg := range segments {
		bytesRead := int64(0)
		sr := io.NewSectionReader(t.file, headerSize+seg.start, seg.size)
		br := bufio.NewReaderSize(sr, 1024*1024)

		for bytesRead < seg.size {
			if seg.size-bytesRead < 4 {
				break
			}
			lenBuf := make([]byte, 4)
			if _, err := io.ReadFull(br, lenBuf); err != nil {
				break
			}
			dataLen := binary.LittleEndian.Uint32(lenBuf)
			if dataLen == 0 || int64(dataLen) > t.maxData {
				break
			}
			recordLen := int64(4 + dataLen)
			if bytesRead+recordLen > seg.size {
				break
			}

			data := make([]byte, dataLen)
			if _, err := io.ReadFull(br, data); err != nil {
				break
			}

			// Decode record (handles JSON/binary automatically)
			sample, err := t.readRecord(data)
			if err != nil {
				// Skip corrupted records during migration to keep as much as possible.
				bytesRead += recordLen
				continue
			}

			// Write to new binary format
			if err := tmpTier.Write(sample); err != nil {
				return fmt.Errorf("writing migrated record: %w", err)
			}
			processed++
			if processed%1000 == 0 {
				fmt.Printf("  ...migrated %d records in %s\n", processed, filepath.Base(t.path))
			}
			bytesRead += recordLen
		}
	}

	fmt.Printf("  Migration complete: %d records processed for %s\n", processed, filepath.Base(t.path))

	// Finalize tmp file
	if err := tmpTier.writeHeader(); err != nil {
		return err
	}
	if err := tmpFile.Sync(); err != nil {
		return err
	}
	_ = tmpFile.Close()

	// Atomic replacement:
	// 1. Rename tmp to original (this replaces the file on disk)
	if err := os.Rename(tmpPath, t.path); err != nil {
		return fmt.Errorf("renaming migrated file: %w", err)
	}

	// 2. Open the NEW file FIRST before closing the old one to avoid nil state
	newFile, err := os.OpenFile(t.path, os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("opening migrated file: %w", err)
	}

	// 3. Swap file handles and close old
	oldFile := t.file
	t.file = newFile
	if oldFile != nil {
		_ = oldFile.Close()
	}

	// 4. RESET STALE STATE before reading the new header
	// This prevents the "wrapped" state leak bug.
	t.wrapped = false
	t.oldestOff = 0
	t.count = 0
	t.writeOff = 0
	t.oldestTS = time.Time{}
	t.newestTS = time.Time{}

	// 5. Load new metadata
	if err := t.readHeader(); err != nil {
		return fmt.Errorf("reading migrated header: %w", err)
	}

	// 6. Verification: Ensure we can read the newest record
	if t.count > 0 {
		if _, err := t.ReadLatest(1); err != nil {
			return fmt.Errorf("migration verification failed for %s: %w", t.path, err)
		}
	}

	return nil
}

// OldestTimestamp returns the oldest sample timestamp in this tier.
func (t *Tier) OldestTimestamp() time.Time {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.oldestTS
}

// NewestTimestamp returns the newest sample timestamp in this tier.
func (t *Tier) NewestTimestamp() time.Time {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.newestTS
}

// Count returns the total number of records written to this tier.
func (t *Tier) Count() uint64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.count
}

// TierInfo holds metadata about a tier file, extracted without locking or loading the full file.
type TierInfo struct {
	Version  uint64
	MaxData  int64
	WriteOff int64
	Count    uint64
	OldestTS time.Time
	NewestTS time.Time
	Wrapped  bool
}

// InspectTierFile reads only the header of a tier file and returns metadata.
func InspectTierFile(path string) (*TierInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	buf := make([]byte, headerSize)
	if _, err := io.ReadFull(f, buf); err != nil {
		return nil, fmt.Errorf("reading header: %w", err)
	}

	magic := string(buf[0:4])
	if magic != magicString {
		return nil, fmt.Errorf("invalid magic: %q", magic)
	}

	info := &TierInfo{
		Version:  binary.LittleEndian.Uint64(buf[8:16]),
		MaxData:  int64(binary.LittleEndian.Uint64(buf[16:24])),
		WriteOff: int64(binary.LittleEndian.Uint64(buf[24:32])),
		Count:    binary.LittleEndian.Uint64(buf[32:40]),
	}

	oldestNano := int64(binary.LittleEndian.Uint64(buf[40:48]))
	newestNano := int64(binary.LittleEndian.Uint64(buf[48:56]))
	if oldestNano > 0 {
		info.OldestTS = time.Unix(0, oldestNano)
	}
	if newestNano > 0 {
		info.NewestTS = time.Unix(0, newestNano)
	}

	headerFlags := binary.LittleEndian.Uint32(buf[4:8])
	if headerFlags&flagHeaderHasTail != 0 {
		// Trust the persisted wrap state written by tail-tracking code.
		info.Wrapped = headerFlags&flagHeaderWrapped != 0
	} else if info.Count > 0 {
		// Pre-fix file: fall back to the legacy file-size heuristic. Once the
		// server reopens and rewrites the header, the flag path above takes over.
		fileInfo, _ := f.Stat()
		if fileInfo != nil && fileInfo.Size() > headerSize+info.WriteOff {
			info.Wrapped = true
		}
	}

	return info, nil
}
