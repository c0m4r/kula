package main

import (
	"bufio"
	"os"
	"strings"
)

func getOSName() string {
	file, err := os.Open("/etc/os-release")
	if err != nil {
		return "Unknown OS"
	}
	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "PRETTY_NAME=") {
			return strings.Trim(strings.TrimPrefix(line, "PRETTY_NAME="), `"'`)
		}
	}
	return "Unknown OS"
}

func getKernelVersion() string {
	data, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return "Unknown Kernel"
	}
	return strings.TrimSpace(string(data))
}
