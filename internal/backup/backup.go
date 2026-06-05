// Package backup implements scheduled, consistent snapshots of the storage
// tier files into <storage.directory>/backup.
//
// On each cron tick the scheduler writes a timestamped sub-directory (named
// "20060102-150405", local time) containing a byte-for-byte copy of the raw
// tier file and, optionally, the aggregated tiers up to backup.maxtier. Copies
// are taken under the per-tier read lock so they never race the collection
// loop. With compression enabled each tier is stored as tier_N.dat.gz; gzip is
// performed outside the storage lock to keep the collection stall to the
// (page-cache-fast) raw copy only.
//
// Backups older than backup.retention are pruned on startup and after each run.
package backup

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"kula/internal/config"
)

// runDirLayout is the timestamp format used for per-run backup directories.
// It is also parsed back out during pruning, so backups Kula did not create
// (whose names don't match) are left untouched.
const runDirLayout = "20060102-150405"

// tmpSuffix marks an in-progress run directory; it is renamed to its final name
// only after every tier has been copied, so a crash mid-backup never leaves a
// partial run that looks complete.
const tmpSuffix = ".tmp"

// Snapshotter is the subset of *storage.Store the scheduler needs. Kept as an
// interface so the scheduler can be tested without a real store.
type Snapshotter interface {
	TierCount() int
	SnapshotTier(i int, w io.Writer) (int64, error)
}

// Scheduler runs periodic tier backups according to a cron schedule.
type Scheduler struct {
	store     Snapshotter
	schedule  *Schedule
	backupDir string
	maxTier   int
	retention time.Duration
	compress  bool
}

// New builds a Scheduler from the parsed backup configuration. It validates the
// cron expression (the one piece config.Load defers to here).
func New(store Snapshotter, storageDir string, cfg config.BackupConfig) (*Scheduler, error) {
	sched, err := ParseSchedule(cfg.Cron)
	if err != nil {
		return nil, fmt.Errorf("backup.cron: %w", err)
	}
	if cfg.MaxTier < 1 {
		return nil, fmt.Errorf("backup.maxtier must be >= 1, got %d", cfg.MaxTier)
	}
	return &Scheduler{
		store:     store,
		schedule:  sched,
		backupDir: filepath.Join(storageDir, "backup"),
		maxTier:   cfg.MaxTier,
		retention: cfg.RetentionDur,
		compress:  cfg.Compress,
	}, nil
}

// Run drives the scheduler until ctx is cancelled. It wakes once per minute,
// aligned to the wall-clock minute boundary, and triggers a backup whenever the
// schedule matches. It returns when ctx is done.
func (s *Scheduler) Run(ctx context.Context) {
	if err := os.MkdirAll(s.backupDir, 0750); err != nil {
		log.Printf("backup: cannot create backup directory %s: %v", s.backupDir, err)
		return
	}

	// Sweep any leftover partial runs and expired backups before the first tick.
	s.cleanup(time.Now())

	// Align to the next minute boundary so Matches is evaluated once per minute.
	next := time.Now().Truncate(time.Minute).Add(time.Minute)
	timer := time.NewTimer(time.Until(next))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
	}

	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	var lastRun time.Time
	check := func(now time.Time) {
		now = now.Truncate(time.Minute)
		if now.Equal(lastRun) {
			return
		}
		if s.schedule.Matches(now) {
			lastRun = now
			if err := s.RunBackup(now); err != nil {
				log.Printf("backup: run failed: %v", err)
			}
			s.cleanup(now)
		}
	}

	check(time.Now())
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			check(now)
		}
	}
}

