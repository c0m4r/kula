package storage

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
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
//     [4:8]   header flags (uint32 LE): has tail, wrapped — see below
//     [8:16]  version (uint64)
//     [16:24] max data size (uint64)
//     [24:32] write offset within data region (uint64)
//     [32:40] total records written (uint64)
//     [40:48] oldest timestamp (int64, unix nano)
//     [48:56] newest timestamp (int64, unix nano)
//     [56:64] oldest record offset within data region (uint64), the ring's
//             tail; valid only with the has-tail flag
//   Data region:
//     Sequence of: [length uint32][data []byte]
//     data is [0x03][encoded sample][crc32] for records written since 0.22.0
//     (record_checksum.go); older files also hold 0x02-tagged, legacy JSON
//     and legacy binary records, which carry no checksum.
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
	// In-memory ring pass and attempted write extent let batched readers
	// detect overwritten snapshot bytes while releasing the lock to reduce.
	writeCycle int64
	writeEnd   int64
	// readOnly tiers are views of a file another process writes (see
	// OpenReadOnly); they never write, and Close only releases the file.
	readOnly bool

	// Sparse seek index (tier_index.go), ring order: the previous pass, then
	// the current one. Only trusted while seekReady.
	seek      []seekPoint
	seekReady bool
	// passMax is the greatest timestamp written in pass writeCycle, and
	// prevPassMax that of the pass before it.
	passMax, prevPassMax int64
	// backstep and prevBackstep are, for the current and the previous pass,
	// the offset of the newest record written with a timestamp below an
	// earlier one of its pass (the clock stepped back), or -1. A scan may
	// stop at the first record past its window only at or beyond it.
	backstep, prevBackstep int64
	// seekFromSidecar records that OpenTier adopted the saved index instead
	// of walking the ring.
	seekFromSidecar bool
	// seekWalked is where the index of the current pass ends: the end of the
	// last record indexed. A read-only tier extends its index from there.
	seekWalked int64

	// snapshotCopied, set only by tests, runs after SnapshotTo's unlocked
	// copy and before it copies what changed meanwhile.
	snapshotCopied func()
}

// maxTierData bounds a ring's data region. Far beyond any tier a filesystem
// holds, it keeps every file offset into the ring, plus a record, within an
// int64.
const maxTierData = 1 << 62

