package collector

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func collectSystem() SystemStats {
	s := SystemStats{}

	// Hostname
	s.Hostname, _ = os.Hostname()

	// Uptime
	if data, err := os.ReadFile(filepath.Join(procPath, "uptime")); err == nil {
		fields := strings.Fields(string(data))
		if len(fields) >= 1 {
			s.Uptime = parseFloat(fields[0], 64, "system.uptime")
			s.UptimeHuman = formatUptime(s.Uptime)
		}
	}

	// Entropy
	if data, err := os.ReadFile(filepath.Join(procPath, "sys/kernel/random/entropy_avail")); err == nil {
		s.Entropy, _ = strconv.Atoi(strings.TrimSpace(string(data)))
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
	// Try timedatectl equivalent: check /run/systemd/timesync/synchronized
	if _, err := os.Stat(filepath.Join(runPath, "systemd/timesync/synchronized")); err == nil {
		return true
	}
	// As a fallback, check if chrony or ntp socket exists
	for _, path := range []string{
		"/var/run/chrony/chronyd.pid",
		"/var/run/ntpd.pid",
		"/run/chrony/chronyd.pid",
		"/run/ntpd.pid",
	} {
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	return false
}

// countLoggedInUsers reads /var/run/utmp to count logged-in users.
func countLoggedInUsers() int {
	f, err := os.Open("/var/run/utmp")
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
		if _, err := strconv.Atoi(pid); err != nil {
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
