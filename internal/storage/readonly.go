package storage

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"kula/internal/config"
)

// ErrReadOnly is returned by every write path of a store or tier opened with
// OpenReadOnly.
var ErrReadOnly = errors.New("storage is opened read-only")

// OpenReadOnly opens the configured tier files for history queries without
// taking ownership of them, so a second process (the terminal UI) can chart
// what a running `kula serve` records. Nothing is created, migrated, resized
// or rewritten: files are opened O_RDONLY, a missing directory or tier file
// reads as empty until it appears, and Close only releases descriptors.
//
// The owning process rewrites each tier header after every record, so Refresh
// adopts its current ring geometry before a query. Its tier locks do not reach
// across processes, so every scan checks itself against the header as it goes
// (see readOnlyScan): a scan the owner overtook while overwriting the oldest
// records is retried on a fresh snapshot, and QueryRangeWithMeta reports
// errHistorySnapshotExpired if that keeps happening, rather than return torn
// records. Results are a view, not an archive.
//
// A read-only store has no latest-sample cache; QueryLatest returns nil.
func OpenReadOnly(cfg config.StorageConfig) (*Store, error) {
	absDir, err := filepath.Abs(cfg.Directory)
	if err != nil {
		return nil, fmt.Errorf("resolving storage directory: %w", err)
	}

	s := &Store{
		dir:           absDir,
		configs:       cfg.Tiers,
		readOnly:      true,
		queryCache:    make(map[queryCacheKey]queryCacheEntry),
		queryCacheTTL: time.Second,
	}
	if len(cfg.Tiers) > 0 && cfg.Tiers[0].Resolution > 0 {
		s.queryCacheTTL = cfg.Tiers[0].Resolution
	}
	for i := range cfg.Tiers {
		tier := &Tier{
			path:     filepath.Join(absDir, fmt.Sprintf("tier_%d.dat", i)),
			readOnly: true,
		}
		if err := tier.reloadReadOnly(); err != nil {
			_ = tier.Close()
			_ = s.Close()
			return nil, err
		}
		s.tiers = append(s.tiers, tier)
	}
	return s, nil
}

// ReadOnly reports whether the store was opened with OpenReadOnly.
func (s *Store) ReadOnly() bool {
	return s.readOnly
}

// Dir returns the absolute storage directory the store reads.
func (s *Store) Dir() string {
	return s.dir
}

// Refresh re-reads every tier header of a read-only store so later queries
// cover records the owning process has written since the last refresh. It
// also picks up tier files created, or replaced, after the store was opened.
// It is a no-op on a store that owns its files, which is always current.
func (s *Store) Refresh() error {
	if !s.readOnly {
		return nil
	}
	var firstErr error
	for _, tier := range s.tiers {
		if err := tier.reloadReadOnly(); err != nil && firstErr == nil {
			firstErr = err
		}
	}

	// Results computed from the previous headers describe an older ring.
	s.mu.Lock()
	s.writeGeneration++
	s.queryCacheMu.Lock()
	clear(s.queryCache)
	s.queryCacheMu.Unlock()
	s.mu.Unlock()
	return firstErr
}

// reloadReadOnly (re)opens a read-only tier and adopts the header its owning
// process last wrote. A tier whose file does not exist yet, or is still too
// short to hold a header, reads as empty.
func (t *Tier) reloadReadOnly() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Stat and open errors are *PathError values that already name the file.
	info, err := os.Stat(t.path)
	if err != nil {
		t.resetReadOnlyLocked()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}

	// Moving a tier aside and restarting the owner replaces the file; keep
	// reading the path, not the orphaned inode.
	if t.file != nil {
		if current, statErr := t.file.Stat(); statErr != nil || !os.SameFile(current, info) {
			t.resetReadOnlyLocked()
		}
	}
	if t.file == nil {
		file, openErr := os.Open(t.path)
		if openErr != nil {
			return openErr
		}
		t.file = file
	}
	if info.Size() < headerSize {
		t.clearReadOnlyStateLocked()
		return nil
	}

	next := Tier{file: t.file, path: t.path}
	if err := next.readHeader(); err != nil {
		t.clearReadOnlyStateLocked()
		return fmt.Errorf("tier %s: unreadable header: %w", t.path, err)
	}

	// Mirror Write's pass bookkeeping so a scan that releases the lock between
	// batches notices when a refresh reveals the writer lapped its snapshot.
	if t.count > 0 && (next.writeOff < t.writeOff || next.count < t.count) {
		t.writeCycle++
	}
	t.maxData = next.maxData
	t.writeOff = next.writeOff
	t.writeEnd = next.writeOff
	t.count = next.count
	t.oldestTS = next.oldestTS
	t.newestTS = next.newestTS
	t.wrapped = next.wrapped
	t.oldestOff = next.oldestOff
	t.codecVer = next.codecVer
	return nil
}

// resetReadOnlyLocked releases the file and empties the tier. file == nil
// always implies count == 0, so no reader touches a missing file.
func (t *Tier) resetReadOnlyLocked() {
	if t.file != nil {
		_ = t.file.Close()
		t.file = nil
	}
	t.clearReadOnlyStateLocked()
}

func (t *Tier) clearReadOnlyStateLocked() {
	t.writeOff = 0
	t.writeEnd = 0
	t.count = 0
	t.oldestTS = time.Time{}
	t.newestTS = time.Time{}
	t.wrapped = false
	t.oldestOff = 0
}

// readOnlyCheckEvery is how many records a read-only scan traverses between
// checks of the owner's header.
const readOnlyCheckEvery = 256

