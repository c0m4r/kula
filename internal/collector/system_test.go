package collector

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"golang.org/x/sys/unix"

	"kula/internal/config"
)

func TestClockSyncedFromTimex(t *testing.T) {
	tests := []struct {
		name string
		tx   unix.Timex
		want bool
	}{
		{"synced", unix.Timex{Status: unix.STA_PLL, Maxerror: 250000}, true},
		// systemd-timesyncd with LocalRTC=yes keeps STA_UNSYNC set while synced.
		{"synced local rtc", unix.Timex{Status: unix.STA_PLL | unix.STA_UNSYNC, Maxerror: 250000}, true},
		{"unsynced at limit", unix.Timex{Status: unix.STA_UNSYNC, Maxerror: ntpMaxErrorLimit}, false},
		{"unsynced no flag", unix.Timex{Maxerror: ntpMaxErrorLimit}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := clockSyncedFromTimex(&tt.tx); got != tt.want {
				t.Errorf("clockSyncedFromTimex() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCollectSystem(t *testing.T) {
	procPath = "testdata/proc"
	sysPath = "testdata/sys"
	runPath = "testdata/run"

	c := New(config.GlobalConfig{}, config.CollectionConfig{}, config.ApplicationsConfig{}, "")
	sys := c.collectSystem()
	if sys.Uptime != 123456.78 {
		t.Errorf("unexpected uptime: %v", sys.Uptime)
	}
	if sys.Entropy != 3200 {
		t.Errorf("unexpected entropy: %v", sys.Entropy)
	}
	if sys.ClockSource != "tsc" {
		t.Errorf("unexpected clock source: %v", sys.ClockSource)
	}
}

func TestCollectProcesses(t *testing.T) {
	procPath = "testdata/proc"

	ps := collectProcesses()
	if ps.Total != 2 {
		t.Errorf("expected 2 processes, got %d", ps.Total)
	}
	if ps.Threads != 2 {
		t.Errorf("expected 2 threads, got %d", ps.Threads)
	}
	if ps.Sleeping != 2 {
		t.Errorf("expected 2 sleeping processes, got %d", ps.Sleeping)
	}
}

func TestCollectSelf(t *testing.T) {
	procPath = "testdata/proc"

	c := New(config.GlobalConfig{}, config.CollectionConfig{}, config.ApplicationsConfig{}, "")
	self := c.collectSelf(1.0)
	if self.FDs != 1 {
		t.Errorf("expected 1 FD, got %d", self.FDs)
	}
	if self.MemRSS != 16000*1024 {
		t.Errorf("expected 16MB RSS, got %d", self.MemRSS)
	}
}

// utmpRecord builds one utmp record of size bytes, as glibc lays it out.
func utmpRecord(size int, utType int16, line, user string) []byte {
	record := make([]byte, size)
	binary.NativeEndian.PutUint16(record[0:], uint16(utType))
	copy(record[8:40], line)
	copy(record[44:76], user)
	return record
}

// hostUtmpRecord builds a utmp record for the architecture under test.
func hostUtmpRecord(utType int16, line, user string) []byte {
	return utmpRecord(utmpRecordSize(runtime.GOARCH), utType, line, user)
}

// TestUtmpRecordSizes: glibc's struct utmp is 400 bytes on aarch64, where
// ut_session and ut_tv are 64-bit, and 384 elsewhere. Reading one layout at
// the other's stride misaligns every record after the first.
func TestUtmpRecordSizes(t *testing.T) {
	for goarch, want := range map[string]int{"amd64": 384, "386": 384, "arm": 384, "riscv64": 384, "arm64": 400} {
		if got := utmpRecordSize(goarch); got != want {
			t.Errorf("utmpRecordSize(%s) = %d, want %d", goarch, got, want)
		}
	}
	for _, size := range []int{384, 400} {
		var utmp []byte
		utmp = append(utmp, utmpRecord(size, 2, "~", "reboot")...) // BOOT_TIME
		for _, user := range []string{"alice", "bob", "carol"} {
			utmp = append(utmp, utmpRecord(size, 7, "pts/0", user)...)
		}
		path := filepath.Join(t.TempDir(), "utmp")
		if err := os.WriteFile(path, utmp, 0o644); err != nil {
			t.Fatal(err)
		}
		if got, ok := countUtmpUsers(path, size); !ok || got != 3 {
			t.Errorf("%d-byte records: %d users (ok %v), want 3", size, got, ok)
		}
		other := 784 - size
		if got, _ := countUtmpUsers(path, other); got == 3 {
			t.Errorf("%d-byte records read at a %d-byte stride still counted 3: the fixture does not tell the layouts apart", size, other)
		}
	}
}

func TestLoggedInUsersFromUtmpLogindOrUnknown(t *testing.T) {
	oldRun, oldVarRun := runPath, varRunPath
	t.Cleanup(func() { runPath, varRunPath = oldRun, oldVarRun })
	runPath, varRunPath = t.TempDir(), t.TempDir()

	// Neither source: unknown, not zero.
	if got := countLoggedInUsers(); got != -1 {
		t.Fatalf("no utmp and no logind: %d, want -1 (unknown)", got)
	}

	// logind: only open sessions of class "user" are logins.
	sessions := filepath.Join(runPath, "systemd", "sessions")
	if err := os.MkdirAll(sessions, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"1":  "USER=alice\nSTATE=active\nCLASS=user\nTYPE=tty\n",
		"2":  "USER=bob\nSTATE=online\nCLASS=user\nREMOTE=1\n",
		"3":  "USER=alice\nSTATE=active\nCLASS=manager\n",
		"4":  "USER=carol\nSTATE=closing\nCLASS=user\n",
		"c1": "USER=gdm\nSTATE=active\nCLASS=greeter\n",
	} {
		if err := os.WriteFile(filepath.Join(sessions, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := countLoggedInUsers(); got != 2 {
		t.Fatalf("logind sessions: %d, want 2", got)
	}

	// An empty utmp (systemd built without utmp support still creating the
	// file) records no login: logind decides.
	if err := os.WriteFile(filepath.Join(runPath, "utmp"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := countLoggedInUsers(); got != 2 {
		t.Fatalf("empty utmp beside logind: %d, want logind's 2", got)
	}
	if err := os.Remove(filepath.Join(runPath, "utmp")); err != nil {
		t.Fatal(err)
	}

	// utmp wins when present: USER_PROCESS records with a user name.
	var utmp []byte
	utmp = append(utmp, hostUtmpRecord(2, "~", "reboot")...) // BOOT_TIME
	utmp = append(utmp, hostUtmpRecord(7, "tty1", "alice")...)
	utmp = append(utmp, hostUtmpRecord(7, "pts/0", "bob")...)
	utmp = append(utmp, hostUtmpRecord(6, "tty2", "LOGIN")...) // LOGIN_PROCESS
	utmp = append(utmp, hostUtmpRecord(7, "pts/1", "")...)
	if err := os.WriteFile(filepath.Join(varRunPath, "utmp"), utmp, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := countLoggedInUsers(); got != 2 {
		t.Fatalf("utmp: %d, want 2", got)
	}

	// utmp that records logins wins over logind, as for `who`.
	if err := os.WriteFile(filepath.Join(varRunPath, "utmp"), hostUtmpRecord(7, "tty1", "alice"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := countLoggedInUsers(); got != 1 {
		t.Fatalf("utmp with one login beside two logind sessions: %d, want 1", got)
	}

	// No logind and an empty utmp: nobody is logged in, a known zero.
	if err := os.RemoveAll(sessions); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(varRunPath, "utmp"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := countLoggedInUsers(); got != 0 {
		t.Fatalf("empty utmp without logind: %d, want 0", got)
	}
}
