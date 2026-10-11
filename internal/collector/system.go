package collector

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
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

// countLoggedInUsers reads /var/run/utmp to count logged-in users.
func countLoggedInUsers() int {
	f, err := os.Open(filepath.Join(varRunPath, "utmp"))
	if err != nil {
		return countUsersFromProc()
	}
	defer func() { _ = f.Close() }()

	count := 0
	// utmp record size on x86_64 Linux is 384 bytes
	const recordSize = 384
	const utTypeOffset = 0
	const utUserOffset = 8
	const userProcess = 7

	buf := make([]byte, recordSize)
	for {
		n, err := f.Read(buf)
		if n < recordSize || err != nil {
			break
		}

		utType := int32(buf[utTypeOffset]) | int32(buf[utTypeOffset+1])<<8 |
			int32(buf[utTypeOffset+2])<<16 | int32(buf[utTypeOffset+3])<<24
		if utType != userProcess {
			continue
		}

		name := strings.TrimRight(string(buf[utUserOffset:utUserOffset+32]), "\x00")
		if name != "" {
			count++
		}
	}
	return count
}

func countUsersFromProc() int {
	seen := make(map[string]bool)

	entries, err := os.ReadDir(procPath)
	if err != nil {
		return 0
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid := entry.Name()
		if _, err := strconv.ParseInt(pid, 10, 64); err != nil {
			continue
		}

		statusPath := filepath.Join(procPath, pid, "status")
		f, err := os.Open(statusPath)
		if err != nil {
			continue
		}

		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "Uid:") {
				fields := strings.Fields(line)
				if len(fields) >= 2 && fields[1] != "0" && !seen[fields[1]] {
					seen[fields[1]] = true
				}
			}
		}
		_ = f.Close()
	}

	return len(seen)
}
