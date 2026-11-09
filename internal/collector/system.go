package collector

import (
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/sys/unix"
)

func (c *Collector) collectSystem() SystemStats {
	s := SystemStats{}

	// Hostname
	if c.cfg.Hostname != "" {
		s.Hostname = c.cfg.Hostname
	} else {
		s.Hostname, _ = os.Hostname()
	}

	// Uptime
	if data, err := os.ReadFile(filepath.Join(procPath, "uptime")); err == nil {
		fields := strings.Fields(string(data))
		if len(fields) >= 1 {
			s.Uptime = c.parseFloat(fields[0], 64, "system.uptime")
			s.UptimeHuman = FormatUptime(s.Uptime)
		}
	}

	// Entropy
	if data, err := os.ReadFile(filepath.Join(procPath, "sys/kernel/random/entropy_avail")); err == nil {
		s.Entropy = int(c.parseInt(strings.TrimSpace(string(data)), 10, 32, "system.entropy"))
	}

	// Clock source
	if data, err := os.ReadFile(filepath.Join(sysPath, "devices/system/clocksource/clocksource0/current_clocksource")); err == nil {
		s.ClockSource = strings.TrimSpace(string(data))
	}

	// Clock sync - check via /sys/class/ptp or adjtimex status
	s.ClockSync = checkClockSync()

	// User count only (no full user struct array)
	s.UserCount = countLoggedInUsers()

	return s
}

func checkClockSync() bool {
	// 1. Try kernel adjtimex syscall - most robust, works in containers
	var tx unix.Timex
	if _, err := unix.Adjtimex(&tx); err == nil {
		return clockSyncedFromTimex(&tx)
	}

	// 2. Fallback: check for systemd-timesyncd state file
	if _, err := os.Stat(filepath.Join(runPath, "systemd/timesync/synchronized")); err == nil {
		return true
	}

	// 3. Last resort: check if common NTP daemon PIDs exist
	for _, path := range []string{
		filepath.Join(varRunPath, "chrony/chronyd.pid"),
		filepath.Join(varRunPath, "ntpd.pid"),
		filepath.Join(runPath, "chrony/chronyd.pid"),
		filepath.Join(runPath, "ntpd.pid"),
	} {
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	return false
}

// ntpMaxErrorLimit is the kernel's NTP_PHASE_LIMIT (16 s, in µs). The kernel
// resets maxerror to this value when unsynchronized, and NTP daemons (chrony,
// ntpd, systemd-timesyncd) report it when they lose sync.
const ntpMaxErrorLimit = 16000000

// clockSyncedFromTimex mirrors systemd's ntp_synced() (used by timedatectl):
// the clock is synchronized when maxerror is below the 16 s limit. STA_UNSYNC
// (and thus the TIME_ERROR return value) is deliberately ignored, because
// systemd-timesyncd keeps it set when the RTC runs in local time (LocalRTC=yes)
// to stop the kernel's 11-minute mode from writing to the RTC.
func clockSyncedFromTimex(tx *unix.Timex) bool {
	return tx.Maxerror < ntpMaxErrorLimit
}

// countLoggedInUsers counts login sessions the way `who` does, from the
// USER_PROCESS entries in utmp. Where utmp is missing (distributions that
// dropped it, most containers) or records no login, it counts systemd-logind
// user sessions instead: systemd built without utmp support can leave an empty
// /run/utmp behind, which would otherwise always read as nobody logged in. It
// returns -1 when neither source exists: the count is unknown, which is not
// the same as nobody being logged in.
func countLoggedInUsers() int {
	utmpUsers, haveUtmp := -1, false
	for _, path := range []string{filepath.Join(runPath, "utmp"), filepath.Join(varRunPath, "utmp")} {
		if n, ok := countUtmpUsers(path, utmpRecordSize(runtime.GOARCH)); ok {
			utmpUsers, haveUtmp = n, true
			break
		}
	}
	if haveUtmp && utmpUsers > 0 {
		return utmpUsers
	}
	if n, ok := countLogindSessions(filepath.Join(runPath, "systemd", "sessions")); ok {
		return n
	}
	return utmpUsers
}

// utmpRecordSize is glibc's sizeof(struct utmp) on goarch. Where glibc sets
// __WORDSIZE_TIME64_COMPAT32 (x86, riscv, powerpc64, mips, arm), ut_session
// and ut_tv stay 32-bit so 32- and 64-bit programs can share the file: 384
// bytes. aarch64 clears it, making them a long and a struct timeval: 400. The
// fields read here sit at the same offsets in both; check bits/wordsize.h
// before adding an architecture.
func utmpRecordSize(goarch string) int {
	if goarch == "arm64" {
		return 400
	}
	return 384
}

// countUtmpUsers counts USER_PROCESS records with a user name in a utmp file
// of recordSize-byte records.
func countUtmpUsers(path string, recordSize int) (int, bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer func() { _ = f.Close() }()

	count := 0
	// ut_type is a short in the host's byte order. ut_user follows ut_type
	// (2, padded to 4), ut_pid (4), ut_line (32) and ut_id (4).
	const utUserOffset = 44
	const userProcess = 7

	buf := make([]byte, recordSize)
	for {
		if _, err := io.ReadFull(f, buf); err != nil {
			break
		}
		if int16(binary.NativeEndian.Uint16(buf[0:2])) != userProcess {
			continue
		}

		name := strings.TrimRight(string(buf[utUserOffset:utUserOffset+32]), "\x00")
		if name != "" {
			count++
		}
	}
	return count, true
}

// countLogindSessions counts the open user sessions systemd-logind records in
// dir, one file per session. Sessions of class "user" are logins (tty, SSH,
// desktop); "manager" (the per-user service manager), "background" and greeter
// sessions are not, and a session in state "closing" has logged out.
func countLogindSessions(dir string) (int, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, false
	}
	count := 0
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		var class, state string
		for _, line := range strings.Split(string(data), "\n") {
			if v, ok := strings.CutPrefix(line, "CLASS="); ok {
				class = v
			} else if v, ok := strings.CutPrefix(line, "STATE="); ok {
				state = v
			}
		}
		if class == "user" && state != "closing" {
			count++
		}
	}
	return count, true
}
