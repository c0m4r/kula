package collector

import (
	"testing"

	"kula/internal/config"
)

func TestParseDiskStats(t *testing.T) {
	procPath = "testdata/proc"

	c := New(config.GlobalConfig{}, config.CollectionConfig{}, config.ApplicationsConfig{}, "")
	raw := c.parseDiskStats()
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
	if sda.ioTicks != 150 {
		t.Errorf("sda io_ticks = %d, want 150", sda.ioTicks)
	}
}

func TestCollectFileSystems(t *testing.T) {
	procPath = "testdata/proc"

	c := New(config.GlobalConfig{}, config.CollectionConfig{}, config.ApplicationsConfig{}, "")
	fs := c.collectFileSystems()
	// Note: syscall.Statfs is executed on the mount point. Since the mock file says "/",
	// and "/" exists on the real system, it will return real disk space stats for "/".
	if len(fs) != 1 {
		t.Fatalf("expected 1 filesystem, got %d", len(fs))
	}
	if fs[0].Device != "/dev/sda1" || fs[0].MountPoint != "/" || fs[0].FSType != "ext4" {
		t.Errorf("unexpected fs info: %+v", fs[0])
	}
}

func TestCollectFileSystemsDocker(t *testing.T) {
	procPath = "testdata/docker_proc"

	c := New(config.GlobalConfig{}, config.CollectionConfig{}, config.ApplicationsConfig{}, "")
	fs := c.collectFileSystems()
	// Should only have 'overlay' at '/'
	// /etc/resolv.conf etc should be ignored
	// tmpfs and shm should be filtered by fstype switch
	if len(fs) != 1 {
		t.Fatalf("expected 1 filesystem (overlay), got %d: %+v", len(fs), fs)
	}
	if fs[0].FSType != "overlay" || fs[0].MountPoint != "/" {
		t.Errorf("expected overlay at /, got %s at %s", fs[0].FSType, fs[0].MountPoint)
	}
}
func TestParseDiskStatsConfig(t *testing.T) {
	procPath = "testdata/proc_with_partitions"

	// 1. Default (no config) - should skip sda1, sda2
	c1 := New(config.GlobalConfig{}, config.CollectionConfig{}, config.ApplicationsConfig{}, "")
	raw1 := c1.parseDiskStats()
	if len(raw1) != 1 {
		t.Errorf("expected 1 disk (sda), got %d: %v", len(raw1), raw1)
	}
	if _, ok := raw1["sda"]; !ok {
		t.Errorf("missing sda")
	}

	// 2. Explicit config - should allow sda1 even if it's a partition
	c2 := New(config.GlobalConfig{}, config.CollectionConfig{
		Devices: []string{"sda", "sda1"},
	}, config.ApplicationsConfig{}, "")
	raw2 := c2.parseDiskStats()
	if len(raw2) != 2 {
		t.Errorf("expected 2 disks (sda, sda1), got %d: %v", len(raw2), raw2)
	}
	if _, ok := raw2["sda1"]; !ok {
		t.Errorf("missing sda1 with explicit config")
	}
}

func TestIsPartition(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"sda", false},
		{"sda1", true},
		{"sdb", false},
		{"sdb10", true},
		{"nvme0n1", false},
		{"nvme0n1p1", true},
		{"mmcblk0", false},
		{"mmcblk0p2", true},
		{"vda", false},
		{"vda1", true},
		{"xvda", false},
		{"xvda1", true},
	}

	for _, tt := range tests {
		if got := isPartition(tt.name); got != tt.want {
			t.Errorf("isPartition(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestCollectFileSystemsWithExplicitMountPoints(t *testing.T) {
	procPath = "testdata/docker_proc"

	// Try collecting with MountPoints filter set to "/dev/shm", which has "tmpfs" type.
	c := New(config.GlobalConfig{}, config.CollectionConfig{
		MountPoints: []string{"/dev/shm"},
	}, config.ApplicationsConfig{}, "")
	fs := c.collectFileSystems()

	// Since we specified "/dev/shm" explicitly, it should ignore the fstype constraint (tmpfs is not in realFSTypes)
	// and monitor "/dev/shm".
	if len(fs) != 1 {
		t.Fatalf("expected 1 filesystem, got %d: %+v", len(fs), fs)
	}
	if fs[0].MountPoint != "/dev/shm" {
		t.Errorf("expected mount point /dev/shm, got %s", fs[0].MountPoint)
	}
	if fs[0].FSType != "/dev/shm" && fs[0].FSType != "tmpfs" {
		t.Errorf("expected fstype tmpfs, got %s", fs[0].FSType)
	}
}
