package collector

import "testing"

func TestParseDiskStats(t *testing.T) {
	procPath = "testdata/proc"

	raw := parseDiskStats()
	if len(raw) != 1 {
		t.Fatalf("expected 1 disk, got %d", len(raw))
	}
	sda, ok := raw["sda"]
	if !ok {
		t.Fatalf("missing sda stats")
	}
	if sda.reads != 1000 || sda.writes != 500 {
		t.Errorf("unexpected sda stats: %+v", sda)
	}
	if sda.readSect != 20000 || sda.writeSect != 10000 {
		t.Errorf("unexpected sda sectors: %+v", sda)
	}
}

func TestCollectFileSystems(t *testing.T) {
	procPath = "testdata/proc"

	fs := collectFileSystems()
	// Note: syscall.Statfs is executed on the mount point. Since the mock file says "/",
	// and "/" exists on the real system, it will return real disk space stats for "/".
	if len(fs) != 1 {
		t.Fatalf("expected 1 filesystem, got %d", len(fs))
	}
	if fs[0].Device != "/dev/sda1" || fs[0].MountPoint != "/" || fs[0].FSType != "ext4" {
		t.Errorf("unexpected fs info: %+v", fs[0])
	}
}
