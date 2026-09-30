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
// across processes, though: a scan that races the writer overwriting the
// oldest records of a wrapped ring can end that segment early or skip a torn
// record. The decoder never panics on such bytes, and the next Refresh and
// query read a consistent ring again, so results are a view, not an archive.
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