func OpenTier(path string, maxSize int64) (*Tier, error) {
	maxData := maxSize - headerSize
	if maxData < 1024 {
		return nil, fmt.Errorf("max_size too small for tier: %d", maxSize)
	}
	if maxData > maxTierData {
		return nil, fmt.Errorf("max_size too large for tier: %d", maxSize)
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
		// A write offset past the end of the file counts records that never
		// reached the disk: a crash wrote the header but not them. Readers
		// clip the ring to the file, and writing resumes where its data ends
		// instead of past a hole every scan would resync across, which a
		// crafted offset could make exabytes long.
		if dataEnd := info.Size() - headerSize; t.writeOff > dataEnd {
			t.writeOff = dataEnd
		}
		// readHeader replaced maxData with the geometry the file was actually
		// created with. Reconcile it with what the operator configured, so
		// changing storage.tiers[].max_size is not a silent no-op.
		if err := t.applyConfiguredSize(maxData); err != nil {
			_ = f.Close()
			return nil, err
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

	t.removeStaleSeekTemps()
	t.openSeekIndex()
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
	// The ring size and write offset place every read and write in the file,
	// so a corrupt or crafted header must not get past here with values the
	// ring arithmetic cannot hold: a write offset near 2^63 once overflowed
	// Write's wrap check, which then wrote at that offset, and the next scan
	// walked the 9.2 EB sparse file that made, holding the tier lock.
	t.maxData = int64(binary.LittleEndian.Uint64(buf[16:24]))
	if t.maxData <= 0 || t.maxData > maxTierData {
		return fmt.Errorf("invalid header: ring size %d", uint64(t.maxData))
	}
	t.writeOff = int64(binary.LittleEndian.Uint64(buf[24:32]))
	if t.writeOff < 0 || t.writeOff > t.maxData {
		return fmt.Errorf("invalid header: write offset %d lies outside the %d-byte ring", uint64(t.writeOff), t.maxData)
	}
	t.count = binary.LittleEndian.Uint64(buf[32:40])

	oldestNano := int64(binary.LittleEndian.Uint64(buf[40:48]))
	newestNano := int64(binary.LittleEndian.Uint64(buf[48:56]))
	if oldestNano > 0 {
		t.oldestTS = time.Unix(0, oldestNano)
	}
	if newestNano > 0 {
		t.newestTS = time.Unix(0, newestNano)
	}

	// Locate the previous pass, the old segment [oldestOff, its end) of a
	// wrapped ring. Files written by tail-tracking code carry oldestOff;
	// older ones kept the whole ring, the oldest record assumed at writeOff.
	// Either way the record found there must prove itself (see oldRecordFrom),
	// not merely be older than newestTS: after the clock steps back the whole
	// previous pass is later than the newest record, and it was dropped. An
	// offset that fails (a corrupt header from an earlier build, or one a
	// crash left behind the data) is resynced forward to the next real record
	// instead of being assumed at writeOff, which is no record boundary since
	// the ring tracks its tail.
	headerFlags := binary.LittleEndian.Uint32(buf[4:8])
	candidate := t.writeOff
	t.wrapped = false
	t.oldestOff = 0
	if headerFlags&flagHeaderHasTail != 0 {
		if headerFlags&flagHeaderWrapped == 0 {
			candidate = -1 // a legitimate non-wrapped ring
		} else if persisted := int64(binary.LittleEndian.Uint64(buf[56:64])); persisted >= t.writeOff && persisted < t.maxData {
			candidate = persisted
		}
	}
	if candidate >= 0 && t.count > 0 {
		if fileInfo, _ := t.file.Stat(); fileInfo != nil && fileInfo.Size() > headerSize+t.writeOff {
			// Records written before kind bytes (and legacy JSON) cannot
			// prove themselves; keep the offset there, as the builds that
			// wrote them did.
			off := candidate
			if t.codecVer >= codecVersion2 {
				if found, ok := t.oldRecordFrom(candidate, newResyncBudget()); ok {
					off = found
				}
			}
			t.wrapped, t.oldestOff = true, off
			if ts, err := t.readTimestampAt(off); err == nil && !ts.IsZero() {
				t.oldestTS = ts
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

// applyConfiguredSize reconciles the ring geometry persisted in the file header
// with the size the operator configured. The header value is authoritative for
// reading — it is the geometry the existing records were laid out with, and
// reinterpreting them under a different one would corrupt the wrap arithmetic —
// so the two can only be reconciled by moving the end of the ring.
//
// Growing is safe in place, wrapped or not: the bytes the ring gains lie past
// the end of the file, so nothing reads them before the writer fills them. A
// wrapped old segment still terminates at the end-of-segment sentinel written
// at the previous wrap, and where that wrap left too few bytes for a sentinel
// the scan runs into EOF at the same place instead. Either way the new room is
// reached only by the writer, which fills it in record order like any other.
//
// Shrinking is not applied: writeOff and oldestOff would fall outside the new
// ring, stranding every record beyond it. The tier keeps running at its on-disk
// size and says so, loudly enough that the setting does not appear to have
// taken effect when it has not.
func (t *Tier) applyConfiguredSize(configured int64) error {
	switch {
	case configured == t.maxData:
		return nil

	case configured > t.maxData:
		previous := t.maxData
		t.maxData = configured
		if err := t.writeHeader(); err != nil {
			t.maxData = previous
			return fmt.Errorf("tier %s: growing ring from %s to %s: %w",
				t.path, formatTierSize(previous+headerSize), formatTierSize(configured+headerSize), err)
		}
		fmt.Printf("Storage: tier %s grown in place from %s to %s to match max_size; existing history kept.\n",
			filepath.Base(t.path), formatTierSize(previous+headerSize), formatTierSize(configured+headerSize))

	default:
		fmt.Printf("Storage: tier %s was created at max_size %s and keeps running at that size; "+
			"the configured %s is smaller and shrinking in place would strand records already written. "+
			"To apply it, stop Kula and move %s aside — that discards the tier's history.\n",
			filepath.Base(t.path), formatTierSize(t.maxData+headerSize),
			formatTierSize(configured+headerSize), t.path)
	}

	return nil
}

// formatTierSize renders a byte count the way max_size is written in the
// config, so the numbers in these messages can be matched against the YAML.
func formatTierSize(b int64) string {
	const (
		kb = 1024
		mb = 1024 * kb
		gb = 1024 * mb
	)
	switch {
	case b >= gb:
		return fmt.Sprintf("%.1fGB", float64(b)/gb)
	case b >= mb:
		return fmt.Sprintf("%.1fMB", float64(b)/mb)
	case b >= kb:
		return fmt.Sprintf("%.1fKB", float64(b)/kb)
	default:
		return fmt.Sprintf("%dB", b)
	}
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
	if t.readOnly {
		return ErrReadOnly
	}
	// encodeSampleV returns [kind][preamble][fixed][variable...][crc32] — the
	// full on-disk payload, recordKindChecksummed at [0] and its checksum last.
	data, err := encodeSampleV(s)
	if err != nil {
		return err
	}
	return t.writePayload(data, s.Timestamp)
}

// writePayload appends one encoded on-disk payload whose record time is ts.
// Write is its only production caller; tests use it to lay down records in
// the formats older versions wrote, through the same bookkeeping.
func (t *Tier) writePayload(data []byte, ts time.Time) error {
	recordLen := 4 + len(data) // length prefix + data
	if int64(recordLen) > t.maxData || len(data) > maxRecordBytes {
		return fmt.Errorf("sample too large: %d bytes (tier holds %d, a record at most %d)", recordLen, t.maxData, maxRecordBytes)
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	// Check if we need to wrap. readHeader keeps writeOff within the ring, so
	// the room left cannot overflow, as writeOff+recordLen could.
	if room := t.maxData - t.writeOff; int64(recordLen) > room {
		// Write a zero sentinel to mark end-of-segment so ReadRange
		// knows there are no more records in this tail region.
		if room >= 4 {
			var sentinel [4]byte // zero-value sentinel (dataLen == 0)
			_, _ = t.file.WriteAt(sentinel[:], headerSize+t.writeOff)
		}
		t.writeOff = 0
		// Every record of the just-finished pass becomes the "old" segment,
		// and the oldest one sits at offset 0. That holds even when the ring
		// was still wrapped: a pass that ends with larger records than the one
		// before it wraps while a few of the older pass's records still lie
		// past the writer. Those are older than anything else and are dropped
		// here (the sentinel above now ends the old segment before them).
		// Keeping oldestOff on them would instead make every scan read them,
		// stop at their sentinel and skip the whole pass just finished.
		t.wrapped = true
		t.oldestOff = 0
		t.writeCycle++
		t.writeEnd = 0
		t.prevPassMax, t.prevBackstep = t.passMax, t.backstep
		t.passMax, t.backstep = math.MinInt64, -1
	}
	t.writeEnd = t.writeOff + int64(recordLen)

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
	t.seekTrim()

	var lenBuf [4]byte
	binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(data)))

	fileOff := headerSize + t.writeOff
	if _, err := t.file.WriteAt(lenBuf[:], fileOff); err != nil {
		return err
	}
	if _, err := t.file.WriteAt(data, fileOff+4); err != nil {
		return err
	}
	t.seekAppend(t.writeOff, ts.UnixNano())

	t.writeOff += int64(recordLen)
	t.count++
	t.newestTS = ts

	// Keep oldestTS pinned to the actual oldest surviving record.
	if t.wrapped {
		// oldestOff points at a valid record boundary (maintained above).
		if oldest, err := t.readTimestampAt(t.oldestOff); err == nil {
			t.oldestTS = oldest
		}
	} else if oldest, err := t.readTimestampAt(0); err == nil {
		// Not wrapped: records were only appended, so the oldest is at offset 0.
		t.oldestTS = oldest
	} else if t.oldestTS.IsZero() {
		t.oldestTS = ts
	}

	// Bump codec version to binary on first write to a legacy JSON file.
	if t.codecVer < codecVersion2 {
		t.codecVer = codecVersion2
		_ = t.writeHeader()
	}

	return t.writeHeader()
}

// maxRecordBytes bounds one record's payload. Encoded samples are kilobytes,
// and a rollup of a host with 20,000 interfaces, 5,000 filesystems and 10,000
// containers is about 12 MiB; the most interfaces the codec can hold come to
// 18 MiB. The bound keeps a corrupt or torn length prefix from allocating up
// to the whole ring (hundreds of MB) before the read that follows fails.
const maxRecordBytes = 64 << 20

// validRecordLen reports whether a length prefix can describe a record: not
// the zero end-of-segment sentinel, within the ring and within
// maxRecordBytes.
func (t *Tier) validRecordLen(dataLen uint32) bool {
	return dataLen != 0 && dataLen <= maxRecordBytes && int64(dataLen) <= t.maxData
}

// errHistorySnapshotExpired prevents combining old and overwritten ring bytes
// if a writer overtakes a reader between batches.
var errHistorySnapshotExpired = errors.New("history retention changed during query; retry the request")

// scanReaderSize is the read-ahead of a tier scan.
const scanReaderSize = 1 << 20

// scanReaders recycles the read buffers of tier scans. A scan used to
// allocate one for every ring segment, and for a scan of a few records (the
// newest buckets of a view, refreshed over and over) that megabyte was
// nearly everything it allocated.
var scanReaders = sync.Pool{New: func() any { return bufio.NewReaderSize(nil, scanReaderSize) }}

// scanReader returns a pooled reader set to read from r, to be handed back
// through releaseScanReader.
func scanReader(r io.Reader) *bufio.Reader {
	br := scanReaders.Get().(*bufio.Reader)
	br.Reset(r)
	return br
}

func releaseScanReader(br *bufio.Reader) {
	br.Reset(nil) // the pool must not keep a tier file reachable
	scanReaders.Put(br)
}

// ReadRange returns all samples within [from, to]. History queries use the
// batched scanner to avoid retaining every decoded source record at once.
func (t *Tier) ReadRange(from, to time.Time) ([]*AggregatedSample, error) {
	return t.scanRange(from, to, 0, nil)
}

func (t *Tier) scanRange(from, to time.Time, batchSize int, consume func([]*AggregatedSample) error) ([]*AggregatedSample, error) {
	if t.readOnly {
		t.prepareReadOnlySeek()
	}
	t.mu.RLock()
	defer t.mu.RUnlock()

	if t.count == 0 {
		return nil, nil
	}

	// A read-only tier's owner writes it from another process while it is
	// read; the guard checks the scan against it (see readOnlyScan).
	var guard *readOnlyScan
	if t.readOnly {
		guard = t.newReadOnlyScan()
	}

	var samples []*AggregatedSample
	keep := func(sample *AggregatedSample, offset, length int64) {
		samples = append(samples, sample)
		if guard != nil {
			guard.kept(offset, length)
		}
	}
	check := func() error {
		if guard == nil {
			return nil
		}
		var err error
		samples, err = guard.check(samples)
		return err
	}
	flush := func() error {
		if err := check(); err != nil {
			return err
		}
		if consume == nil {
			return nil
		}
		// Always reacquire before the outer deferred unlock, even on panic.
		err := func() error {
			t.mu.RUnlock()
			defer t.mu.RLock()
			if len(samples) > 0 {
				return consume(samples)
			}
			return nil
		}()
		samples = nil
		return err
	}

	// Segments in ring order: while wrapped, the previous pass [oldestOff,
	// maxData) and then the current pass [0, writeOff); otherwise only the
	// current pass. The previous pass is skipped when every record it holds
	// precedes `from`. Its first record cannot tell: after the clock stepped
	// back, the current pass can begin before the previous one ended.
	segments := t.ringSegments()
	if len(segments) == 2 && t.seekReady && time.Unix(0, t.prevPassMax).Before(from) {
		segments = segments[1:]
	}

	br := scanReader(nil)
	defer releaseScanReader(br)

	scanned := 0
	for _, seg := range segments {
		// Start at the seek point nearest before `from` (tier_index.go); the
		// records before it would all have been skipped.
		start := t.seekStart(seg, from)
		bytesRead := start - seg.start
		budget := newResyncBudget()

		// Use buffered reader for drastic performance improvement over thousands of reads.
		br.Reset(io.NewSectionReader(t.file, headerSize+start, seg.size-bytesRead))

		for bytesRead < seg.size {
			if consume != nil && batchSize > 0 && scanned >= batchSize {
				if err := flush(); err != nil {
					return nil, err
				}
				scanned = 0
			}
			scanned++
			if guard != nil {
				if guard.due() {
					if err := check(); err != nil {
						return nil, err
					}
				}
				guard.reached(seg.start + bytesRead)
			}
			// The source extent is fixed at scan start. Appends are harmless;
			// overwrites of unread bytes invalidate this snapshot.
			passes := t.writeCycle - seg.cycle
			if passes > 1 || (passes == 1 && max(t.writeOff, t.writeEnd) > seg.start+bytesRead) {
				return nil, errHistorySnapshotExpired
			}
			if seg.size-bytesRead < 4 {
				break
			}

			// Peek the length prefix (plus enough of the header to sanity-check a
			// record during resync) without consuming it.
			peekN := int64(13)
			if peekN > seg.size-bytesRead {
				peekN = seg.size - bytesRead
			}
			hdr, _ := br.Peek(int(peekN))
			if len(hdr) < 4 {
				break
			}
			dataLen := binary.LittleEndian.Uint32(hdr[0:4])

			if !t.validRecordLen(dataLen) || bytesRead+4+int64(dataLen) > seg.size {
				// In the old wrapped segment a zero length is the end-of-segment
				// sentinel: stop. In the active segment it is a hole from an
				// unclean shutdown — skip past it and keep the data written after
				// it instead of abandoning the rest of the tier.
				if !seg.resync || !t.resyncToNextRecord(br, &bytesRead, seg.size, budget) {
					break
				}
				continue
			}

			recordLen := int64(4 + dataLen)
			// Binary timestamps fit in the peeked header. Skip out-of-range
			// payloads without allocating or decoding their metric graphs.
			if len(hdr) >= 13 && dataLen >= 9 && hdr[4] != '{' {
				if ts, err := extractTimestamp(hdr[4:]); err == nil {
					if ts.After(to) && t.mayStopAt(seg, seg.start+bytesRead) {
						break
					}
					if ts.Before(from) || ts.After(to) {
						if _, err := br.Discard(int(recordLen)); err != nil {
							break
						}
						bytesRead += recordLen
						continue
					}
				}
			}
			if _, err := br.Discard(4); err != nil {
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
					if sample.Timestamp.After(to) && t.mayStopAt(seg, seg.start+bytesRead) {
						break
					}
					if !sample.Timestamp.Before(from) && !sample.Timestamp.After(to) {
						keep(sample, seg.start+bytesRead, recordLen)
					}
				}
				bytesRead += recordLen
				continue
			}

			// Past the window: done, unless a record after this one was
			// written with an earlier timestamp.
			if ts.After(to) && t.mayStopAt(seg, seg.start+bytesRead) {
				break
			}

			if ts.Before(from) || ts.After(to) {
				bytesRead += recordLen
				continue
			}

			sample, err := t.readRecord(data)
			if err != nil {
				bytesRead += recordLen
				continue
			}

			keep(sample, seg.start+bytesRead, recordLen)
			bytesRead += recordLen
		}
	}

	if err := flush(); err != nil {
		return nil, err
	}
	return samples, nil
}

// ReadLatest returns the n most recent samples, oldest first. With a seek
// index it walks only the end of the ring.
func (t *Tier) ReadLatest(n int) ([]*AggregatedSample, error) {
	if t.readOnly {
		t.prepareReadOnlySeek()
	}
	t.mu.RLock()
	defer t.mu.RUnlock()

	if t.count == 0 || n <= 0 {
		return nil, nil
	}

	var samples []*AggregatedSample
	for _, loc := range t.latestRecords(n) {
		data := make([]byte, loc.length)
		if _, err := t.file.ReadAt(data, headerSize+loc.offset+4); err != nil {
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
	if t.readOnly {
		if t.file == nil {
			return nil
		}
		err := t.file.Close()
		t.file = nil
		t.clearReadOnlyStateLocked()
		return err
	}
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
	// The saved index lets the next start skip walking the ring. Losing it
	// costs only that walk.
	if t.seekReady {
		if err := t.saveSeekIndex(); err != nil {
			fmt.Printf("Storage: tier %s: could not save its seek index (%v); the next start rebuilds it.\n",
				filepath.Base(t.path), err)
		}
	}
	return t.file.Close()
}

func (t *Tier) Flush() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.readOnly {
		return ErrReadOnly
	}
	return t.writeHeader()
}

// readRecord decodes a payload using per-record format detection.
//
//   - recordKindChecksummed (0x03): binary written by current code; a record
//     that fails its checksum returns errRecordChecksum
//   - recordKindBinary (0x02): kind-tagged binary written before checksums
//   - '{' (0x7B):              legacy JSON record
//   - anything else:           legacy binary record (no kind byte)
//
// Every reader skips a record readRecord rejects.
func (t *Tier) readRecord(data []byte) (*AggregatedSample, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty record")
	}
	switch data[0] {
	case recordKindChecksummed:
		body, err := checkedRecordBody(data)
		if err != nil {
			return nil, err
		}
		return decodeSample(body)
	case recordKindBinary:
		return decodeSample(data[1:])
	case '{':
		return decodeSampleJSON(data)
	default:
		// Legacy binary record written before the kind-byte format.
		return decodeSample(data)
	}
}

// Resync verifies a checksummed candidate whole, which costs a CRC over the
// length it claims. A misaligned position rarely reads as a sane checksummed
// header, but a crafted file can repeat one every few bytes, each claiming
// close to a megabyte: resync then verified about a megabyte per byte it
// passed, and an 8 MiB tier took 90 s to scan. So each walk of a segment
// gets a budget for candidates that fail their checksum: it starts at
// resyncChecksumAllowance and earns resyncChecksumCredit for every byte
// resync passes. A candidate the budget cannot pay for is passed over
// unverified, like any position that starts no record. Real damage rarely
// puts more than a few false candidates before the next record, so the budget
// does not touch it, and a crafted file costs time linear in its size.
// addons/inspect_tier.py spends the same budget.
const (
	resyncChecksumAllowance = 64 << 20
	resyncChecksumCredit    = 16
)

// resyncBudget is the checksum work a walk may still spend on resync
// candidates that fail their checksum. One walk shares one budget across all
// its resyncs. Not safe for concurrent use; every walk makes its own.
type resyncBudget struct{ left int64 }

func newResyncBudget() *resyncBudget {
	return &resyncBudget{left: resyncChecksumAllowance}
}

// resyncToNextRecord advances br past a corrupt or zero-filled hole until it is
// positioned at the start of a plausible record, returning true on success and
// false if it reaches the end of the segment first. *bytesRead is advanced by
// the number of bytes skipped. budget bounds the checksums it verifies in vain.
//
// Such holes are the signature of an unclean shutdown: Kula does not fsync every
// write (see Close), so a power loss can leave the header's count/writeOff
// pointing past record payloads that never reached disk, which read back as
// zeros. Without this resync the scan would stop at the first zero record and
// silently hide every sample written after it. Only called on the active head
// segment, where any zero/garbage is corruption rather than the legitimate
// end-of-segment sentinel written on wrap.
func (t *Tier) resyncToNextRecord(br *bufio.Reader, bytesRead *int64, segSize int64, budget *resyncBudget) bool {
	// Checksummed records are verified whole. For records without a checksum,
	// peek a window large enough to hold a typical record plus the following
	// record's header, so resync can require TWO valid records in a row before
	// accepting a position. A single byte misalignment can momentarily mimic one
	// plausible record (a stray length + timestamp); it almost never mimics two.
	const resyncPeek = 4096
	for *bytesRead < segSize {
		remain := segSize - *bytesRead
		peekN := int64(resyncPeek)
		if peekN > remain {
			peekN = remain
		}
		hdr, _ := br.Peek(int(peekN))
		if dataLen, ok := t.recordHeaderSane(hdr); ok {
			next := 4 + dataLen
			switch {
			case *bytesRead+next > segSize:
				// Runs past the end of the segment, so it is no record of it.
				// Accepting it would hand the caller a record it rejects at
				// once, and the caller would resync here again, forever.
			case hdr[4] == recordKindChecksummed:
				// A checksummed record proves itself; a misaligned read cannot
				// match a trailer (barring a 1-in-2^32 coincidence), even where
				// the record is too large for the cross-check below. A torn one
				// is passed over too: the caller would skip it anyway. A record
				// too large to peek whole passes on its header, for free.
				if next > int64(br.Size()) {
					return true
				}
				if budget.left >= next {
					if peekedRecordIntact(br, next) {
						return true
					}
					budget.left -= next
				}
			case *bytesRead+next == segSize:
				// This record runs to the end of the segment: nothing follows to
				// cross-check, and the header itself is sane — accept.
				return true
			case next+13 <= int64(len(hdr)):
				// The next record's header is within the peek window — require it
				// to be sane too.
				if _, ok2 := t.recordHeaderSane(hdr[next:]); ok2 {
					return true
				}
			default:
				// Record larger than the peek window; fall back to the
				// single-record check (kind byte + sane timestamp).
				return true
			}
		}
		step := t.resyncStep(hdr)
		if _, err := br.Discard(step); err != nil {
			return false
		}
		*bytesRead += int64(step)
		budget.left += int64(step) * resyncChecksumCredit
	}
	return false
}

// resyncStep returns how far resync may advance from a position hdr (the
// peeked bytes there) shows starts no record: to the next position whose kind
// byte, four bytes in, could start one. A v2 tier's records all carry a kind
// byte, so the positions before it are no candidates, and a hole of zeros is
// passed at the speed of a byte search. A legacy tier advances one byte.
func (t *Tier) resyncStep(hdr []byte) int {
	if t.codecVer < codecVersion2 || len(hdr) <= 5 {
		return 1
	}
	rest := hdr[5:]
	next := bytes.IndexByte(rest, recordKindChecksummed)
	if older := bytes.IndexByte(rest, recordKindBinary); older >= 0 && (next < 0 || older < next) {
		next = older
	}
	if next < 0 {
		// No kind byte in the window: the first position whose kind byte lies
		// past it.
		return len(hdr) - 4
	}
	return next + 1
}

// recordHeaderSane reports whether hdr — positioned at a record's 4-byte length
// prefix — is a plausible record start, returning the payload length on success.
// It is the validator resync uses to find the first real record after a hole.
//
// For v2 files it requires the explicit kind byte at the exact offset; that is
// what rejects the off-by-N misalignments a byte-scan would otherwise accept
// (a coincidental length + a garbage timestamp read one byte early). It also
// bounds the timestamp to a sane window (this millennium, no later than the
// tier's newest sample plus slop). Legacy records (pre-kind-byte) are validated
// leniently on length + timestamp only; such files are migrated to v2 on open,
// so the strict path covers everything the readers see in practice.
func (t *Tier) recordHeaderSane(hdr []byte) (int64, bool) {
	if len(hdr) < 13 {
		return 0, false
	}
	if !t.validRecordLen(binary.LittleEndian.Uint32(hdr[0:4])) {
		return 0, false
	}
	dataLen := int64(binary.LittleEndian.Uint32(hdr[0:4]))
	var ts time.Time
	if hdr[4] == recordKindBinary || hdr[4] == recordKindChecksummed {
		ts = time.Unix(0, int64(binary.LittleEndian.Uint64(hdr[5:13])))
	} else {
		if t.codecVer >= codecVersion2 {
			// A v2 tier's records all carry the kind byte; anything else here is
			// a misaligned read into record interior, not a record boundary.
			return 0, false
		}
		ts = time.Unix(0, int64(binary.LittleEndian.Uint64(hdr[4:12])))
	}
	if ts.IsZero() || ts.Year() < 2000 {
		return 0, false
	}
	// A checksummed record proves itself (resync checks its trailer), so its
	// time is not bounded: records from before a backwards clock step are
	// later than the newest one.
	if hdr[4] != recordKindChecksummed && !t.newestTS.IsZero() && ts.After(t.newestTS.Add(time.Hour)) {
		return 0, false
	}
	return dataLen, true
}

// oldRecordSearch bounds how far oldRecordFrom looks: past the remains of
// one overwritten record and the few a crash can leave unaccounted, without
// scanning a whole ring of records that cannot prove themselves.
const oldRecordSearch = 16 << 20

// oldRecordFrom returns the first offset at or after from, within the ring
// and oldRecordSearch, where a record proves itself as resync accepts one: by
// its checksum, or for a record without one by a sane header followed by
// another. A caller that searches again from where a search left off passes
// the same budget, so the searches together stay linear. Caller holds t.mu
// or the tier is not shared yet.
func (t *Tier) oldRecordFrom(from int64, budget *resyncBudget) (int64, bool) {
	end := t.maxData
	if info, err := t.file.Stat(); err == nil {
		end = min(end, info.Size()-headerSize)
	}
	end = min(end, from+oldRecordSearch)
	if from < 0 || from >= end {
		return 0, false
	}
	br := scanReader(io.NewSectionReader(t.file, headerSize+from, end-from))
	defer releaseScanReader(br)
	var skipped int64
	if !t.resyncToNextRecord(br, &skipped, end-from, budget) {
		return 0, false
	}
	return from + skipped, true
}

// verifyOldSegment follows the previous pass from oldestOff to its end. A
// crash can leave the header behind the data: the records written past
// writeOff then fill the tail's place, followed by what is left of the old
// record they overwrote, so the pass seems to end there, and the writer's
// tail advance gives the rest of it up at that point. When the walk meets a
// record that is not one (a stray length, kind byte or checksum) before a zero
// length or the end of the ring, the pass resumes at the next record proven
// after it; without one it is left as it was. A zero length ends the segment
// as always: it is the sentinel, or a hole no walk can tell from one. Caller
// holds t.mu exclusively, or the tier is not shared yet.
func (t *Tier) verifyOldSegment() {
	if !t.wrapped || t.codecVer < codecVersion2 {
		return
	}
	budget := newResyncBudget()
	// The remains of the record the unaccounted ones overwrote come next and
	// may begin with what reads as a zero length, so resync past them
	// before walking.
	if end := t.unaccountedEnd(); end > t.oldestOff {
		if off, ok := t.oldRecordFrom(end, budget); ok {
			t.oldestOff = off
		}
	}
	for t.wrapped {
		broken, ok := t.oldSegmentBreak(t.oldestOff)
		if ok {
			return
		}
		off, found := t.oldRecordFrom(max(broken, t.oldestOff+1), budget)
		if !found {
			return
		}
		t.oldestOff = off
	}
	if ts, err := t.readTimestampAt(t.oldestOff); err == nil && !ts.IsZero() {
		t.oldestTS = ts
	}
}

// unaccountedEnd returns the end of the records a crash left past writeOff.
// Write updates the header after each record, so a power loss can keep
// records the header does not count; they are checksummed, newer than
// newestTS and in order, and the first record that is not all three ends
// them. Without such records it returns writeOff.
func (t *Tier) unaccountedEnd() int64 {
	off := t.writeOff
	if t.newestTS.IsZero() {
		return off
	}
	end := t.maxData
	if info, err := t.file.Stat(); err == nil {
		end = min(end, info.Size()-headerSize)
	}
	br := scanReader(io.NewSectionReader(t.file, headerSize+off, max(0, end-off)))
	defer releaseScanReader(br)
	last := t.newestTS.UnixNano()
	for end-off >= 13 {
		hdr, _ := br.Peek(13)
		if len(hdr) < 13 {
			break
		}
		dataLen := binary.LittleEndian.Uint32(hdr[0:4])
		recordLen := 4 + int64(dataLen)
		if !t.validRecordLen(dataLen) || off+recordLen > end || hdr[4] != recordKindChecksummed {
			break
		}
		ts := int64(binary.LittleEndian.Uint64(hdr[5:13]))
		if ts <= last || !peekedRecordIntact(br, recordLen) {
			break
		}
		if _, err := br.Discard(int(recordLen)); err != nil {
			break
		}
		last = ts
		off += recordLen
	}
	return off
}

// oldSegmentBreak walks the old segment from start and returns the offset of
// the first record that is not one, or ok when the walk reaches a zero length
// or the end of the ring first. A record without a kind byte (written before
// 0.11.0) is one unless it follows a kind-tagged record: a pass only ever
// moved from the old format to the new.
func (t *Tier) oldSegmentBreak(start int64) (int64, bool) {
	tagged := false
	end := t.maxData
	if info, err := t.file.Stat(); err == nil {
		end = min(end, info.Size()-headerSize)
	}
	br := scanReader(io.NewSectionReader(t.file, headerSize+start, max(0, end-start)))
	defer releaseScanReader(br)
	for off := start; ; {
		if end-off < 4 {
			return 0, true
		}
		hdr, _ := br.Peek(int(min(5, end-off)))
		if len(hdr) < 4 {
			return 0, true
		}
		dataLen := binary.LittleEndian.Uint32(hdr[0:4])
		if dataLen == 0 {
			return 0, true
		}
		recordLen := 4 + int64(dataLen)
		if !t.validRecordLen(dataLen) || off+recordLen > end || len(hdr) < 5 {
			return off, false
		}
		switch hdr[4] {
		case recordKindChecksummed:
			if !peekedRecordIntact(br, recordLen) {
				return off, false
			}
			tagged = true
		case recordKindBinary:
			tagged = true
		default:
			if tagged {
				return off, false
			}
		}
		if _, err := br.Discard(int(recordLen)); err != nil {
			return off, false
		}
		off += recordLen
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
	if !t.validRecordLen(dataLen) {
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
	if !t.validRecordLen(dataLen) {
		return time.Time{}, fmt.Errorf("invalid record length %d at offset %d", dataLen, dataOffset)
	}

	switch buf[4] {
	case recordKindBinary, recordKindChecksummed:
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
	// Bavail/Bsize widths vary by GOARCH (int32 on some platforms), so the
	// conversions are needed for cross-arch builds even if redundant here.
	available := int64(stat.Bavail) * int64(stat.Bsize) //nolint:unconvert // platform-dependent field widths
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
			if !t.validRecordLen(dataLen) {
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

// OldestDuration returns the duration of the oldest record in this tier, or 0
// when it cannot be read. A rollup tier's oldest record spans its whole
// window unless it is the store's first, which holds only the part of the
// window the store has run for.
func (t *Tier) OldestDuration() time.Duration {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.count == 0 || t.file == nil {
		return 0
	}
	dataLen, ok := t.recordLenAt(t.oldestOff)
	if !ok {
		return 0
	}
	data := make([]byte, dataLen)
	if _, err := t.file.ReadAt(data, headerSize+t.oldestOff+4); err != nil {
		return 0
	}
	sample, err := t.readRecord(data)
	if err != nil {
		return 0
	}
	return sample.Duration
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

// InspectLatestTierSample opens a tier read-only and decodes its newest record.
// It is intended for diagnostics: unlike InspectTierFile, it scans record
// boundaries in the data region and is therefore only called for verbose
// inspection.
func InspectLatestTierSample(path string) (*AggregatedSample, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	t := &Tier{file: f, path: path}
	if err := t.readHeader(); err != nil {
		return nil, fmt.Errorf("reading header: %w", err)
	}
	if t.count == 0 {
		return nil, nil
	}

	// The newest record is always in the active [0, writeOff) segment, even
	// after the ring has wrapped. Avoid scanning the older tail segment merely
	// to display one diagnostic record.
	t.wrapped = false
	t.oldestOff = 0
	samples, err := t.ReadLatest(1)
	if err != nil {
		return nil, err
	}
	if len(samples) == 0 {
		return nil, fmt.Errorf("latest record could not be decoded")
	}
	return samples[0], nil
}