// RunBackup performs a single backup run timestamped at now. It writes into a
// temporary directory that is renamed into place only once every tier has been
// copied successfully.
func (s *Scheduler) RunBackup(now time.Time) error {
	runName := now.Format(runDirLayout)
	runDir := filepath.Join(s.backupDir, runName)
	tmpDir := runDir + tmpSuffix

	if err := os.RemoveAll(tmpDir); err != nil {
		return fmt.Errorf("clearing stale temp dir: %w", err)
	}
	if err := os.MkdirAll(tmpDir, 0750); err != nil {
		return fmt.Errorf("creating temp dir: %w", err)
	}

	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(tmpDir)
		}
	}()

	n := s.maxTier
	if avail := s.store.TierCount(); n > avail {
		log.Printf("backup: maxtier %d exceeds %d configured tiers; backing up %d", s.maxTier, avail, avail)
		n = avail
	}
	if n < 1 {
		return fmt.Errorf("no tiers available to back up")
	}

	for i := 0; i < n; i++ {
		if err := s.backupTier(i, tmpDir); err != nil {
			return fmt.Errorf("tier %d: %w", i, err)
		}
	}

	if err := os.Rename(tmpDir, runDir); err != nil {
		return fmt.Errorf("finalizing run dir: %w", err)
	}
	committed = true

	log.Printf("backup: wrote %d tier(s) to %s", n, runDir)
	return nil
}

// backupTier copies tier i into dir. The consistent raw copy is taken under the
// storage lock; gzip compression (when enabled) runs afterwards from the
// already-written raw file so the lock is not held during the slow compress.
func (s *Scheduler) backupTier(i int, dir string) error {
	rawPath := filepath.Join(dir, fmt.Sprintf("tier_%d.dat", i))
	raw, err := os.OpenFile(rawPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = raw.Close() }()

	if _, err := s.store.SnapshotTier(i, raw); err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	if err := raw.Sync(); err != nil {
		return fmt.Errorf("syncing snapshot: %w", err)
	}

	if !s.compress {
		return raw.Close()
	}

	if _, err := raw.Seek(0, io.SeekStart); err != nil {
		return err
	}
	gzPath := rawPath + ".gz"
	gzFile, err := os.OpenFile(gzPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	gw := gzip.NewWriter(gzFile)
	if _, err := io.Copy(gw, raw); err != nil {
		_ = gw.Close()
		_ = gzFile.Close()
		return fmt.Errorf("compressing: %w", err)
	}
	if err := gw.Close(); err != nil {
		_ = gzFile.Close()
		return fmt.Errorf("finishing gzip: %w", err)
	}
	if err := gzFile.Sync(); err != nil {
		_ = gzFile.Close()
		return fmt.Errorf("syncing gzip: %w", err)
	}
	if err := gzFile.Close(); err != nil {
		return err
	}
	if err := raw.Close(); err != nil {
		return err
	}
	// Drop the uncompressed intermediate; only the .gz is kept.
	return os.Remove(rawPath)
}

// cleanup removes leftover in-progress (.tmp) run directories and, when
// retention is positive, prunes completed runs older than the cutoff. Failures
// are logged and otherwise ignored so a single un-removable entry never stops
// the scheduler.
func (s *Scheduler) cleanup(now time.Time) {
	entries, err := os.ReadDir(s.backupDir)
	if err != nil {
		return
	}
	var cutoff time.Time
	prune := s.retention > 0
	if prune {
		cutoff = now.Add(-s.retention)
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		full := filepath.Join(s.backupDir, name)

		if filepath.Ext(name) == tmpSuffix {
			if err := os.RemoveAll(full); err != nil {
				log.Printf("backup: failed to remove stale temp dir %s: %v", name, err)
			}
			continue
		}

		if !prune {
			continue
		}
		ts, err := time.ParseInLocation(runDirLayout, name, time.Local)
		if err != nil {
			// Not a backup directory we created; leave it alone.
			continue
		}
		if ts.Before(cutoff) {
			if err := os.RemoveAll(full); err != nil {
				log.Printf("backup: failed to prune expired backup %s: %v", name, err)
			} else {
				log.Printf("backup: pruned expired backup %s", name)
			}
		}
	}
}
