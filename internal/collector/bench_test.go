package collector

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildProcFixtures writes realistic /proc/{meminfo,stat,net/dev} files into a
// temp dir and points procPath at it for the duration of the benchmark. The
// shapes mirror a busy multi-core host: a 32-core /proc/stat with large intr
// and softirq lines, a full 58-line meminfo, and several network interfaces.
func buildProcFixtures(tb testing.TB) {
	tb.Helper()
	dir := tb.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "meminfo"), []byte(realMeminfo), 0o644); err != nil {
		tb.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(buildProcStat(32)), 0o644); err != nil {
		tb.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "net"), 0o755); err != nil {
		tb.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "net", "dev"), []byte(realNetDev), 0o644); err != nil {
		tb.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "diskstats"), []byte(buildDiskStats(40)), 0o644); err != nil {
		tb.Fatal(err)
	}

	orig := procPath
	procPath = dir
	tb.Cleanup(func() { procPath = orig })
}

// buildDiskStats synthesises a /proc/diskstats with a couple of real disks and
// their partitions plus many loop and dm- devices (as seen on snap-heavy or
// LVM hosts) — all of which the parser must skip every tick.
func buildDiskStats(loops int) string {
	const cols = " 12345 678 9012345 6789 23456 789 3456789 8901 0 4567 8901 0 0 0 0 0 0"
	var b strings.Builder
	b.WriteString("   8       0 sda" + cols + "\n")
	b.WriteString("   8       1 sda1" + cols + "\n")
	b.WriteString("   8       2 sda2" + cols + "\n")
	b.WriteString("   8      16 sdb" + cols + "\n")
	b.WriteString(" 259       0 nvme0n1" + cols + "\n")
	b.WriteString(" 259       1 nvme0n1p1" + cols + "\n")
	for i := 0; i < loops; i++ {
		b.WriteString("   7   " + itoa(i) + " loop" + itoa(i) + cols + "\n")
	}
	for i := 0; i < 4; i++ {
		b.WriteString(" 253   " + itoa(i) + " dm-" + itoa(i) + cols + "\n")
	}
	return b.String()
}

// buildProcessTree creates n numeric PID dirs each holding a realistic stat file
// and points procPath at the tree.
func buildProcessTree(tb testing.TB, n int) {
	tb.Helper()
	dir := tb.TempDir()
	for i := 1; i <= n; i++ {
		pid := itoa(i)
		pdir := filepath.Join(dir, pid)
		if err := os.MkdirAll(pdir, 0o755); err != nil {
			tb.Fatal(err)
		}
		stat := pid + " (proc" + pid + ") S 1 1 1 0 -1 4194560 100 200 0 0 50 60 0 0 20 0 1 0 " +
			"13340578 12693504 1530 18446744073709551615 0 0 0 0 0 0 0 0 0 0 0 0 17 2 0 0 0 0 0\n"
		if err := os.WriteFile(filepath.Join(pdir, "stat"), []byte(stat), 0o644); err != nil {
			tb.Fatal(err)
		}
		// One thread entry under task/ so both the old (ReadDir-based) and new
		// (num_threads-based) implementations see the same thread count.
		if err := os.MkdirAll(filepath.Join(pdir, "task", pid), 0o755); err != nil {
			tb.Fatal(err)
		}
	}
	orig := procPath
	procPath = dir
	tb.Cleanup(func() { procPath = orig })
}

