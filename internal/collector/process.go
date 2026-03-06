package collector

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}

		ps.Total++

		// Read /proc/[pid]/stat for state
		statPath := filepath.Join(procPath, entry.Name(), "stat")
		data, err := os.ReadFile(statPath)
		if err != nil {
			continue
		}

		// Find state: it's the character after the last ')' in the stat line
		content := string(data)
		idx := strings.LastIndex(content, ")")
		if idx < 0 || idx+2 >= len(content) {
			continue
		}
		state := string(content[idx+2])

		switch state {
		case "R":
			ps.Running++
		case "S":
			ps.Sleeping++
		case "D":
			ps.Blocked++
		case "Z":
			ps.Zombie++
		}

		// Count threads from /proc/[pid]/task
		taskDir := filepath.Join(procPath, entry.Name(), "task")
		if tasks, err := os.ReadDir(taskDir); err == nil {
			ps.Threads += len(tasks)
		}
	}

	return ps
}