// readOnlyScan guards one scan of a read-only tier. The owner writes the file
// from another process, so t.mu does not hold it off.
//
// The scan reads the ring the last refresh adopted. The owner writes on from
// that snapshot's writeOff in ring order, over the oldest records, so every
// position below is a ring distance counted from writeOff: the bytes the
// owner has changed since the snapshot are the first `written` of them, and
// the record it may be writing at this moment comes right after.
//
// Every readOnlyCheckEvery records, before a batch is handed on and at the
// end, check re-reads the header. A record traversed since the previous check
// that lies in written bytes may be torn, and so may everything after it,
// since each length prefix positions the next record: the scan is abandoned
// with errHistorySnapshotExpired and the store retries it on a fresh
// snapshot. Records that passed a check were read before the owner reached
// them and stand. When the owner did write, samples near its next write are
// left out as well, since that write may be under way. The owner writes once
// per collection interval and a scan runs far faster, so the owner stays
// behind the scan and retries are rare.
type readOnlyScan struct {
	tier     *Tier
	writeOff int64
	maxData  int64
	count    uint64
	newest   time.Time

	// uncheckedFrom is the distance where the records traversed since the
	// last check begin, or -1 when none has been.
	uncheckedFrom int64
	traversed     int
	// pending holds the span of each sample kept since the last check, in
	// the order the scan appended them.
	pending []recordSpan
	longest int64
}

type recordSpan struct{ distance, length int64 }

// newReadOnlyScan snapshots the ring geometry a scan is about to read. The
// caller holds t.mu.
func (t *Tier) newReadOnlyScan() *readOnlyScan {
	return &readOnlyScan{
		tier:          t,
		writeOff:      t.writeOff,
		maxData:       t.maxData,
		count:         t.count,
		newest:        t.newestTS,
		uncheckedFrom: -1,
	}
}

func (g *readOnlyScan) distance(offset int64) int64 {
	return ((offset-g.writeOff)%g.maxData + g.maxData) % g.maxData
}

// due counts a record the scan is about to traverse and reports whether a
// check is due before it.
func (g *readOnlyScan) due() bool {
	g.traversed++
	return g.traversed%readOnlyCheckEvery == 0
}

// reached notes that the scan traverses the record at offset.
func (g *readOnlyScan) reached(offset int64) {
	if g.uncheckedFrom < 0 {
		g.uncheckedFrom = g.distance(offset)
	}
}

// kept records the span of a sample the scan appended.
func (g *readOnlyScan) kept(offset, length int64) {
	g.pending = append(g.pending, recordSpan{distance: g.distance(offset), length: length})
	g.longest = max(g.longest, length)
}

// check validates the samples kept since the last check, which are the last
// len(g.pending) of samples, and returns samples without those that may be
// torn.
func (g *readOnlyScan) check(samples []*AggregatedSample) ([]*AggregatedSample, error) {
	now := Tier{file: g.tier.file, path: g.tier.path}
	if err := now.readHeader(); err != nil || now.maxData != g.maxData || now.count < g.count {
		// Unreadable, resized or rewritten: the snapshot describes another ring.
		return nil, errHistorySnapshotExpired
	}
	var written int64
	if now.count > g.count {
		written = g.distance(now.writeOff)
		// Back at the same offset, or every record of the snapshot gone: the
		// owner came all the way round the ring during the scan.
		if written == 0 || (!g.newest.IsZero() && now.oldestTS.After(g.newest)) {
			return nil, errHistorySnapshotExpired
		}
		if g.uncheckedFrom >= 0 && g.uncheckedFrom < written {
			return nil, errHistorySnapshotExpired
		}
	}

	// An active owner's next record starts at `written`; allow for it being
	// longer than any seen here, and for the end-of-segment sentinel on a wrap.
	var reach int64
	if written > 0 {
		reach = written + 2*g.longest + 4
	}
	first := len(samples) - len(g.pending)
	out := samples[:first]
	for i, span := range g.pending {
		if span.distance >= reach {
			out = append(out, samples[first+i])
		}
	}
	g.pending = g.pending[:0]
	g.uncheckedFrom = -1
	return out, nil
}

// LayoutMismatch describes how the tier files on disk differ from the tiers
// the store was opened with, or returns "" when they agree. A read-only store
// opened with another config than the owner's still reads the files, but
// picks tiers and labels their resolutions from its own config, which the
// files do not record. Only tier sizes and the number of tier files can be
// compared. The owner itself may differ in size only after a max_size was
// lowered, which it does not apply to an existing file.
func (s *Store) LayoutMismatch() string {
	if !s.readOnly {
		return ""
	}
	for i, tier := range s.tiers {
		tier.mu.RLock()
		size, open := tier.maxData+headerSize, tier.file != nil && tier.maxData > 0
		tier.mu.RUnlock()
		if open && i < len(s.configs) && s.configs[i].MaxBytes > 0 && size != s.configs[i].MaxBytes {
			return fmt.Sprintf("tier %d is %s on disk but %s in this configuration",
				i, sizeText(size), sizeText(s.configs[i].MaxBytes))
		}
	}
	extra := filepath.Join(s.dir, fmt.Sprintf("tier_%d.dat", len(s.tiers)))
	if _, err := os.Stat(extra); err == nil {
		return fmt.Sprintf("the directory holds more tiers than the %d configured", len(s.tiers))
	}
	return ""
}

func sizeText(bytes int64) string {
	for _, unit := range []struct {
		size int64
		name string
	}{{1 << 30, "GiB"}, {1 << 20, "MiB"}, {1 << 10, "KiB"}} {
		if bytes >= unit.size {
			return fmt.Sprintf("%.4g %s", float64(bytes)/float64(unit.size), unit.name)
		}
	}
	return fmt.Sprintf("%d B", bytes)
}