// buildProcStat synthesises a /proc/stat with the aggregate cpu line, n per-core
// lines, and the large intr / softirq lines that the parser must skip past.
func buildProcStat(cores int) string {
	var b strings.Builder
	b.WriteString("cpu  123456 789 234567 8901234 5678 0 1234 0 0 0\n")
	for i := 0; i < cores; i++ {
		b.WriteString("cpu")
		b.WriteString(itoa(i))
		b.WriteString(" 3456 12 7890 278123 178 0 56 0 0 0\n")
	}
	// intr line: one aggregate + many per-IRQ counters (~250 numbers).
	b.WriteString("intr 1234567890")
	for i := 0; i < 250; i++ {
		b.WriteString(" ")
		b.WriteString(itoa(i * 37))
	}
	b.WriteString("\n")
	b.WriteString("ctxt 9876543210\n")
	b.WriteString("btime 1700000000\n")
	b.WriteString("processes 123456\n")
	b.WriteString("procs_running 3\n")
	b.WriteString("procs_blocked 0\n")
	// softirq line: aggregate + 10 categories, each large.
	b.WriteString("softirq 5000000000")
	for i := 0; i < 10; i++ {
		b.WriteString(" ")
		b.WriteString(itoa(500000000 + i))
	}
	b.WriteString("\n")
	return b.String()
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}

const realMeminfo = `MemTotal:       16000000 kB
MemFree:         4000000 kB
MemAvailable:    9000000 kB
Buffers:          500000 kB
Cached:          3000000 kB
SwapCached:            0 kB
Active:          6000000 kB
Inactive:        3500000 kB
Active(anon):    4000000 kB
Inactive(anon):   200000 kB
Active(file):    2000000 kB
Inactive(file):  3300000 kB
Unevictable:       12345 kB
Mlocked:           12345 kB
SwapTotal:       4000000 kB
SwapFree:        4000000 kB
Zswap:                 0 kB
Zswapped:              0 kB
Dirty:               456 kB
Writeback:             0 kB
AnonPages:       4200000 kB
Mapped:           800000 kB
Shmem:            150000 kB
KReclaimable:     300000 kB
Slab:             450000 kB
SReclaimable:     300000 kB
SUnreclaim:       150000 kB
KernelStack:       20000 kB
PageTables:        50000 kB
SecPageTables:         0 kB
NFS_Unstable:          0 kB
Bounce:                0 kB
WritebackTmp:          0 kB
CommitLimit:    12000000 kB
Committed_AS:    8000000 kB
VmallocTotal:   34359738367 kB
VmallocUsed:      120000 kB
VmallocChunk:          0 kB
Percpu:            10000 kB
HardwareCorrupted:     0 kB
AnonHugePages:    100000 kB
ShmemHugePages:        0 kB
ShmemPmdMapped:        0 kB
FileHugePages:         0 kB
FilePmdMapped:         0 kB
Unaccepted:            0 kB
HugePages_Total:       0
HugePages_Free:        0
HugePages_Rsvd:        0
HugePages_Surp:        0
Hugepagesize:       2048 kB
Hugetlb:               0 kB
DirectMap4k:      200000 kB
DirectMap2M:    16000000 kB
DirectMap1G:     2097152 kB
`

const realNetDev = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 12345678   23456    0    0    0     0          0         0 12345678   23456    0    0    0     0       0          0
  eth0: 987654321  876543   0    0    0     0          0      1234 123456789  234567    0    0    0     0       0          0
  eth1: 111111111  222222   1    2    0     0          0        55 333333333  444444    0    3    0     0       0          0
  wlan0: 555555555 666666   0    0    0     0          0       777 888888888  999999    0    0    0     0       0          0
 docker0: 100      2        0    0    0     0          0         0 200        4         0    0    0     0       0          0
 br-abc: 300       6        0    0    0     0          0         0 400        8         0    0    0     0       0          0
`

func BenchmarkCollectMemSwap(b *testing.B) {
	buildProcFixtures(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = collectMemory()
		_ = collectSwap()
	}
}

func BenchmarkParseProcStat(b *testing.B) {
	buildProcFixtures(b)
	c := &Collector{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = c.parseProcStat()
	}
}

func BenchmarkParseNetDev(b *testing.B) {
	buildProcFixtures(b)
	c := &Collector{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = c.parseNetDev()
	}
}

func BenchmarkParseDiskStats(b *testing.B) {
	buildProcFixtures(b)
	c := &Collector{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = c.parseDiskStats()
	}
}

func BenchmarkCollectProcesses(b *testing.B) {
	buildProcessTree(b, 300)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = collectProcesses()
	}
}
