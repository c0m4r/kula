package storage

import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"kula/internal/collector"
)

// within runs fn and fails the test if it has not returned after a few
// seconds: an open that blocks on a FIFO would hang the test.
func within(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s still blocked after 5 s", what)
	}
}

// victim returns a file outside the storage directory, with its contents, for
// a planted link to point at.
func victim(t *testing.T) (string, []byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "elsewhere")
	contents := []byte("not Kula's to write\n")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, contents
}

func unchanged(t *testing.T, path string, want []byte) {
	t.Helper()
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("the file a planted link points to was changed: %q, %v", got, err)
	}
}

// TestOwnerOpensOnlyRegularFiles: the files the owner keeps in its storage
// directory are never symbolic links or FIFOs. A link planted in place of one
// had Kula overwrite the file it points to, and a FIFO in place of the seek
// index sidecar blocked startup for good.
func TestOwnerOpensOnlyRegularFiles(t *testing.T) {
	base := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	// closedTier writes a tier with a few records and closes it, which saves
	// its sidecar.
	closedTier := func(t *testing.T) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "tier_0.dat")
		tier, err := OpenTier(path, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 5; i++ {
			if err := tier.Write(varSample(base.Add(time.Duration(i)*time.Second), 2)); err != nil {
				t.Fatal(err)
			}
		}
		if err := tier.Close(); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Run("tier is a link", func(t *testing.T) {
		target, contents := victim(t)
		path := filepath.Join(t.TempDir(), "tier_0.dat")
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		if tier, err := OpenTier(path, 1<<20); err == nil {
			_ = tier.Close()
			t.Fatal("OpenTier opened a tier through a symbolic link")
		}
		unchanged(t, target, contents)
	})

	t.Run("tier is a FIFO", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "tier_0.dat")
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Skipf("mkfifo: %v", err)
		}
		within(t, "OpenTier", func() {
			if tier, err := OpenTier(path, 1<<20); err == nil {
				_ = tier.Close()
				t.Error("OpenTier opened a FIFO as a tier")
			}
		})
	})

	t.Run("sidecar is a FIFO", func(t *testing.T) {
		path := closedTier(t)
		sidecar := seekSidecarPath(path)
		if err := os.Remove(sidecar); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(sidecar, 0o600); err != nil {
			t.Skipf("mkfifo: %v", err)
		}
		within(t, "OpenTier", func() {
			tier, err := OpenTier(path, 1<<20)
			if err != nil {
				t.Error(err)
				return
			}
			defer func() { _ = tier.Close() }()
			if got, err := tier.ReadRange(base, base.Add(time.Minute)); err != nil || len(got) != 5 {
				t.Errorf("ReadRange returned %d records, err %v; want 5", len(got), err)
			}
		})
	})

	t.Run("sidecar is a link", func(t *testing.T) {
		path := closedTier(t)
		sidecar := seekSidecarPath(path)
		moved := filepath.Join(t.TempDir(), "tier_0.idx")
		if err := os.Rename(sidecar, moved); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(moved, sidecar); err != nil {
			t.Fatal(err)
		}
		tier, err := OpenTier(path, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tier.Close() }()
		if tier.seekFromSidecar {
			t.Fatal("OpenTier adopted a seek index through a symbolic link")
		}
	})

	t.Run("migration copy is a link", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "tier_0.dat")
		writeLegacyJSONTier(t, path, 64<<10, &AggregatedSample{
			Timestamp: base, Duration: time.Second,
			Data: &collector.Sample{Timestamp: base, System: collector.SystemStats{Hostname: "host"}},
		})
		target, contents := victim(t)
		if err := os.Symlink(target, path+".migration"); err != nil {
			t.Fatal(err)
		}
		if tier, err := OpenTier(path, 64<<10+headerSize); err == nil {
			_ = tier.Close()
			t.Fatal("the migration wrote its copy through a symbolic link")
		}
		unchanged(t, target, contents)
		if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {
			t.Fatalf("the tier is no longer a regular file: %v, %v", info, err)
		}
	})
}

// TestReadOnlyViewsOpenOnlyRegularFiles: the terminal UI (often run as root)
// and `kula inspect` open tier files another process owns. A FIFO planted in
// place of one blocked them for good, the TUI before it drew anything, and a
// link had them read whatever it points to.
func TestReadOnlyViewsOpenOnlyRegularFiles(t *testing.T) {
	base := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	tierAt := func(t *testing.T, dir string) string {
		t.Helper()
		path := filepath.Join(dir, "tier_0.dat")
		tier, err := OpenTier(path, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		if err := tier.Write(varSample(base, 2)); err != nil {
			t.Fatal(err)
		}
		if err := tier.Close(); err != nil {
			t.Fatal(err)
		}
		return path
	}
	views := func(t *testing.T, dir string) {
		t.Helper()
		path := filepath.Join(dir, "tier_0.dat")
		within(t, "opening the views", func() {
			if store, err := OpenReadOnly(readOnlyTestConfig(dir, 1<<20)); err == nil {
				_ = store.Close()
				t.Error("OpenReadOnly opened the tier")
			}
			if _, err := InspectTierFile(path); err == nil {
				t.Error("InspectTierFile read the tier")
			}
			if _, err := InspectLatestTierSample(path); err == nil {
				t.Error("InspectLatestTierSample read the tier")
			}
		})
	}

	t.Run("FIFO", func(t *testing.T) {
		dir := t.TempDir()
		if err := syscall.Mkfifo(filepath.Join(dir, "tier_0.dat"), 0o600); err != nil {
			t.Skipf("mkfifo: %v", err)
		}
		views(t, dir)
	})

	t.Run("link", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Symlink(tierAt(t, t.TempDir()), filepath.Join(dir, "tier_0.dat")); err != nil {
			t.Fatal(err)
		}
		views(t, dir)
	})

	t.Run("FIFO swapped in under an open view", func(t *testing.T) {
		dir := t.TempDir()
		path := tierAt(t, dir)
		store, err := OpenReadOnly(readOnlyTestConfig(dir, 1<<20))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Skipf("mkfifo: %v", err)
		}
		// A blocked Refresh holds the tier lock, so Close only after it
		// returned.
		within(t, "Refresh", func() {
			if err := store.Refresh(); err == nil {
				t.Error("Refresh opened a FIFO as the tier")
			}
		})
		_ = store.Close()
	})
}
