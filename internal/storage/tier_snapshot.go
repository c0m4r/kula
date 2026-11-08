package storage

import (
	"fmt"
	"io"
	"os"
)

// snapshotBufferSize sizes the reads and writes of a snapshot copy.
const snapshotBufferSize = 1 << 20

// SnapshotTo writes a byte-for-byte copy of the tier file to dst: a
// self-contained tier file that reopens with OpenTier, equal to the file as it
// was when SnapshotTo returned. It holds the tier lock only for moments, so
// Write, and the collection loop waiting on it, keep running during the copy:
//
//  1. Under the read lock, note where the writer is: its write offset and
//     ring pass.
//  2. Copy the whole file without the lock.
//  3. Under the read lock again, copy what the writer changed meanwhile: the
//     header, and the ring from the noted write offset to the current one,
//     through the end of the ring and on from its start if it wrapped (the
//     wrap's end-of-segment sentinel lies in between). Write changes no other
//     byte, so the rest of the copy already matches.
//
// A writer that laps the ring during the copy, which only a tiny ring allows,
// makes step 3 copy the whole file again. A Close during step 2 fails the
// snapshot with a closed-file error.
func (t *Tier) SnapshotTo(dst io.WriterAt) (int64, error) {
	t.mu.RLock()
	f := t.file
	startOff, startCycle := t.writeOff, t.writeCycle
	t.mu.RUnlock()
	if f == nil {
		return 0, fmt.Errorf("tier %s: no file to snapshot", t.path)
	}
	size, err := snapshotFileSize(f)
	if err != nil {
		return 0, fmt.Errorf("stat tier %s: %w", t.path, err)
	}
	buf := make([]byte, snapshotBufferSize)
	if err := copySpan(dst, f, 0, size, buf); err != nil {
		return 0, fmt.Errorf("copying tier %s: %w", t.path, err)
	}
	if t.snapshotCopied != nil {
		t.snapshotCopied()
	}

	t.mu.RLock()
	defer t.mu.RUnlock()
	if size, err = snapshotFileSize(f); err != nil {
		return 0, fmt.Errorf("stat tier %s: %w", t.path, err)
	}
	// A failed write may have left bytes up to writeEnd; copy those too.
	end := headerSize + max(t.writeOff, t.writeEnd)
	spans := [][2]int64{{0, headerSize}}
	switch t.writeCycle - startCycle {
	case 0:
		spans = append(spans, [2]int64{headerSize + startOff, end})
	case 1:
		spans = append(spans, [2]int64{headerSize + startOff, headerSize + t.maxData}, [2]int64{headerSize, end})
	default:
		spans = [][2]int64{{0, size}}
	}
	for _, span := range spans {
		if err := copySpan(dst, f, span[0], min(span[1], size), buf); err != nil {
			return 0, fmt.Errorf("copying tier %s: %w", t.path, err)
		}
	}
	return size, nil
}

func snapshotFileSize(f *os.File) (int64, error) {
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// copySpan copies src[from:to) to the same offsets of dst.
func copySpan(dst io.WriterAt, src *os.File, from, to int64, buf []byte) error {
	if to <= from {
		return nil
	}
	_, err := io.CopyBuffer(io.NewOffsetWriter(dst, from), io.NewSectionReader(src, from, to-from), buf)
	return err
}
