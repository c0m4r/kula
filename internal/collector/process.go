package collector

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
)

func collectProcesses() ProcessStats {
	ps := ProcessStats{}

	entries, err := os.ReadDir(procPath)
	if err != nil {
		return ps
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		// Only numeric directories (PIDs)
		if _, err := strconv.ParseInt(entry.Name(), 10, 64); err != nil {
			continue
		}

		ps.Total++

		// Read /proc/[pid]/stat once. It carries both the process state and the
		// thread count (num_threads, field 20), so the per-process os.ReadDir on
		// /proc/[pid]/task — a second syscall plus a DirEntry slice allocation for
		// every process — is no longer needed. On hosts running thousands of
		// processes this roughly halves the syscalls of the heaviest collector.
		data, err := os.ReadFile(filepath.Join(procPath, entry.Name(), "stat"))
		if err != nil {
			continue
		}

		// The comm field (field 2) may contain spaces and parentheses, so the
		// fields we need follow the final ')': field 3 = state, field 20 =
		// num_threads, i.e. tokens 0 and 17 of the remainder.
		idx := bytes.LastIndexByte(data, ')')
		if idx < 0 || idx+2 >= len(data) {
			continue
		}
		rest := data[idx+2:]

		fieldIdx := 0
		pos := 0
		for pos < len(rest) {
			for pos < len(rest) && rest[pos] == ' ' {
				pos++
			}
			if pos >= len(rest) {
				break
			}
			start := pos
			for pos < len(rest) && rest[pos] != ' ' && rest[pos] != '\n' {
				pos++
			}
			field := rest[start:pos]

			switch fieldIdx {
			case 0:
				switch field[0] {
				case 'R':
					ps.Running++
				case 'S':
					ps.Sleeping++
				case 'D':
					ps.Blocked++
				case 'Z':
					ps.Zombie++
				}
			case 17:
				ps.Threads += int(parseUintBytes(field))
			}
			if fieldIdx == 17 {
				break // state and thread count captured; skip the rest of the line
			}
			fieldIdx++
		}
	}

	return ps
}
